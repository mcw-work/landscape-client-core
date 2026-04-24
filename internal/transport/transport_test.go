package transport_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/transport"
)

// newTestClient is a helper to create a Client or fail the test.
func newTestClient(t *testing.T, cfg transport.Config) *transport.Client {
	t.Helper()
	c, err := transport.New(cfg)
	if err != nil {
		t.Fatalf("transport.New: %v", err)
	}
	return c
}

// Test 1: Successful POST returns response body.
func TestPost_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	got, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// Test 2: HTTPError returned for 4xx status.
func TestPost_4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	httpErr, ok := err.(*transport.HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != http.StatusNotFound {
		t.Errorf("got status %d, want %d", httpErr.StatusCode, http.StatusNotFound)
	}
}

// Test 3: HTTPError returned for 5xx status.
func TestPost_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	httpErr, ok := err.(*transport.HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if httpErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("got status %d, want %d", httpErr.StatusCode, http.StatusInternalServerError)
	}
}

// Test 4: Content-Type: application/octet-stream always present.
func TestPost_ContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", gotCT, "application/octet-stream")
	}
}

// Test 5: User-Agent header set when configured.
func TestPost_UserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{UserAgent: "test-agent/1.0"})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUA != "test-agent/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "test-agent/1.0")
	}
}

// Test 6: Custom headers are sent.
func TestPost_CustomHeaders(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Custom")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", map[string]string{"X-Custom": "myvalue"}, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotHeader != "myvalue" {
		t.Errorf("X-Custom = %q, want %q", gotHeader, "myvalue")
	}
}

// Test 7: Custom headers cannot override Content-Type.
func TestPost_CustomHeadersCannotOverrideContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", map[string]string{"Content-Type": "text/plain"}, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want %q", gotCT, "application/octet-stream")
	}
}

// Test 8: Context cancellation aborts in-flight request.
func TestPost_ContextCancellation(t *testing.T) {
	started := make(chan struct{})
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Block until the test unblocks us (via CloseClientConnections or channel).
		select {
		case <-unblock:
		case <-time.After(10 * time.Second):
		}
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	c := newTestClient(t, transport.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Post(ctx, srv.URL+"/post", nil, []byte("body"))
		done <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after context cancellation, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Post did not return after context cancellation")
	}
}

// Test 9: TLS with custom CA cert: use httptest.NewTLSServer, load its CA cert.
func TestPost_TLSCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("tls-ok"))
	}))
	defer srv.Close()

	certFile := writeTLSServerCert(t, srv)

	c := newTestClient(t, transport.Config{SSLPublicKey: certFile})
	got, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "tls-ok" {
		t.Errorf("got %q, want %q", got, "tls-ok")
	}
}

// writeTLSServerCert writes the TLS server certificate to a temp file as PEM
// and returns the file path.
func writeTLSServerCert(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	cert := srv.TLS.Certificates[0]
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parsing server cert: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "ca-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer f.Close()

	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: x509Cert.Raw}); err != nil {
		t.Fatalf("writing cert PEM: %v", err)
	}
	return f.Name()
}

// Test 10: TLS failure: valid TLS server but Client configured with wrong CA.
func TestPost_TLSFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Use default system CAs (which don't trust the test server's self-signed cert).
	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err == nil {
		t.Fatal("expected TLS error, got nil")
	}
}

// Test 11: No redirect: server returns 301 → Client returns HTTPError{301}.
func TestPost_NoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err == nil {
		t.Fatal("expected error for redirect, got nil")
	}
	httpErr, ok := err.(*transport.HTTPError)
	if !ok {
		t.Fatalf("expected *HTTPError, got %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusMovedPermanently {
		t.Errorf("got status %d, want %d", httpErr.StatusCode, http.StatusMovedPermanently)
	}
}

// Test 12: TotalTimeout: server delays 200ms, Client has 50ms timeout → error.
func TestPost_TotalTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{TotalTimeout: 50 * time.Millisecond})
	_, err := c.Post(context.Background(), srv.URL+"/post", nil, []byte("body"))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// Test 13: New with non-existent SSLPublicKey path → error.
func TestNew_NonExistentSSLPublicKey(t *testing.T) {
	_, err := transport.New(transport.Config{SSLPublicKey: "/nonexistent/path/to/cert.pem"})
	if err == nil {
		t.Fatal("expected error for non-existent cert file, got nil")
	}
}

// Test 14: New with invalid PEM content → error.
func TestNew_InvalidPEM(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bad-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	f.WriteString("this is not valid PEM content")
	f.Close()

	_, err = transport.New(transport.Config{SSLPublicKey: f.Name()})
	if err == nil {
		t.Fatal("expected error for invalid PEM content, got nil")
	}
}

// Test 15: User-Agent header in headers map overrides config User-Agent.
func TestPost_UserAgentOverride(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, transport.Config{UserAgent: "config-agent/1.0"})
	_, err := c.Post(context.Background(), srv.URL+"/post", map[string]string{"User-Agent": "custom-agent"}, []byte("body"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotUA != "custom-agent" {
		t.Errorf("User-Agent = %q, want %q", gotUA, "custom-agent")
	}
}
