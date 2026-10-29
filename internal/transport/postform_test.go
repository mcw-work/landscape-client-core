package transport_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/canonical/landscape-client-core/internal/transport"
)

func TestHTTPError_Error(t *testing.T) {
	withURL := &transport.HTTPError{StatusCode: 503, URL: "https://x/message-system"}
	if got, want := withURL.Error(), "transport: HTTP 503 from https://x/message-system"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	noURL := &transport.HTTPError{StatusCode: 404}
	if got, want := noURL.Error(), "transport: HTTP 404"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPostForm_SendsFormEncodedBody(t *testing.T) {
	var gotCT, gotUA, gotID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		_ = r.ParseForm()
		gotID = r.PostForm.Get("insecure_id")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c, err := transport.New(transport.Config{UserAgent: "lcc-test/1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, err := c.PostForm(context.Background(), srv.URL, url.Values{"insecure_id": {"ins-1"}})
	if err != nil {
		t.Fatalf("PostForm: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotUA != "lcc-test/1" {
		t.Errorf("User-Agent = %q", gotUA)
	}
	if gotID != "ins-1" {
		t.Errorf("insecure_id = %q, want ins-1", gotID)
	}
}

func TestPostForm_HTTPErrorCarriesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("busy"))
	}))
	defer srv.Close()

	c, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.PostForm(context.Background(), srv.URL, url.Values{})
	var httpErr *transport.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("want *HTTPError, got %v", err)
	}
	if httpErr.StatusCode != http.StatusServiceUnavailable || string(httpErr.Body) != "busy" {
		t.Errorf("got status %d body %q", httpErr.StatusCode, httpErr.Body)
	}
}

// A hostile server must not be able to make the client buffer an unbounded body.
func TestPost_ResponseBodyIsBounded(t *testing.T) {
	const limit = 4 * 1024 * 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(zeroReader{}, limit+1024))
	}))
	defer srv.Close()

	c, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body, err := c.Post(context.Background(), srv.URL, nil, []byte("x"))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(body) != limit {
		t.Errorf("body length = %d, want capped at %d", len(body), limit)
	}
}

// The error body is capped too, so a 5xx page cannot balloon memory.
func TestPost_ErrorBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(bytes.Repeat([]byte("e"), 10000))
	}))
	defer srv.Close()

	c, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Post(context.Background(), srv.URL, nil, nil)
	var httpErr *transport.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("want *HTTPError, got %v", err)
	}
	if len(httpErr.Body) != 4096 {
		t.Errorf("error body length = %d, want 4096", len(httpErr.Body))
	}
}

func TestPost_InvalidURLIsError(t *testing.T) {
	c, err := transport.New(transport.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Post(context.Background(), "://bad", nil, nil); err == nil {
		t.Error("want error for invalid URL")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
