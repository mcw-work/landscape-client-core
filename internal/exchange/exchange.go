package exchange

import (
	"context"
	"crypto/md5"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/canonical/landscape-client-core/internal/bpickle"
	"github.com/canonical/landscape-client-core/internal/config"
	"github.com/canonical/landscape-client-core/internal/persist"
	"github.com/canonical/landscape-client-core/internal/transport"
)

const apiVersion = "3.3"

// Message is a single Landscape protocol message (a bpickle dict).
type Message map[string]any

// MessageSink accepts outbound messages from monitor plugins.
type MessageSink interface {
	Send(ctx context.Context, msg Message) error
}

// CommandSource allows manager handlers to subscribe to inbound message types.
type CommandSource interface {
	Subscribe(msgType string, handler func(ctx context.Context, msg Message))
}

// ResultSink allows manager handlers to send operation results back to the server.
type ResultSink interface {
	SendResult(ctx context.Context, operationID int64, status int, output string) error
}

// StatusSucceeded and StatusFailed match the Python client's constants.
const (
	StatusSucceeded = 6
	StatusFailed    = 5
)

// Exchange is the central coordinator: message queue, sequence tracking, exchange loop.
type Exchange struct {
	cfg       *config.Config
	store     *persist.Store
	transport *transport.Client

	mu       sync.Mutex
	pending  []Message
	handlers map[string][]func(ctx context.Context, msg Message)
	wg       sync.WaitGroup
}

// New creates an Exchange.
func New(cfg *config.Config, store *persist.Store, tc *transport.Client) *Exchange {
	return &Exchange{
		cfg:       cfg,
		store:     store,
		transport: tc,
		handlers:  make(map[string][]func(ctx context.Context, msg Message)),
	}
}

// Run starts the exchange loop. Runs until ctx is cancelled.
// On return, attempts a final drain exchange (bounded by a 5s grace period).
func (e *Exchange) Run(ctx context.Context) error {
	state, err := e.store.Load()
	if err != nil {
		return fmt.Errorf("exchange: loading state: %w", err)
	}

	for {
		if err := e.performExchange(ctx, state); err != nil {
			log.Printf("exchange: exchange failed: %v", err)
		}
		e.wg.Wait()

		e.mu.Lock()
		hasPending := len(e.pending) > 0
		e.mu.Unlock()

		interval := e.cfg.ExchangeInterval
		if hasPending {
			interval = e.cfg.UrgentExchangeInterval
		}

		select {
		case <-time.After(interval):
			// next iteration
		case <-ctx.Done():
			graceCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.performExchange(graceCtx, state); err != nil {
				log.Printf("exchange: final drain exchange failed: %v", err)
			}
			done := make(chan struct{})
			go func() {
				e.wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-graceCtx.Done():
			}
			return nil
		}
	}
}

// Send enqueues a message for the next exchange.
// Safe to call from multiple goroutines.
func (e *Exchange) Send(_ context.Context, msg Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending = append(e.pending, msg)
	return nil
}

// Subscribe registers a handler for a given inbound message type.
// Multiple handlers can be registered for the same type.
func (e *Exchange) Subscribe(msgType string, handler func(ctx context.Context, msg Message)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[msgType] = append(e.handlers[msgType], handler)
}

// SendResult enqueues an operation-result message.
func (e *Exchange) SendResult(ctx context.Context, operationID int64, status int, output string) error {
	return e.Send(ctx, Message{
		"type":         "operation-result",
		"operation-id": operationID,
		"status":       int64(status),
		"result-text":  output,
	})
}

// performExchange executes a single exchange with the Landscape server.
func (e *Exchange) performExchange(ctx context.Context, state *persist.State) error {
	// Inject a registration message at the front of the queue if not yet registered.
	if state.SecureID == "" {
		hostname, _ := os.Hostname()
		machineID := ""
		if data, err := os.ReadFile("/etc/machine-id"); err == nil {
			machineID = strings.TrimSpace(string(data))
		}
		regMsg := Message{
			"type":             "register",
			"api":              apiVersion,
			"account-name":     e.cfg.AccountName,
			"registration-key": e.cfg.RegistrationKey,
			"computer-title":   e.cfg.ComputerTitle,
			"hostname":         hostname,
			"machine-id":       machineID,
			"tags":             e.cfg.Tags,
			"access-group":     e.cfg.AccessGroup,
		}
		e.mu.Lock()
		e.pending = append([]Message{regMsg}, e.pending...)
		e.mu.Unlock()
	}

	// Take a snapshot of pending under lock and clear the queue.
	e.mu.Lock()
	snapshot := make([]Message, len(e.pending))
	copy(snapshot, e.pending)
	e.pending = nil
	e.mu.Unlock()

	// Build the messages slice as []any for bpickle.
	messages := make([]any, len(snapshot))
	for i, m := range snapshot {
		messages[i] = map[string]any(m)
	}

	// Assemble the exchange payload.
	payload := map[string]any{
		"server-api":             apiVersion,
		"client-api":             apiVersion,
		"sequence":               state.OutboundSequence,
		"accepted-types":         state.AcceptedTypesHash,
		"messages":               messages,
		"total-messages":         int64(len(snapshot)),
		"next-expected-sequence": state.NextExpectedFromServer,
	}

	// Include client-accepted-types when we do not have a confirmed hash from the server.
	if state.AcceptedTypesHash == "" {
		e.mu.Lock()
		clientTypes := make([]string, 0, len(e.handlers))
		for t := range e.handlers {
			clientTypes = append(clientTypes, t)
		}
		e.mu.Unlock()
		sort.Strings(clientTypes)
		typesAny := make([]any, len(clientTypes))
		for i, t := range clientTypes {
			typesAny[i] = t
		}
		payload["client-accepted-types"] = typesAny
	}

	// Marshal payload.
	body, err := bpickle.Marshal(payload)
	if err != nil {
		return fmt.Errorf("exchange: marshaling payload: %w", err)
	}

	// Build request headers.
	headers := map[string]string{
		"X-Message-API":    apiVersion,
		"X-Computer-ID":    state.InsecureID,
		"X-Exchange-Token": state.ExchangeToken,
	}

	// POST to server.
	responseBytes, err := e.transport.Post(ctx, e.cfg.URL+"/message-system", headers, body)
	if err != nil {
		// Re-queue the snapshot so messages are not lost on transport failure.
		e.mu.Lock()
		e.pending = append(snapshot, e.pending...)
		e.mu.Unlock()
		return fmt.Errorf("exchange: posting to server: %w", err)
	}

	// Decode response.
	rawResponse, err := bpickle.Unmarshal(responseBytes)
	if err != nil {
		return fmt.Errorf("exchange: unmarshaling response: %w", err)
	}
	response, ok := rawResponse.(map[string]any)
	if !ok {
		return fmt.Errorf("exchange: response is not a dict")
	}

	// Advance outbound sequence by the number of messages sent.
	state.OutboundSequence += int64(len(snapshot))

	// Adopt the server's next-expected-sequence.
	if v, ok := response["next-expected-sequence"]; ok {
		state.NextExpectedFromServer = toInt64(v)
	}

	// Store the exchange token for the next request.
	if v, ok := response["next-exchange-token"]; ok {
		if s, ok := v.(string); ok {
			state.ExchangeToken = s
		}
	}

	// Process inbound messages: special types are handled here; others go to subscribers.
	specialTypes := map[string]bool{
		"set-id":         true,
		"accepted-types": true,
		"resynchronize":  true,
	}

	for _, msg := range extractMessages(response) {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "set-id":
			if v, ok := msg["secure-id"]; ok {
				state.SecureID, _ = v.(string)
			}
			if v, ok := msg["insecure-id"]; ok {
				state.InsecureID, _ = v.(string)
			}

		case "accepted-types":
			if v, ok := msg["types"]; ok {
				if list, ok := v.([]any); ok {
					types := make([]string, 0, len(list))
					for _, t := range list {
						if s, ok := t.(string); ok {
							types = append(types, s)
						}
					}
					state.AcceptedTypes = types
					state.AcceptedTypesHash = hashTypes(types)
				}
			}

		case "resynchronize":
			state.OutboundSequence = 0
			state.NextExpectedFromServer = 0
			state.PluginState = nil
		}

		if !specialTypes[msgType] {
			e.mu.Lock()
			handlers := make([]func(ctx context.Context, msg Message), len(e.handlers[msgType]))
			copy(handlers, e.handlers[msgType])
			e.mu.Unlock()

			if len(handlers) == 0 {
				log.Printf("exchange: no handler for message type %q", msgType)
				continue
			}
			for _, h := range handlers {
				h := h
				msg := msg
				e.wg.Add(1)
				go func() {
					defer e.wg.Done()
					h(ctx, msg)
				}()
			}
		}
	}

	// Persist updated state.
	if err := e.store.Save(state); err != nil {
		return fmt.Errorf("exchange: saving state: %w", err)
	}

	return nil
}

// toInt64 converts numeric bpickle values to int64.
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// extractMessages pulls the messages list out of a server response dict.
func extractMessages(response map[string]any) []Message {
	v, ok := response["messages"]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	msgs := make([]Message, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			msgs = append(msgs, Message(m))
		}
	}
	return msgs
}

// hashTypes returns the MD5 hex digest of comma-joined sorted type names,
// matching the Python client: md5(",".join(sorted(types))).
func hashTypes(types []string) string {
	sorted := make([]string, len(types))
	copy(sorted, types)
	sort.Strings(sorted)
	joined := strings.Join(sorted, ",")
	h := md5.Sum([]byte(joined))
	return fmt.Sprintf("%x", h)
}
