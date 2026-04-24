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

// reportResult sends a succeeded or failed result back to the server.
func reportResult(ctx context.Context, result exchange.ResultSink, opID int64, err error) {
	if err != nil {
		_ = result.SendResult(ctx, opID, exchange.StatusFailed, err.Error())
	} else {
		_ = result.SendResult(ctx, opID, exchange.StatusSucceeded, "")
	}
}

// InstallSnapHandler handles "install-snap" commands.
type InstallSnapHandler struct {
	Snapd snapd.Client
}

func (h *InstallSnapHandler) MessageType() string { return "install-snap" }

func (h *InstallSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	name, err := getString(msg, "snap-name")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}
	channel, _ := getString(msg, "channel")
	classic := getBool(msg, "classic")

	changeID, err := h.Snapd.InstallSnap(ctx, name, snapd.InstallOptions{Channel: channel, Classic: classic})
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()

	reportResult(ctx, result, opID, h.Snapd.WaitForChange(waitCtx, changeID))
	return nil
}

// RemoveSnapHandler handles "remove-snap" commands.
type RemoveSnapHandler struct {
	Snapd snapd.Client
}

func (h *RemoveSnapHandler) MessageType() string { return "remove-snap" }

func (h *RemoveSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	name, err := getString(msg, "snap-name")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	changeID, err := h.Snapd.RemoveSnap(ctx, name)
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()

	reportResult(ctx, result, opID, h.Snapd.WaitForChange(waitCtx, changeID))
	return nil
}

// RefreshSnapHandler handles "refresh-snap" commands.
type RefreshSnapHandler struct {
	Snapd snapd.Client
}

func (h *RefreshSnapHandler) MessageType() string { return "refresh-snap" }

func (h *RefreshSnapHandler) Handle(ctx context.Context, msg exchange.Message, result exchange.ResultSink) error {
	name, err := getString(msg, "snap-name")
	if err != nil {
		return err
	}
	opID, err := getInt64(msg, "operation-id")
	if err != nil {
		return err
	}

	changeID, err := h.Snapd.RefreshSnap(ctx, name)
	if err != nil {
		reportResult(ctx, result, opID, err)
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, changeTimeout)
	defer cancel()

	reportResult(ctx, result, opID, h.Snapd.WaitForChange(waitCtx, changeID))
	return nil
}

// StartServiceHandler handles "start-snap-service" commands.
type StartServiceHandler struct {
	Snapd snapd.Client
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

	reportResult(ctx, result, opID, h.Snapd.StartService(ctx, snapName, service))
	return nil
}

// StopServiceHandler handles "stop-snap-service" commands.
type StopServiceHandler struct {
	Snapd snapd.Client
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

	reportResult(ctx, result, opID, h.Snapd.StopService(ctx, snapName, service))
	return nil
}

// RestartServiceHandler handles "restart-snap-service" commands.
type RestartServiceHandler struct {
	Snapd snapd.Client
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

	reportResult(ctx, result, opID, h.Snapd.RestartService(ctx, snapName, service))
	return nil
}
