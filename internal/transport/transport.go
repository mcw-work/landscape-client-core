package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultConnectTimeout = 30 * time.Second
	contentType           = "application/octet-stream"
)

// Config configures a transport Client.
type Config struct {
	SSLPublicKey   string        // path to CA cert file; "" means use system CAs
	HTTPProxy      string        // proxy URL for http:// requests; "" means no proxy
	HTTPSProxy     string        // proxy URL for https:// requests; "" means no proxy
	ConnectTimeout time.Duration // default: 30s
	TotalTimeout   time.Duration // default: 600s
	UserAgent      string        // e.g. "landscape-client-core/1.0"
}

// HTTPError is returned when the server responds with a non-2xx status code.
type HTTPError struct {
	StatusCode int
	Body       []byte
	URL        string
}

func (e *HTTPError) Error() string {
	if e.URL != "" {
		return fmt.Sprintf("transport: HTTP %d from %s", e.StatusCode, e.URL)
	}
	return fmt.Sprintf("transport: HTTP %d", e.StatusCode)
}

// Client sends HTTP POST requests with bpickle-encoded bodies.
type Client struct {
	httpClient   *http.Client
	userAgent    string
	totalTimeout time.Duration
}

// New creates a Client from cfg.
// Returns an error if SSLPublicKey is set but the file cannot be loaded or parsed.
func New(cfg Config) (*Client, error) {
	tlsCfg := &tls.Config{}

	if cfg.SSLPublicKey != "" {
		pemData, err := os.ReadFile(cfg.SSLPublicKey)
		if err != nil {
			return nil, fmt.Errorf("transport: reading SSL public key: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("transport: no valid PEM certificates found in %s", cfg.SSLPublicKey)
		}
		tlsCfg.RootCAs = pool
	}

	connectTimeout := cfg.ConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = defaultConnectTimeout
	}

	httpProxy := cfg.HTTPProxy
	httpsProxy := cfg.HTTPSProxy

	proxyFunc := func(req *http.Request) (*url.URL, error) {
		scheme := req.URL.Scheme
		if scheme == "https" && httpsProxy != "" {
			return url.Parse(httpsProxy)
		}
		if scheme == "http" && httpProxy != "" {
			return url.Parse(httpProxy)
		}
		return http.ProxyFromEnvironment(req)
	}

	transport := &http.Transport{
		TLSClientConfig:     tlsCfg,
		Proxy:               proxyFunc,
		TLSHandshakeTimeout: connectTimeout,
		DialContext: (&net.Dialer{
			Timeout: connectTimeout,
		}).DialContext,
	}

	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &Client{
		httpClient:   httpClient,
		userAgent:    cfg.UserAgent,
		totalTimeout: cfg.TotalTimeout,
	}, nil
}

// doRequest sends an HTTP request with the given method, URL, body (may be nil), and headers.
// Returns the response body on success (2xx), or HTTPError for non-2xx responses.
func (c *Client) doRequest(ctx context.Context, method, rawURL string, inBody io.Reader, headers map[string]string) ([]byte, error) {
	if c.totalTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.totalTimeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, inBody)
	if err != nil {
		return nil, fmt.Errorf("transport: creating request: %w", err)
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: sending request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		const maxErrBody = 4096
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Body: errBody, URL: rawURL}
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("transport: reading response from %s: %w", rawURL, err)
	}
	return respBody, nil
}

// Post sends a POST request to rawURL with the given body and headers.
// Returns the response body on success (2xx), or HTTPError for non-2xx responses.
// The caller owns the returned bytes.
func (c *Client) Post(ctx context.Context, rawURL string, headers map[string]string, body []byte) ([]byte, error) {
	mergedHeaders := make(map[string]string, len(headers)+2)
	if c.userAgent != "" {
		mergedHeaders["User-Agent"] = c.userAgent
	}
	for k, v := range headers {
		mergedHeaders[k] = v
	}
	mergedHeaders["Content-Type"] = contentType // enforce; callers cannot override
	return c.doRequest(ctx, http.MethodPost, rawURL, bytes.NewReader(body), mergedHeaders)
}

// Get sends a GET request to rawURL with the given headers.
// Returns the response body on success (2xx), or HTTPError for non-2xx responses.
// The caller owns the returned bytes.
func (c *Client) Get(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	merged := make(map[string]string, len(headers)+1)
	if c.userAgent != "" {
		merged["User-Agent"] = c.userAgent
	}
	for k, v := range headers {
		merged[k] = v
	}
	return c.doRequest(ctx, http.MethodGet, rawURL, nil, merged)
}

// PostForm sends an HTTP POST with an application/x-www-form-urlencoded body.
// Used by the ping client to query the Landscape ping server.
func (c *Client) PostForm(ctx context.Context, rawURL string, data url.Values) ([]byte, error) {
	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}
	if c.userAgent != "" {
		headers["User-Agent"] = c.userAgent
	}

	return c.doRequest(ctx, http.MethodPost, rawURL, strings.NewReader(data.Encode()), headers)
}
