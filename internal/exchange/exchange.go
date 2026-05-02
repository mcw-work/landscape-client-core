package exchange

import (
	"context"
	"crypto/md5"
	"encoding/json"
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
	// SendResultCode is like SendResult but also sets the result-code field in the
	// operation-result message.
	SendResultCode(ctx context.Context, operationID int64, status int, resultCode int64, output string) error
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

	// Kept as a mutex because pending queue, handler map, and insecureID require
	// coordinated read/write access across goroutines; see docs/concurrency.md.
	mu         sync.Mutex
	pending    []Message
	handlers   map[string][]func(ctx context.Context, msg Message)
	wg         sync.WaitGroup
	insecureID string        // guarded by mu; updated from set-id messages
	wake       chan struct{} // buffered(1); written by TriggerExchange
}

// New creates an Exchange.
func New(cfg *config.Config, store *persist.Store, tc *transport.Client) *Exchange {
	return &Exchange{
		cfg:       cfg,
		store:     store,
		transport: tc,
		handlers:  make(map[string][]func(ctx context.Context, msg Message)),
		wake:      make(chan struct{}, 1),
	}
}

// TriggerExchange wakes the exchange loop immediately (e.g. after a ping).
// Safe to call from any goroutine. Non-blocking.
func (e *Exchange) TriggerExchange() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// InsecureID returns the current insecure-id (set after registration).
// Returns empty string if not yet registered.
func (e *Exchange) InsecureID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.insecureID
}

// Run starts the exchange loop. Runs until ctx is cancelled.
// On return, attempts a final drain exchange (bounded by a 5s grace period).
func (e *Exchange) Run(ctx context.Context) error {
	state, err := e.store.Load()
	if err != nil {
		return fmt.Errorf("exchange: loading state: %w", err)
	}

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	// Initialise insecureID from persisted state so the ping loop can use it
	// immediately (i.e. if already registered from a previous run).
	e.mu.Lock()
	e.insecureID = state.InsecureID
	e.mu.Unlock()

	for {
		prevSecureID := state.SecureID
		if err := e.performExchange(ctx, state); err != nil {
			log.Printf("exchange: exchange failed: %v", err)
		}
		e.wg.Wait()

		e.mu.Lock()
		hasPending := len(e.pending) > 0
		e.mu.Unlock()

		justRegistered := prevSecureID == "" && state.SecureID != ""
		interval := e.cfg.ExchangeInterval
		// Use urgent interval until registration is complete, so the client
		// polls quickly after the server processes the registration request.
		// Also use urgent interval immediately after registration so device
		// info is delivered without waiting 15 minutes.
		if hasPending || state.SecureID == "" || justRegistered {
			interval = e.cfg.UrgentExchangeInterval
		}

		if timer == nil {
			timer = time.NewTimer(interval)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
		}

		select {
		case <-timer.C:
			// next iteration
		case <-e.wake:
			// ping triggered an urgent exchange
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

// Send enqueues a message for the next exchange and wakes the exchange loop
// so the message is delivered promptly rather than waiting for the next timer tick.
// Safe to call from multiple goroutines.
func (e *Exchange) Send(_ context.Context, msg Message) error {
	e.mu.Lock()
	e.pending = append(e.pending, msg)
	e.mu.Unlock()
	e.TriggerExchange()
	return nil
}

// Subscribe registers a handler for a given inbound message type.
// Multiple handlers can be registered for the same type.
func (e *Exchange) Subscribe(msgType string, handler func(ctx context.Context, msg Message)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[msgType] = append(e.handlers[msgType], handler)
}

func (e *Exchange) sendOperationResult(ctx context.Context, operationID int64, status int, resultCode *int64, output string) error {
	msg := Message{
		"type":         "operation-result",
		"operation-id": operationID,
		"status":       int64(status),
		"result-text":  output,
		// The Python broker always injects timestamp (as int) before sending.
		// The Landscape server uses it to display when the operation completed.
		"timestamp": int64(time.Now().Unix()),
	}
	if resultCode != nil {
		msg["result-code"] = *resultCode
	}
	return e.Send(ctx, msg)
}

// SendResult enqueues an operation-result message.
func (e *Exchange) SendResult(ctx context.Context, operationID int64, status int, output string) error {
	return e.sendOperationResult(ctx, operationID, status, nil, output)
}

// SendResultCode enqueues an operation-result message with a result-code field.
func (e *Exchange) SendResultCode(ctx context.Context, operationID int64, status int, resultCode int64, output string) error {
	return e.sendOperationResult(ctx, operationID, status, &resultCode, output)
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
			"type":                  "register",
			"api":                   apiVersion,
			"account_name":          e.cfg.AccountName,
			"registration_password": e.cfg.RegistrationKey,
			"computer_title":        e.cfg.ComputerTitle,
			"hostname":              hostname,
			"machine_id":            machineID,
			"tags":                  e.cfg.Tags,
			"access_group":          e.cfg.AccessGroup,
		}
		e.mu.Lock()
		e.pending = append([]Message{regMsg}, e.pending...)
		e.mu.Unlock()
		log.Printf("exchange: sending registration request (account=%q title=%q key-set=%v)",
			e.cfg.AccountName, e.cfg.ComputerTitle, e.cfg.RegistrationKey != "")
	}

	// Take a snapshot of pending under lock and clear the queue.
	e.mu.Lock()
	snapshot := make([]Message, len(e.pending))
	copy(snapshot, e.pending)
	e.pending = nil
	e.mu.Unlock()

	// Filter out message types the server has not declared it handles.
	// The server's accepted types are stored in state.AcceptedTypes after the
	// first accepted-types exchange. Until then (empty list), allow all types
	// through so that the initial register/registration messages can be sent.
	if len(state.AcceptedTypes) > 0 {
		accepted := make(map[string]bool, len(state.AcceptedTypes))
		for _, t := range state.AcceptedTypes {
			accepted[t] = true
		}
		// Always allow protocol-level messages regardless of accepted types.
		for _, t := range []string{"register", "resynchronize", "operation-result"} {
			accepted[t] = true
		}
		filtered := snapshot[:0]
		for _, m := range snapshot {
			t, _ := m["type"].(string)
			if accepted[t] {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) != len(snapshot) {
			dropped := len(snapshot) - len(filtered)
			log.Printf("exchange: dropped %d message(s) with types not in server accepted list", dropped)
		}
		snapshot = filtered
	}

	// Build the messages slice as []any for bpickle.
	messages := make([]any, len(snapshot))
	for i, m := range snapshot {
		messages[i] = map[string]any(m)
	}

	// Log outbound messages so we can confirm data is reaching the server.
	if len(snapshot) > 0 {
		outTypes := make([]string, 0, len(snapshot))
		for _, m := range snapshot {
			if t, ok := m["type"].(string); ok {
				outTypes = append(outTypes, t)
			}
		}
		log.Printf("exchange: sending %d message(s): %v", len(snapshot), outTypes)
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
	if len(state.AcceptedTypesHash) == 0 {
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
	// After registration, X-Computer-ID is the secure-id (long string).
	// Before registration, no X-Computer-ID is sent (matches Python client).
	headers := map[string]string{
		"X-Message-API": apiVersion,
	}
	if state.SecureID != "" {
		headers["X-Computer-ID"] = state.SecureID
	}
	if state.ExchangeToken != "" {
		headers["X-Exchange-Token"] = state.ExchangeToken
	}

	// POST to server.
	responseBytes, err := e.transport.Post(ctx, e.cfg.URL, headers, body)
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

	// Log the inbound messages for debugging.
	inbound := extractMessages(response)
	log.Printf("exchange: client-seq=%d server-client-seq=%d server-ack=%v",
		state.OutboundSequence, state.NextExpectedFromServer, response["next-expected-sequence"])
	if len(inbound) == 0 {
		log.Printf("exchange: server response: no messages")
	} else {
		for _, m := range inbound {
			log.Printf("exchange: server message: type=%q", m["type"])
		}
	}

	// response["next-expected-sequence"] is the server's ACK: the next client→server
	// sequence the server wants to receive. Use it to set OutboundSequence.
	sentUpTo := state.OutboundSequence + int64(len(snapshot))
	serverACK := sentUpTo // default: assume all messages ACK'd
	if v, ok := response["next-expected-sequence"]; ok {
		serverACK = toInt64(v)
	}

	// If the server's ACK is below what we just sent, re-enqueue un-ACK'd messages
	// so they are retransmitted at the correct sequence on the next exchange.
	if serverACK < sentUpTo {
		nAcked := int(serverACK - state.OutboundSequence)
		if nAcked < 0 {
			nAcked = 0
		}
		if nAcked < len(snapshot) {
			e.mu.Lock()
			e.pending = append(snapshot[nAcked:], e.pending...)
			e.mu.Unlock()
			log.Printf("exchange: server ACK'd %d/%d messages (our seq=%d, server wants=%d); re-queuing %d",
				nAcked, len(snapshot), state.OutboundSequence, serverACK, len(snapshot)-nAcked)
		}
	}
	state.OutboundSequence = serverACK

	// Advance server→client sequence for each server message we receive.
	state.NextExpectedFromServer += int64(len(inbound))

	// Store the exchange token for the next request.
	if v, ok := response["next-exchange-token"]; ok {
		if s, ok := v.(string); ok {
			state.ExchangeToken = s
		}
	}

	// Process inbound messages: special types are handled here; others go to subscribers.
	for _, msg := range inbound {
		msgType, _ := msg["type"].(string)

		switch msgType {
		case "set-id":
			if v, ok := msg["id"]; ok {
				switch x := v.(type) {
				case string:
					state.SecureID = x
				case []byte:
					state.SecureID = string(x)
				case int64:
					state.SecureID = fmt.Sprintf("%d", x)
				}
			}
			if v, ok := msg["insecure-id"]; ok {
				switch x := v.(type) {
				case string:
					state.InsecureID = x
				case []byte:
					state.InsecureID = string(x)
				case int64:
					state.InsecureID = fmt.Sprintf("%d", x)
				}
			}
			// Reset plugin state so all monitors re-report to the newly registered server.
			state.PluginState = nil
			// Keep insecureID in sync for the ping loop.
			e.mu.Lock()
			e.insecureID = state.InsecureID
			e.mu.Unlock()
			log.Printf("exchange: registered successfully (secure-id=%q insecure-id=%q)", state.SecureID, state.InsecureID)
		case "accepted-types":
			if v, ok := msg["types"]; ok {
				if l, ok := v.([]any); ok {
					var types []string
					for _, t := range l {
						switch s := t.(type) {
						case string:
							types = append(types, s)
						case []byte:
							types = append(types, string(s))
						}
					}
					state.AcceptedTypes = types
					state.AcceptedTypesHash = hashTypes(types)
					log.Printf("exchange: accepted-types: %d types", len(types))
				}
			}
		case "resynchronize":
			// Do NOT reset OutboundSequence — the server still expects the same
			// sequence number. Just clear plugin state so monitors re-report,
			// and send the resynchronize ack back so the server calls
			// computer.resynchronize() which resets its own next_expected_sequence.
			state.PluginState = nil
			// Send resynchronize back to the server with the operation-id.
			resyncAck := Message{"type": "resynchronize"}
			if opid, ok := msg["operation-id"]; ok {
				resyncAck["operation-id"] = opid
			}
			e.mu.Lock()
			e.pending = append([]Message{resyncAck}, e.pending...)
			e.mu.Unlock()
			log.Printf("exchange: received resynchronize from server, queuing ack")
		case "unknown-id":
			log.Printf("exchange: server does not recognize our identity, clearing IDs to re-register")
			state.SecureID = ""
			state.InsecureID = ""
		case "registration":
			info, _ := msgBytes(msg["info"])
			switch info {
			case "unknown-account", "max-pending-computers":
				log.Printf("exchange: registration failed: %s", info)
			default:
				log.Printf("exchange: registration pending (info=%q)", info)
			}
		case "registration-complete":
			log.Printf("exchange: registration complete")
		}

		if !isSpecialMessageType(msgType) {
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

	// Persist updated state via a serialized read-modify-write, preserving any
	// plugin state saved by monitor goroutines since the last exchange.
	if err := e.store.Update(func(current *persist.State) error {
		mergedPluginState := state.PluginState
		if mergedPluginState == nil {
			mergedPluginState = make(map[string]json.RawMessage)
		}
		for k, v := range current.PluginState {
			mergedPluginState[k] = v
		}

		current.SecureID = state.SecureID
		current.InsecureID = state.InsecureID
		current.ServerUUID = state.ServerUUID
		current.OutboundSequence = state.OutboundSequence
		current.NextExpectedFromServer = state.NextExpectedFromServer
		current.ExchangeToken = state.ExchangeToken
		current.AcceptedTypes = append(current.AcceptedTypes[:0], state.AcceptedTypes...)
		current.AcceptedTypesHash = append(current.AcceptedTypesHash[:0], state.AcceptedTypesHash...)
		current.PluginState = mergedPluginState

		return nil
	}); err != nil {
		return fmt.Errorf("exchange: saving state: %w", err)
	}

	return nil
}

func isSpecialMessageType(msgType string) bool {
	switch msgType {
	case "set-id", "accepted-types", "resynchronize", "unknown-id", "registration", "registration-complete":
		return true
	default:
		return false
	}
}

// msgBytes converts a bpickle value that may be []byte or string to string.
func msgBytes(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
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

// hashTypes returns the raw MD5 digest of semicolon-joined type names,
// matching the Python client: md5(";".join(types)).digest().
// The list is NOT sorted — it must be in the order provided by the server.
func hashTypes(types []string) []byte {
	joined := strings.Join(types, ";")
	h := md5.Sum([]byte(joined))
	return h[:]
}
