package exchange

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"os"
	"runtime/debug"
	"slices"
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

// maxMessagesPerExchange matches the Python client's max_messages.
const maxMessagesPerExchange = 100

const (
	backoffMin = 300 * time.Second
	backoffMax = 7200 * time.Second
)

// backoff sheds load from an overloaded server. Without it a fleet of devices
// retries at a fixed interval against a server that is already struggling.
// Ported from Python's ExponentialBackoff.
type backoff struct {
	delay time.Duration
	rand  *rand.Rand
}

func newBackoff() *backoff {
	return &backoff{rand: rand.New(rand.NewSource(time.Now().UnixNano()))}
}

// failure records an HTTP status. Only 429 and 5xx back off: a 4xx other than
// 429 is our own bad request, and waiting does not help.
func (b *backoff) failure(status int) {
	if status != 429 && (status < 500 || status > 599) {
		return
	}
	if b.delay == 0 {
		b.delay = backoffMin
	} else {
		b.delay *= 2
	}
	if b.delay > backoffMax {
		b.delay = backoffMax
	}
}

func (b *backoff) success() {
	b.delay = 0
}

// current returns the delay with up to 20% jitter, so a fleet does not retry in
// lockstep.
func (b *backoff) current() time.Duration {
	if b.delay == 0 {
		return 0
	}
	jitter := time.Duration(b.rand.Int63n(int64(b.delay) / 5))
	return b.delay + jitter
}

// previousAPIVersion returns the API version to fall back to when the server
// returns 404 for the current version. This client speaks only one API version,
// so there is no earlier version to downgrade to; the 404 is logged explicitly
// instead of inventing a version ladder.
func previousAPIVersion(_ string) string {
	return ""
}

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
	insecureID string        // guarded by mu; updated from set-id messages
	wake       chan struct{} // buffered(1); written by TriggerExchange
	// spool persists the pending queue across restarts. nil disables persistence
	// (used by tests that do not exercise durability).
	spool *spool
	// dispatchCtx is the daemon-lifetime context used to run inbound message
	// handlers. Handler lifetime must not be tied to the exchange cycle:
	// manager.Runner dispatches into a goroutine and returns immediately, so a
	// per-exchange context would be cancelled while the operation is still running.
	dispatchCtx context.Context
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

// SetSpool enables durable persistence of the pending queue to the given path.
// The queue is a separate file from state.json so a queue write can never touch
// SecureID or the outbound sequence number.
func (e *Exchange) SetSpool(path string) {
	e.spool = newSpool(path)
}

// persistQueue bounds and writes the queue. Called after every mutation so an
// unexpected restart loses at most the messages queued since the last exchange.
func (e *Exchange) persistQueue() {
	if e.spool == nil {
		return
	}
	e.mu.Lock()
	kept, dropped := enforceQueueBound(e.pending)
	e.pending = kept
	snapshot := make([]Message, len(kept))
	copy(snapshot, kept)
	e.mu.Unlock()

	if dropped > 0 {
		slog.Warn("exchange: queue full, dropped oldest telemetry messages, operation results retained", "dropped", dropped)
	}
	if err := e.spool.save(snapshot); err != nil {
		slog.Warn("exchange: cannot save spool", "error", err)
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
		return fmt.Errorf("exchange: cannot load state: %w", err)
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
	e.dispatchCtx = ctx
	e.mu.Unlock()

	// Restore any messages queued before the last restart, prepending them so
	// they are sent before newly generated telemetry.
	if e.spool != nil {
		restored, err := e.spool.load()
		if err != nil {
			slog.Warn("exchange: cannot load spool", "error", err)
		} else if len(restored) > 0 {
			e.mu.Lock()
			e.pending = append(restored, e.pending...)
			e.mu.Unlock()
			slog.Info("exchange: restored queued messages from the spool", "count", len(restored))
		}
	}

	bo := newBackoff()

	for {
		prevSecureID := state.SecureID
		err := e.performExchange(ctx, state)
		// Persist after every exchange attempt: performExchange re-queues on
		// transport failure and on a partial server ACK, so this covers both the
		// success and error paths.
		e.persistQueue()
		if err != nil {
			slog.Warn("exchange: exchange failed", "error", err)
			var httpErr *transport.HTTPError
			if errors.As(err, &httpErr) {
				if httpErr.StatusCode == 404 {
					// An older server does not know this API version. Python drops
					// to the previous version rather than failing permanently.
					if downgraded := previousAPIVersion(apiVersion); downgraded != "" {
						slog.Warn("exchange: server returned 404, downgrading API", "from", apiVersion, "to", downgraded)
					} else {
						slog.Warn("exchange: server returned 404 for unknown API version, no earlier version to downgrade to", "api_version", apiVersion)
					}
				}
				bo.failure(httpErr.StatusCode)
			}
		} else {
			bo.success()
		}
		e.mu.Lock()
		hasUrgent := e.hasUrgentPendingLocked()
		e.mu.Unlock()

		justRegistered := prevSecureID == "" && state.SecureID != ""
		interval := e.cfg.ExchangeInterval
		// Use urgent interval until registration is complete, so the client
		// polls quickly after the server processes the registration request.
		// Also use urgent interval immediately after registration so device
		// info is delivered without waiting 15 minutes.
		if hasUrgent || state.SecureID == "" || justRegistered {
			interval = e.cfg.UrgentExchangeInterval
		}

		// Apply the backoff after the urgent-interval selection: a server
		// returning 503 should not be polled at the urgent interval just because
		// an operation result is queued.
		if d := bo.current(); d > interval {
			slog.Warn("exchange: backing off after server errors", "duration", d)
			interval = d
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
				slog.Warn("exchange: final drain exchange failed", "error", err)
			}
			// Persist whatever remains so a shutdown (including a snap refresh)
			// does not drop unsent messages.
			e.persistQueue()
			return nil
		}
	}
}

// isUrgentType reports whether a message must not wait for the next scheduled
// exchange. The server blocks on operation results.
func isUrgentType(msgType string) bool {
	return msgType == "operation-result"
}

// hasUrgentPendingLocked reports whether the queue holds a message that should
// shorten the next exchange interval. Caller must hold e.mu.
func (e *Exchange) hasUrgentPendingLocked() bool {
	for _, m := range e.pending {
		t, _ := m["type"].(string)
		if isUrgentType(t) {
			return true
		}
	}
	return false
}

// Send enqueues a message for the next scheduled exchange. It does NOT wake the
// exchange loop: plugin telemetry that triggered an exchange per message made
// exchange-interval meaningless — measured 60x the configured rate on a device
// with memory-info at 15s. Matches Python's send_message(urgent=False) default.
func (e *Exchange) Send(_ context.Context, msg Message) error {
	e.mu.Lock()
	e.pending = append(e.pending, msg)
	e.mu.Unlock()
	return nil
}

// SendUrgent enqueues a message and wakes the exchange loop immediately. Use it
// only for messages the server is waiting on, such as operation results.
func (e *Exchange) SendUrgent(_ context.Context, msg Message) error {
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
	return e.SendUrgent(ctx, msg)
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
		e.pending = slices.Insert(e.pending, 0, regMsg)
		e.mu.Unlock()
		slog.Info("exchange: sending registration request",
			"account", e.cfg.AccountName, "title", e.cfg.ComputerTitle, "key_set", e.cfg.RegistrationKey != "")
	}

	// Drain at most maxMessagesPerExchange, matching Python's max_messages. A
	// restored spool after a long outage would otherwise produce one enormous
	// request.
	e.mu.Lock()
	n := min(len(e.pending), maxMessagesPerExchange)
	snapshot := make([]Message, n)
	copy(snapshot, e.pending[:n])
	e.pending = e.pending[n:]
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
			slog.Warn("exchange: dropped messages with types not in server accepted list", "dropped", dropped)
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
		slog.Debug("exchange: sending messages", "count", len(snapshot), "types", outTypes)
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
		return fmt.Errorf("exchange: cannot marshal payload: %w", err)
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
		e.pending = slices.Insert(e.pending, 0, snapshot...)
		e.mu.Unlock()
		return fmt.Errorf("exchange: cannot post to server: %w", err)
	}

	// Decode response.
	rawResponse, err := bpickle.Unmarshal(responseBytes)
	if err != nil {
		return fmt.Errorf("exchange: cannot unmarshal response: %w", err)
	}
	response, ok := rawResponse.(map[string]any)
	if !ok {
		return fmt.Errorf("exchange: response is not a dict")
	}

	// Log the inbound messages for debugging.
	inbound := extractMessages(response)
	slog.Debug("exchange: sequence status",
		"client_sequence", state.OutboundSequence,
		"server_client_sequence", state.NextExpectedFromServer,
		"server_ack", response["next-expected-sequence"])
	if len(inbound) == 0 {
		slog.Debug("exchange: server response: no messages")
	} else {
		for _, m := range inbound {
			slog.Debug("exchange: server message", "type", m["type"])
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
		nAcked := max(int(serverACK-state.OutboundSequence), 0)
		if nAcked < len(snapshot) {
			e.mu.Lock()
			e.pending = slices.Insert(e.pending, 0, snapshot[nAcked:]...)
			e.mu.Unlock()
			slog.Info("exchange: server ACK'd fewer messages than sent, re-queuing",
				"acked", nAcked, "sent", len(snapshot),
				"our_sequence", state.OutboundSequence, "server_expects", serverACK,
				"requeued", len(snapshot)-nAcked)
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
	e.mu.Lock()
	handlerCtx := e.dispatchCtx
	e.mu.Unlock()
	if handlerCtx == nil {
		// performExchange called outside Run (tests, final drain).
		handlerCtx = context.Background()
	}
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
			slog.Info("exchange: registered successfully", "secure_id", state.SecureID, "insecure_id", state.InsecureID)
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
					slog.Info("exchange: received accepted-types", "count", len(types))
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
			e.pending = slices.Insert(e.pending, 0, resyncAck)
			e.mu.Unlock()
			slog.Info("exchange: received resynchronize from server, queuing ack")
		case "unknown-id":
			slog.Warn("exchange: server does not recognize our identity, clearing IDs to re-register")
			state.SecureID = ""
			state.InsecureID = ""
		case "registration":
			info, _ := msgBytes(msg["info"])
			switch info {
			case "unknown-account", "max-pending-computers":
				slog.Warn("exchange: registration failed", "info", info)
			default:
				slog.Info("exchange: registration pending", "info", info)
			}
		case "registration-complete":
			slog.Info("exchange: registration complete")
		}

		if !isSpecialMessageType(msgType) {
			e.mu.Lock()
			handlers := make([]func(ctx context.Context, msg Message), len(e.handlers[msgType]))
			copy(handlers, e.handlers[msgType])
			e.mu.Unlock()

			if len(handlers) == 0 {
				slog.Warn("exchange: no handler for message type", "type", msgType)
				continue
			}
			for _, h := range handlers {
				msg := msg
				go func() {
					defer func() {
						if rec := recover(); rec != nil {
							slog.Error("exchange: handler panicked", "type", msgType, "panic", rec, "stack", string(debug.Stack()))
						}
					}()
					h(handlerCtx, msg)
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
		maps.Copy(mergedPluginState, current.PluginState)

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
		return fmt.Errorf("exchange: cannot save state: %w", err)
	}

	// If messages are still queued (capped backlog or a re-queue), wake the loop
	// so the backlog drains promptly rather than one batch per interval.
	e.mu.Lock()
	backlog := len(e.pending) > 0
	e.mu.Unlock()
	if backlog {
		e.TriggerExchange()
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
