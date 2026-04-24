// Package ping implements the Landscape lightweight ping mechanism.
//
// The Pinger periodically POSTs to the Landscape ping server with the
// client's insecure-id. If the server responds that messages are waiting
// ({"messages": True} in bpickle), an urgent exchange is triggered.
//
// This mirrors the Python client's broker/ping.py Pinger class.
package ping

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/transport"
)

// Pinger periodically POSTs to the Landscape ping server and triggers
// an urgent exchange when the server reports messages are waiting.
type Pinger struct {
	pingURL         string
	getInsecureID   func() string
	triggerExchange func()
	tc              *transport.Client

	mu       sync.Mutex
	interval time.Duration
}

// New returns a Pinger.
//
//   - pingURL: the URL to POST to (e.g. http://landscape.canonical.com/ping).
//   - getInsecureID: called each tick to get the current insecure-id; returns
//     empty string if not yet registered (ping is skipped).
//   - triggerExchange: called when the server reports messages are waiting.
//   - interval: initial ping interval (updated by SetInterval).
//   - tc: transport client for proxy/TLS configuration.
func New(
	pingURL string,
	getInsecureID func() string,
	triggerExchange func(),
	interval time.Duration,
	tc *transport.Client,
) *Pinger {
	return &Pinger{
		pingURL:         pingURL,
		getInsecureID:   getInsecureID,
		triggerExchange: triggerExchange,
		tc:              tc,
		interval:        interval,
	}
}

// SetInterval updates the ping interval. Safe to call from any goroutine.
// Takes effect from the next scheduled ping.
func (p *Pinger) SetInterval(d time.Duration) {
	p.mu.Lock()
	p.interval = d
	p.mu.Unlock()
}

// Run starts the ping loop. Blocks until ctx is cancelled, then returns nil.
func (p *Pinger) Run(ctx context.Context) error {
	for {
		p.mu.Lock()
		interval := p.interval
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		insecureID := p.getInsecureID()
		if insecureID == "" {
			// Not yet registered; skip ping.
			continue
		}

		hasMessages, err := p.doPing(ctx, insecureID)
		if err != nil {
			log.Printf("ping: error contacting ping server at %s: %v", p.pingURL, err)
			continue
		}
		if hasMessages {
			log.Printf("ping: server has messages waiting, triggering urgent exchange")
			p.triggerExchange()
		}
	}
}

// doPing POSTs to the ping server and returns true when the server indicates
// that messages are waiting for this client.
func (p *Pinger) doPing(ctx context.Context, insecureID string) (bool, error) {
	data := url.Values{"insecure_id": []string{insecureID}}
	respBytes, err := p.tc.PostForm(ctx, p.pingURL, data)
	if err != nil {
		return false, err
	}

	raw, err := bpickle.Unmarshal(respBytes)
	if err != nil {
		return false, fmt.Errorf("decoding response: %w", err)
	}

	m, ok := raw.(map[string]any)
	if !ok {
		return false, nil
	}

	v, ok := m["messages"]
	if !ok {
		return false, nil
	}

	b, _ := v.(bool)
	return b, nil
}
