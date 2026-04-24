package snapd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// SnapInfo describes an installed snap.
type SnapInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Revision  int    `json:"revision"`
	Channel   string `json:"channel"`
	Status    string `json:"status"`
	Developer string `json:"developer"`
}

// ServiceInfo describes a snap service.
type ServiceInfo struct {
	Snap    string `json:"snap"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Active  bool   `json:"active"`
}

// InstallOptions controls snap installation.
type InstallOptions struct {
	Channel string `json:"channel,omitempty"`
	Classic bool   `json:"classic,omitempty"`
}

// Assertions holds device identity assertions from snapd.
type Assertions struct {
	Serial string
	Model  string
	Brand  string
}

// Client is the interface implemented by both the real snapd client and MockClient.
type Client interface {
	ListSnaps(ctx context.Context) ([]SnapInfo, error)
	ListServices(ctx context.Context) ([]ServiceInfo, error)
	InstallSnap(ctx context.Context, name string, opts InstallOptions) (changeID string, err error)
	RemoveSnap(ctx context.Context, name string) (changeID string, err error)
	RefreshSnap(ctx context.Context, name string) (changeID string, err error)
	StartService(ctx context.Context, snapName, serviceName string) error
	StopService(ctx context.Context, snapName, serviceName string) error
	RestartService(ctx context.Context, snapName, serviceName string) error
	WaitForChange(ctx context.Context, changeID string) error
	GetAssertions(ctx context.Context) (*Assertions, error)
	GetRebootRequired(ctx context.Context) (bool, error)
}

// snapdResponse is the envelope for all snapd REST responses.
type snapdResponse struct {
	Type       string          `json:"type"`
	StatusCode int             `json:"status-code"`
	Result     json.RawMessage `json:"result"`
	Change     string          `json:"change"`
}

// snapdError is returned by snapd in error responses.
type snapdError struct {
	Message string `json:"message"`
}

// RealClient is a Client connected to the snapd Unix socket.
type RealClient struct {
	http    *http.Client
	baseURL string
}

// New returns a Client connected to the given Unix socket path.
// Use "/run/snapd.socket" in production.
func New(socketPath string) Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &RealClient{
		http:    &http.Client{Transport: transport},
		baseURL: "http://localhost/v2",
	}
}

func (c *RealClient) get(ctx context.Context, path string) (*snapdResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeResponse(resp.Body)
}

func (c *RealClient) post(ctx context.Context, path string, body any) (*snapdResponse, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return decodeResponse(resp.Body)
}

func decodeResponse(r io.Reader) (*snapdResponse, error) {
	var sr snapdResponse
	if err := json.NewDecoder(r).Decode(&sr); err != nil {
		return nil, fmt.Errorf("snapd: decoding response: %w", err)
	}
	if sr.Type == "error" {
		var e snapdError
		_ = json.Unmarshal(sr.Result, &e)
		return nil, fmt.Errorf("snapd: error %d: %s", sr.StatusCode, e.Message)
	}
	return &sr, nil
}

// ListSnaps returns all installed snaps.
func (c *RealClient) ListSnaps(ctx context.Context) ([]SnapInfo, error) {
	sr, err := c.get(ctx, "/snaps")
	if err != nil {
		return nil, err
	}
	var snaps []SnapInfo
	if err := json.Unmarshal(sr.Result, &snaps); err != nil {
		return nil, fmt.Errorf("snapd: parsing snaps: %w", err)
	}
	return snaps, nil
}

// ListServices returns all snap services.
func (c *RealClient) ListServices(ctx context.Context) ([]ServiceInfo, error) {
	sr, err := c.get(ctx, "/apps?select=service")
	if err != nil {
		return nil, err
	}
	var services []ServiceInfo
	if err := json.Unmarshal(sr.Result, &services); err != nil {
		return nil, fmt.Errorf("snapd: parsing services: %w", err)
	}
	return services, nil
}

// InstallSnap installs a snap asynchronously and returns the change ID.
func (c *RealClient) InstallSnap(ctx context.Context, name string, opts InstallOptions) (string, error) {
	body := map[string]any{"action": "install"}
	if opts.Channel != "" {
		body["channel"] = opts.Channel
	}
	if opts.Classic {
		body["classic"] = true
	}
	sr, err := c.post(ctx, "/snaps/"+name, body)
	if err != nil {
		return "", err
	}
	return sr.Change, nil
}

// RemoveSnap removes a snap asynchronously and returns the change ID.
func (c *RealClient) RemoveSnap(ctx context.Context, name string) (string, error) {
	sr, err := c.post(ctx, "/snaps/"+name, map[string]string{"action": "remove"})
	if err != nil {
		return "", err
	}
	return sr.Change, nil
}

// RefreshSnap refreshes a snap asynchronously and returns the change ID.
func (c *RealClient) RefreshSnap(ctx context.Context, name string) (string, error) {
	sr, err := c.post(ctx, "/snaps/"+name, map[string]string{"action": "refresh"})
	if err != nil {
		return "", err
	}
	return sr.Change, nil
}

// serviceAction sends a start/stop/restart action for a snap service.
func (c *RealClient) serviceAction(ctx context.Context, action, snapName, serviceName string) error {
	fullName := snapName + "." + serviceName
	_, err := c.post(ctx, "/apps", map[string]any{
		"action": action,
		"names":  []string{fullName},
	})
	return err
}

// StartService starts a snap service.
func (c *RealClient) StartService(ctx context.Context, snapName, serviceName string) error {
	return c.serviceAction(ctx, "start", snapName, serviceName)
}

// StopService stops a snap service.
func (c *RealClient) StopService(ctx context.Context, snapName, serviceName string) error {
	return c.serviceAction(ctx, "stop", snapName, serviceName)
}

// RestartService restarts a snap service.
func (c *RealClient) RestartService(ctx context.Context, snapName, serviceName string) error {
	return c.serviceAction(ctx, "restart", snapName, serviceName)
}

// changeResult is the result payload from GET /v2/changes/<id>.
type changeResult struct {
	Status string `json:"status"`
	Err    *struct {
		Message string `json:"message"`
	} `json:"err"`
}

// WaitForChange polls until the change reaches "Done" or "Error".
func (c *RealClient) WaitForChange(ctx context.Context, changeID string) error {
	for {
		sr, err := c.get(ctx, "/changes/"+changeID)
		if err != nil {
			return err
		}
		var cr changeResult
		if err := json.Unmarshal(sr.Result, &cr); err != nil {
			return fmt.Errorf("snapd: parsing change: %w", err)
		}
		switch cr.Status {
		case "Done":
			return nil
		case "Error":
			msg := "unknown error"
			if cr.Err != nil {
				msg = cr.Err.Message
			}
			return fmt.Errorf("snapd: change %s failed: %s", changeID, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// errNotFound is returned when a snapd assertion endpoint responds with 404.
var errNotFound = errors.New("snapd: assertion not found")

// GetAssertions returns device identity assertions from snapd.
// If assertions do not exist (404), empty strings are returned (not an error).
// Any other error is propagated.
func (c *RealClient) GetAssertions(ctx context.Context) (*Assertions, error) {
	a := &Assertions{}

	headers, err := c.fetchAssertionHeaders(ctx, "/assertions/serial")
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("snapd: fetching serial assertion: %w", err)
	}
	if err == nil {
		a.Serial = headers["serial"]
	}

	model, brand, err := c.fetchModelAssertion(ctx)
	if err != nil && !errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("snapd: fetching model assertion: %w", err)
	}
	if err == nil {
		a.Model = model
		a.Brand = brand
	}

	return a, nil
}

// fetchAssertionHeaders fetches an assertion and returns its header key/value pairs.
func (c *RealClient) fetchAssertionHeaders(ctx context.Context, path string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("snapd: assertion request failed with status %d", resp.StatusCode)
	}
	return parseAssertionHeaders(resp.Body)
}

// fetchModelAssertion fetches the model assertion and returns (model, brand).
func (c *RealClient) fetchModelAssertion(ctx context.Context) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/assertions/model", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("snapd: model assertion request failed with status %d", resp.StatusCode)
	}
	headers, err := parseAssertionHeaders(resp.Body)
	if err != nil {
		return "", "", err
	}
	return headers["model"], headers["brand-id"], nil
}

// parseAssertionHeaders reads the header lines of a snapd assertion (RFC-style key: value pairs).
func parseAssertionHeaders(r io.Reader) (map[string]string, error) {
	headers := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break // blank line separates headers from body
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			headers[key] = val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return headers, nil
}

// systemInfoResult is the relevant part of GET /v2/system-info result.
type systemInfoResult struct {
	Refresh struct {
		Pending string `json:"pending"`
	} `json:"refresh"`
}

// GetRebootRequired returns true when snapd signals a pending restart.
func (c *RealClient) GetRebootRequired(ctx context.Context) (bool, error) {
	sr, err := c.get(ctx, "/system-info")
	if err != nil {
		return false, err
	}
	var info systemInfoResult
	if err := json.Unmarshal(sr.Result, &info); err != nil {
		return false, fmt.Errorf("snapd: parsing system-info: %w", err)
	}
	return info.Refresh.Pending == "restart", nil
}
