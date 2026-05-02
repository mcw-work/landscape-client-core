package manager

import (
	"context"
	"fmt"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
	"github.com/canonical/landscape-client-core/internal/snapd"
)

const changeTimeout = 10 * time.Minute

// getInt64 extracts a required int64 field from a message.
func getInt64(msg exchange.Message, key string) (int64, error) {
	v, ok := msg[key]
	if !ok {
		return 0, fmt.Errorf("manager: missing required field %q", key)
	}
	n, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("manager: field %q: expected int64, got %T", key, v)
	}
	return n, nil
}

// getString extracts a string field from a message, returning an error if missing or wrong type.
func getString(msg exchange.Message, key string) (string, error) {
	v, ok := msg[key]
	if !ok {
		return "", fmt.Errorf("manager: missing field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("manager: field %q: expected string, got %T", key, v)
	}
	return s, nil
}

// getBool extracts an optional bool field from a message, returning false if absent or wrong type.
func getBool(msg exchange.Message, key string) bool {
	v, ok := msg[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// getAttachments extracts the optional "attachments" field.
// Returns nil if absent or empty.
func getAttachments(msg exchange.Message) map[string]int64 {
	v, ok := msg["attachments"]
	if !ok {
		return nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]int64, len(raw))
	for name, idAny := range raw {
		if id, ok := idAny.(int64); ok {
			result[name] = id
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// reportResult sends a succeeded or failed result back to the server.
func reportResult(ctx context.Context, result exchange.ResultSink, opID int64, err error) {
	if err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed, err.Error())
	} else {
		_ = result.SendResult(ctx, opID, exchange.StatusSucceeded, "")
	}
}

// getSnaps extracts the "snaps" list from a message.
// The server sends snaps as [{"name": "snapname", "args": {...}}, ...].
func getSnaps(msg exchange.Message) ([]map[string]any, error) {
	v, ok := msg["snaps"]
	if !ok {
		return nil, fmt.Errorf("manager: missing required field \"snaps\"")
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("manager: field \"snaps\": expected list, got %T", v)
	}
	result := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("manager: snap entry: expected dict, got %T", item)
		}
		result = append(result, m)
	}
	return result, nil
}

// InstallSnapHandler handles "install-snaps" commands.
type InstallSnapHandler struct {
	Snapd      snapd.Client
	OnComplete func() // called after all snaps have been processed; may be nil
	opCtxMgr   *OperationContextManager
}

func (h *InstallSnapHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *InstallSnapHandler) MessageType() string { return "install-snaps" }

func (h *InstallSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	snaps, err := getSnaps(msg)
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}
	for _, snap := range snaps {
		name, _ := snap["name"].(string)
		if name == "" {
			reportResult(ctx, result, opID, fmt.Errorf("manager: snap entry missing \"name\" field"))
			return nil
		}
		// Per-snap install options may be nested under an "args" key.
		var channel string
		var classic bool
		if args, ok := snap["args"].(map[string]any); ok {
			channel, _ = args["channel"].(string)
			classic, _ = args["classic"].(bool)
		}
		changeID, err := h.Snapd.InstallSnap(operationCtx, name, snapd.InstallOptions{Channel: channel, Classic: classic})
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
		waitCtx, cancel := context.WithTimeout(operationCtx, changeTimeout)
		err = h.Snapd.WaitForChange(waitCtx, changeID)
		cancel()
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
	}
	if h.OnComplete != nil {
		h.OnComplete()
	}
	reportResult(ctx, result, opID, nil)
	return nil
}

// RemoveSnapHandler handles "remove-snaps" commands.
type RemoveSnapHandler struct {
	Snapd      snapd.Client
	OnComplete func() // called after all snaps have been processed; may be nil
	opCtxMgr   *OperationContextManager
}

func (h *RemoveSnapHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *RemoveSnapHandler) MessageType() string { return "remove-snaps" }

func (h *RemoveSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	snaps, err := getSnaps(msg)
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}
	for _, snap := range snaps {
		name, _ := snap["name"].(string)
		if name == "" {
			reportResult(ctx, result, opID, fmt.Errorf("manager: snap entry missing \"name\" field"))
			return nil
		}
		changeID, err := h.Snapd.RemoveSnap(operationCtx, name)
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
		waitCtx, cancel := context.WithTimeout(operationCtx, changeTimeout)
		err = h.Snapd.WaitForChange(waitCtx, changeID)
		cancel()
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
	}
	if h.OnComplete != nil {
		h.OnComplete()
	}
	reportResult(ctx, result, opID, nil)
	return nil
}

// RefreshSnapHandler handles "refresh-snaps" commands.
type RefreshSnapHandler struct {
	Snapd      snapd.Client
	OnComplete func() // called after all snaps have been processed; may be nil
	opCtxMgr   *OperationContextManager
}

func (h *RefreshSnapHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *RefreshSnapHandler) MessageType() string { return "refresh-snaps" }

func (h *RefreshSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	snaps, err := getSnaps(msg)
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}
	for _, snap := range snaps {
		name, _ := snap["name"].(string)
		if name == "" {
			reportResult(ctx, result, opID, fmt.Errorf("manager: snap entry missing \"name\" field"))
			return nil
		}
		changeID, err := h.Snapd.RefreshSnap(operationCtx, name)
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
		waitCtx, cancel := context.WithTimeout(operationCtx, changeTimeout)
		err = h.Snapd.WaitForChange(waitCtx, changeID)
		cancel()
		if err != nil {
			reportResult(ctx, result, opID, err)
			return nil
		}
	}
	if h.OnComplete != nil {
		h.OnComplete()
	}
	reportResult(ctx, result, opID, nil)
	return nil
}

// StartServiceHandler handles "start-snap-service" commands.
type StartServiceHandler struct {
	Snapd    snapd.Client
	opCtxMgr *OperationContextManager
}

func (h *StartServiceHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *StartServiceHandler) MessageType() string { return "start-snap-service" }

func (h *StartServiceHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	snapName, err := getString(msg, "snap")
	if err != nil {
		return err
	}
	service, err := getString(msg, "service")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	reportResult(ctx, result, opID, h.Snapd.StartService(operationCtx, snapName, service))
	return nil
}

// StopServiceHandler handles "stop-snap-service" commands.
type StopServiceHandler struct {
	Snapd    snapd.Client
	opCtxMgr *OperationContextManager
}

func (h *StopServiceHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *StopServiceHandler) MessageType() string { return "stop-snap-service" }

func (h *StopServiceHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	snapName, err := getString(msg, "snap")
	if err != nil {
		return err
	}
	service, err := getString(msg, "service")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	reportResult(ctx, result, opID, h.Snapd.StopService(operationCtx, snapName, service))
	return nil
}

// RestartServiceHandler handles "restart-snap-service" commands.
type RestartServiceHandler struct {
	Snapd    snapd.Client
	opCtxMgr *OperationContextManager
}

func (h *RestartServiceHandler) SetOperationContextManager(opCtxMgr *OperationContextManager) {
	h.opCtxMgr = opCtxMgr
}

func (h *RestartServiceHandler) MessageType() string { return "restart-snap-service" }

func (h *RestartServiceHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	snapName, err := getString(msg, "snap")
	if err != nil {
		return err
	}
	service, err := getString(msg, "service")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	if h.opCtxMgr != nil {
		h.opCtxMgr.Register(opID, cancelOperation)
		defer h.opCtxMgr.Cleanup(opID)
	}

	reportResult(ctx, result, opID, h.Snapd.RestartService(operationCtx, snapName, service))
	return nil
}
