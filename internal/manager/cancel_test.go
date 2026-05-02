package manager

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/canonical/landscape-client-core/internal/exchange"
)

func TestOperationContextManager_CancelCallsRegisteredFunc(t *testing.T) {
	mgr := NewOperationContextManager()

	cancelled := make(chan struct{}, 1)
	mgr.Register(42, func() {
		cancelled <- struct{}{}
	})

	mgr.Cancel(42)

	select {
	case <-cancelled:
	case <-time.After(1 * time.Second):
		t.Fatal("cancel func was not invoked")
	}

	mgr.Cleanup(42)
	mgr.mu.Lock()
	_, ok := mgr.operations[42]
	mgr.mu.Unlock()
	if ok {
		t.Fatal("operation should have been cleaned up")
	}
}

func TestOperationContextManager_ConcurrentCancelMultipleOps(t *testing.T) {
	mgr := NewOperationContextManager()

	const total = 64
	var wg sync.WaitGroup
	wg.Add(total)

	for i := 0; i < total; i++ {
		opID := int64(i + 1)
		mgr.Register(opID, wg.Done)
	}

	for i := 0; i < total; i++ {
		opID := int64(i + 1)
		go mgr.Cancel(opID)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected all %d operations to be cancelled", total)
	}
}

func TestScriptExecHandler_OperationCancellation(t *testing.T) {
	mgr := NewOperationContextManager()
	sink := &mockResultSink{}
	h := NewScriptExecHandler(t.TempDir(), nil)
	h.SetOperationContextManager(mgr)

	msg := exchange.Message{
		"operation-id": int64(5001),
		"code":         "while true; do :; done",
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- h.Handle(context.Background(), msg, sink)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mgr.mu.Lock()
		_, ok := mgr.operations[5001]
		mgr.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mgr.Cancel(5001)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Handle returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("script execution did not return after cancellation")
	}

	res, ok := sink.first()
	if !ok {
		t.Fatal("expected a result after cancellation")
	}
	if res.status != exchange.StatusFailed {
		t.Fatalf("status = %d, want %d", res.status, exchange.StatusFailed)
	}
}
