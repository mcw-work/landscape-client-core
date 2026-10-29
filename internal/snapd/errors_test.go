package snapd_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/canonical/landscape-client-core/internal/snapd"
)

func errorResponse(status int, message string) []byte {
	data, _ := json.Marshal(map[string]any{
		"type":        "error",
		"status-code": status,
		"result":      map[string]string{"message": message},
	})
	return data
}

func rawHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
}

// unreachableClient returns a client whose socket does not exist.
func unreachableClient(t *testing.T) snapd.Client {
	t.Helper()
	return snapd.New(filepath.Join(t.TempDir(), "missing.socket"))
}

// recordedRequest is what the fake snapd saw.
type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Body        map[string]any
}

// recordingServer answers every request with reply and records the requests.
func recordingServer(t *testing.T, reply []byte) (snapd.Client, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
		}
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &rec.Body); err != nil {
				t.Errorf("request body %q is not JSON: %v", data, err)
			}
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)
	})
	_, socketPath := startUnixServer(t, handler)
	return snapd.New(socketPath), func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
}

func TestPostRequests(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		call     func(snapd.Client) (string, error)
		wantPath string
		wantBody map[string]any
	}{{
		name: "install with channel",
		call: func(c snapd.Client) (string, error) {
			return c.InstallSnap(ctx, "hello", snapd.InstallOptions{Channel: "edge"})
		},
		wantPath: "/v2/snaps/hello",
		wantBody: map[string]any{"action": "install", "channel": "edge"},
	}, {
		name: "install classic without channel",
		call: func(c snapd.Client) (string, error) {
			return c.InstallSnap(ctx, "code", snapd.InstallOptions{Classic: true})
		},
		wantPath: "/v2/snaps/code",
		wantBody: map[string]any{"action": "install", "classic": true},
	}, {
		name:     "remove",
		call:     func(c snapd.Client) (string, error) { return c.RemoveSnap(ctx, "hello") },
		wantPath: "/v2/snaps/hello",
		wantBody: map[string]any{"action": "remove"},
	}, {
		name:     "refresh",
		call:     func(c snapd.Client) (string, error) { return c.RefreshSnap(ctx, "hello") },
		wantPath: "/v2/snaps/hello",
		wantBody: map[string]any{"action": "refresh"},
	}, {
		name:     "stop service",
		call:     func(c snapd.Client) (string, error) { return "", c.StopService(ctx, "lxd", "daemon") },
		wantPath: "/v2/apps",
		wantBody: map[string]any{"action": "stop", "names": []any{"lxd.daemon"}},
	}, {
		name:     "restart service",
		call:     func(c snapd.Client) (string, error) { return "", c.RestartService(ctx, "lxd", "daemon") },
		wantPath: "/v2/apps",
		wantBody: map[string]any{"action": "restart", "names": []any{"lxd.daemon"}},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := recordingServer(t, asyncResponse("77"))
			changeID, err := tc.call(client)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.HasPrefix(tc.wantPath, "/v2/snaps/") && changeID != "77" {
				t.Errorf("changeID: got %q, want 77", changeID)
			}
			reqs := requests()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			got := reqs[0]
			if got.Method != http.MethodPost || got.Path != tc.wantPath {
				t.Errorf("request: got %s %s, want POST %s", got.Method, got.Path, tc.wantPath)
			}
			if got.ContentType != "application/json" {
				t.Errorf("Content-Type: got %q, want application/json", got.ContentType)
			}
			if !reflect.DeepEqual(got.Body, tc.wantBody) {
				t.Errorf("body: got %v, want %v", got.Body, tc.wantBody)
			}
		})
	}
}

func TestGetRequests(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name      string
		call      func(snapd.Client) error
		wantPath  string
		wantQuery string
	}{{
		name:     "list snaps",
		call:     func(c snapd.Client) error { _, err := c.ListSnaps(ctx); return err },
		wantPath: "/v2/snaps",
	}, {
		name:      "list services",
		call:      func(c snapd.Client) error { _, err := c.ListServices(ctx); return err },
		wantPath:  "/v2/apps",
		wantQuery: "select=service",
	}, {
		name:     "system info",
		call:     func(c snapd.Client) error { _, err := c.GetRebootRequired(ctx); return err },
		wantPath: "/v2/system-info",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, requests := recordingServer(t, syncResponse(nil))
			if err := tc.call(client); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			reqs := requests()
			if len(reqs) != 1 {
				t.Fatalf("got %d requests, want 1", len(reqs))
			}
			if got := reqs[0]; got.Method != http.MethodGet || got.Path != tc.wantPath || got.Query != tc.wantQuery {
				t.Errorf("request: got %s %s?%s, want GET %s?%s", got.Method, got.Path, got.Query, tc.wantPath, tc.wantQuery)
			}
		})
	}
}

// clientCall runs one Client method and returns only its error.
type clientCall struct {
	name string
	call func(context.Context, snapd.Client) error
}

var allCalls = []clientCall{
	{"ListSnaps", func(ctx context.Context, c snapd.Client) error { _, err := c.ListSnaps(ctx); return err }},
	{"ListServices", func(ctx context.Context, c snapd.Client) error { _, err := c.ListServices(ctx); return err }},
	{"InstallSnap", func(ctx context.Context, c snapd.Client) error {
		_, err := c.InstallSnap(ctx, "s", snapd.InstallOptions{})
		return err
	}},
	{"RemoveSnap", func(ctx context.Context, c snapd.Client) error { _, err := c.RemoveSnap(ctx, "s"); return err }},
	{"RefreshSnap", func(ctx context.Context, c snapd.Client) error { _, err := c.RefreshSnap(ctx, "s"); return err }},
	{"StartService", func(ctx context.Context, c snapd.Client) error { return c.StartService(ctx, "s", "svc") }},
	{"StopService", func(ctx context.Context, c snapd.Client) error { return c.StopService(ctx, "s", "svc") }},
	{"RestartService", func(ctx context.Context, c snapd.Client) error { return c.RestartService(ctx, "s", "svc") }},
	{"WaitForChange", func(ctx context.Context, c snapd.Client) error { return c.WaitForChange(ctx, "1") }},
	{"GetRebootRequired", func(ctx context.Context, c snapd.Client) error { _, err := c.GetRebootRequired(ctx); return err }},
}

func TestTransportErrors(t *testing.T) {
	client := unreachableClient(t)
	for _, tc := range allCalls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background(), client)
			if err == nil || !strings.Contains(err.Error(), "missing.socket") {
				t.Errorf("got %v, want dial error naming the socket", err)
			}
		})
	}
	t.Run("GetAssertions", func(t *testing.T) {
		_, err := client.GetAssertions(context.Background())
		if err == nil || !strings.HasPrefix(err.Error(), "snapd: cannot fetch serial assertion: ") {
			t.Errorf("got %v, want wrapped serial fetch error", err)
		}
	})
}

func TestSnapdErrorResponse(t *testing.T) {
	_, socketPath := startUnixServer(t, rawHandler(string(errorResponse(404, `snap "s" is not installed`))))
	client := snapd.New(socketPath)
	for _, tc := range allCalls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background(), client)
			want := `snapd: error 404: snap "s" is not installed`
			if err == nil || err.Error() != want {
				t.Errorf("got %v, want %q", err, want)
			}
		})
	}
}

func TestErrorResponseWithoutMessage(t *testing.T) {
	_, socketPath := startUnixServer(t, rawHandler(`{"type":"error","status-code":500,"result":"oops"}`))
	_, err := snapd.New(socketPath).ListSnaps(context.Background())
	if err == nil || err.Error() != "snapd: error 500: " {
		t.Errorf("got %v, want %q", err, "snapd: error 500: ")
	}
}

func TestUndecodableResponse(t *testing.T) {
	_, socketPath := startUnixServer(t, rawHandler("<html>not json</html>"))
	client := snapd.New(socketPath)
	for _, tc := range allCalls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background(), client)
			if err == nil || !strings.HasPrefix(err.Error(), "snapd: cannot decode response: ") {
				t.Errorf("got %v, want decode error", err)
			}
		})
	}
}

func TestMalformedResult(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		result  any
		call    func(snapd.Client) error
		wantErr string
	}{{
		name:    "snaps not a list",
		result:  map[string]string{"name": "x"},
		call:    func(c snapd.Client) error { _, err := c.ListSnaps(ctx); return err },
		wantErr: "snapd: cannot parse snaps: ",
	}, {
		name:    "services not a list",
		result:  "nope",
		call:    func(c snapd.Client) error { _, err := c.ListServices(ctx); return err },
		wantErr: "snapd: cannot parse services: ",
	}, {
		name:    "change not an object",
		result:  []int{1},
		call:    func(c snapd.Client) error { return c.WaitForChange(ctx, "5") },
		wantErr: "snapd: cannot parse change: ",
	}, {
		name:    "system-info not an object",
		result:  42,
		call:    func(c snapd.Client) error { _, err := c.GetRebootRequired(ctx); return err },
		wantErr: "snapd: cannot parse system-info: ",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, socketPath := startUnixServer(t, rawHandler(string(syncResponse(tc.result))))
			err := tc.call(snapd.New(socketPath))
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Errorf("got %v, want prefix %q", err, tc.wantErr)
			}
		})
	}
}

func TestInvalidNameInURL(t *testing.T) {
	client, requests := recordingServer(t, asyncResponse("1"))
	ctx := context.Background()
	if _, err := client.InstallSnap(ctx, "%zz", snapd.InstallOptions{}); err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Errorf("InstallSnap: got %v, want invalid URL escape", err)
	}
	if err := client.WaitForChange(ctx, "%zz"); err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
		t.Errorf("WaitForChange: got %v, want invalid URL escape", err)
	}
	if n := len(requests()); n != 0 {
		t.Errorf("snapd received %d requests, want 0", n)
	}
}

func TestGetRebootRequired_OtherPendingValue(t *testing.T) {
	_, socketPath := startUnixServer(t, rawHandler(string(syncResponse(map[string]any{
		"refresh": map[string]string{"pending": "ready"},
	}))))
	got, err := snapd.New(socketPath).GetRebootRequired(context.Background())
	if err != nil || got {
		t.Errorf("got (%v, %v), want (false, nil)", got, err)
	}
}

const (
	serialAssertion = "type: serial\nserial: abc-123\nno colon line\n: empty key\n\nbody: ignored\n"
	modelAssertion  = "type: model\nmodel: pc\nbrand-id: acme\n"
)

// assertionReply describes how the fake snapd answers one assertion endpoint.
type assertionReply struct {
	status int
	body   string
	hangUp bool
}

func assertionServer(t *testing.T, serial, model assertionReply) snapd.Client {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reply assertionReply
		switch r.URL.Path {
		case "/v2/assertions/serial":
			reply = serial
		case "/v2/assertions/model":
			reply = model
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s for %s", r.Method, r.URL.Path)
		}
		if reply.hangUp {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/x.ubuntu.assertion")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	})
	_, socketPath := startUnixServer(t, handler)
	return snapd.New(socketPath)
}

func TestGetAssertionsBranches(t *testing.T) {
	longLine := "serial: " + strings.Repeat("x", 70*1024) + "\n"
	ok := func(body string) assertionReply { return assertionReply{status: http.StatusOK, body: body} }
	notFound := assertionReply{status: http.StatusNotFound}

	tests := []struct {
		name    string
		serial  assertionReply
		model   assertionReply
		want    snapd.Assertions
		wantErr string
	}{{
		name:   "both present",
		serial: ok(serialAssertion),
		model:  ok(modelAssertion),
		want:   snapd.Assertions{Serial: "abc-123", Model: "pc", Brand: "acme"},
	}, {
		name:   "serial missing",
		serial: notFound,
		model:  ok(modelAssertion),
		want:   snapd.Assertions{Model: "pc", Brand: "acme"},
	}, {
		name:   "model missing",
		serial: ok(serialAssertion),
		model:  notFound,
		want:   snapd.Assertions{Serial: "abc-123"},
	}, {
		name:   "neither present",
		serial: notFound,
		model:  notFound,
	}, {
		name:    "serial server error",
		serial:  assertionReply{status: http.StatusInternalServerError},
		model:   ok(modelAssertion),
		wantErr: "snapd: cannot fetch serial assertion: snapd: assertion request failed with status 500",
	}, {
		name:    "serial redirect status",
		serial:  assertionReply{status: http.StatusNotModified},
		model:   ok(modelAssertion),
		wantErr: "snapd: cannot fetch serial assertion: snapd: assertion request failed with status 304",
	}, {
		name:    "serial header too long",
		serial:  ok(longLine),
		model:   ok(modelAssertion),
		wantErr: "snapd: cannot fetch serial assertion: bufio.Scanner: token too long",
	}, {
		name:    "model server error",
		serial:  ok(serialAssertion),
		model:   assertionReply{status: http.StatusForbidden},
		wantErr: "snapd: cannot fetch model assertion: snapd: model assertion request failed with status 403",
	}, {
		name:    "model header too long",
		serial:  notFound,
		model:   ok(longLine),
		wantErr: "snapd: cannot fetch model assertion: bufio.Scanner: token too long",
	}, {
		name:    "model connection dropped",
		serial:  notFound,
		model:   assertionReply{hangUp: true},
		wantErr: "snapd: cannot fetch model assertion: ",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := assertionServer(t, tc.serial, tc.model)
			got, err := client.GetAssertions(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want prefix %q", err, tc.wantErr)
				}
				if got != nil {
					t.Errorf("got %+v alongside error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if *got != tc.want {
				t.Errorf("got %+v, want %+v", *got, tc.want)
			}
		})
	}
}

func TestMockClientBehaviour(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("injected")
	changeErr := errors.New("change failed")
	snaps := []snapd.SnapInfo{{Name: "core"}}
	services := []snapd.ServiceInfo{{Snap: "lxd", Name: "daemon"}}
	assertions := &snapd.Assertions{Serial: "s1", Model: "m", Brand: "b"}

	type outcome struct {
		value any
		err   error
	}
	tests := []struct {
		name  string
		call  func(*snapd.MockClient) outcome
		want  outcome
		calls func(*snapd.MockClient) []string
	}{{
		name: "ListSnaps",
		call: func(m *snapd.MockClient) outcome { v, err := m.ListSnaps(ctx); return outcome{v, err} },
		want: outcome{value: snaps},
	}, {
		name: "ListServices",
		call: func(m *snapd.MockClient) outcome { v, err := m.ListServices(ctx); return outcome{v, err} },
		want: outcome{value: services},
	}, {
		name: "InstallSnap",
		call: func(m *snapd.MockClient) outcome {
			v, err := m.InstallSnap(ctx, "hello", snapd.InstallOptions{})
			return outcome{v, err}
		},
		want:  outcome{value: "c1"},
		calls: func(m *snapd.MockClient) []string { return m.InstallCalls },
	}, {
		name:  "RemoveSnap",
		call:  func(m *snapd.MockClient) outcome { v, err := m.RemoveSnap(ctx, "hello"); return outcome{v, err} },
		want:  outcome{value: "c1"},
		calls: func(m *snapd.MockClient) []string { return m.RemoveCalls },
	}, {
		name:  "RefreshSnap",
		call:  func(m *snapd.MockClient) outcome { v, err := m.RefreshSnap(ctx, "hello"); return outcome{v, err} },
		want:  outcome{value: "c1"},
		calls: func(m *snapd.MockClient) []string { return m.RefreshCalls },
	}, {
		name:  "StartService",
		call:  func(m *snapd.MockClient) outcome { return outcome{nil, m.StartService(ctx, "lxd", "daemon")} },
		calls: func(m *snapd.MockClient) []string { return m.ServiceActions },
	}, {
		name:  "StopService",
		call:  func(m *snapd.MockClient) outcome { return outcome{nil, m.StopService(ctx, "lxd", "daemon")} },
		calls: func(m *snapd.MockClient) []string { return m.ServiceActions },
	}, {
		name:  "RestartService",
		call:  func(m *snapd.MockClient) outcome { return outcome{nil, m.RestartService(ctx, "lxd", "daemon")} },
		calls: func(m *snapd.MockClient) []string { return m.ServiceActions },
	}, {
		name: "GetAssertions",
		call: func(m *snapd.MockClient) outcome { v, err := m.GetAssertions(ctx); return outcome{v, err} },
		want: outcome{value: assertions},
	}, {
		name: "GetRebootRequired",
		call: func(m *snapd.MockClient) outcome { v, err := m.GetRebootRequired(ctx); return outcome{v, err} },
		want: outcome{value: true},
	}}

	wantRecorded := map[string][]string{
		"InstallSnap":    {"hello"},
		"RemoveSnap":     {"hello"},
		"RefreshSnap":    {"hello"},
		"StartService":   {"start:lxd.daemon"},
		"StopService":    {"stop:lxd.daemon"},
		"RestartService": {"restart:lxd.daemon"},
	}

	for _, tc := range tests {
		t.Run(tc.name+"/success", func(t *testing.T) {
			m := &snapd.MockClient{
				Snaps: snaps, Services: services, ChangeID: "c1",
				AssertionsData: assertions, RebootRequired: true,
			}
			got := tc.call(m)
			if got.err != nil {
				t.Fatalf("unexpected error: %v", got.err)
			}
			if tc.want.value != nil && !reflect.DeepEqual(got.value, tc.want.value) {
				t.Errorf("value: got %#v, want %#v", got.value, tc.want.value)
			}
			if tc.calls != nil {
				if rec := tc.calls(m); !reflect.DeepEqual(rec, wantRecorded[tc.name]) {
					t.Errorf("recorded calls: got %v, want %v", rec, wantRecorded[tc.name])
				}
			}
		})
		t.Run(tc.name+"/injected error", func(t *testing.T) {
			m := &snapd.MockClient{ChangeID: "c1", RebootRequired: true, Err: sentinel}
			got := tc.call(m)
			if !errors.Is(got.err, sentinel) {
				t.Errorf("error: got %v, want injected sentinel", got.err)
			}
			if v := reflect.ValueOf(got.value); got.value != nil && !v.IsZero() {
				t.Errorf("value: got %#v, want zero value on error", got.value)
			}
			if tc.calls != nil && len(tc.calls(m)) != 0 {
				t.Errorf("failed call was recorded: %v", tc.calls(m))
			}
		})
	}

	t.Run("WaitForChange", func(t *testing.T) {
		if err := (&snapd.MockClient{Err: sentinel}).WaitForChange(ctx, "1"); err != nil {
			t.Errorf("Err must not affect WaitForChange, got %v", err)
		}
		if err := (&snapd.MockClient{ChangeErr: changeErr}).WaitForChange(ctx, "1"); !errors.Is(err, changeErr) {
			t.Errorf("got %v, want ChangeErr", err)
		}
	})

	t.Run("concurrent recording", func(t *testing.T) {
		m := &snapd.MockClient{}
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = m.InstallSnap(ctx, fmt.Sprint(i), snapd.InstallOptions{})
				_ = m.StartService(ctx, "s", fmt.Sprint(i))
			}()
		}
		wg.Wait()
		if len(m.InstallCalls) != 20 || len(m.ServiceActions) != 20 {
			t.Errorf("got %d installs and %d service actions, want 20 each", len(m.InstallCalls), len(m.ServiceActions))
		}
	})
}
