import re

with open('/home/michael.croft-white@canonical.com/source/landscape-client-core/internal/transport/transport.go', 'r') as f:
    content = f.read()

new_methods = """
func (c *Client) doPost(ctx context.Context, rawURL string, inBody io.Reader, headers map[string]string) ([]byte, error) {
        if c.totalTimeout > 0 {
                var cancel context.CancelFunc
                ctx, cancel = context.WithTimeout(ctx, c.totalTimeout)
                defer cancel()
        }

        req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, inBody)
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
        mergedHeaders["Content-Type"] = contentType
        if c.userAgent != "" {
                mergedHeaders["User-Agent"] = c.userAgent
        }
        for k, v := range headers {
                mergedHeaders[k] = v
        }
        mergedHeaders["Content-Type"] = contentType

        return c.doPost(ctx, rawURL, bytes.NewReader(body), mergedHeaders)
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

        return c.doPost(ctx, rawURL, strings.NewReader(data.Encode()), headers)
}
"""

start = content.find('func (c *Client) Post(')

with open('/home/michael.croft-white@canonical.com/source/landscape-client-core/internal/transport/transport.go', 'w') as f:
    f.write(content[:start] + new_methods.strip())

