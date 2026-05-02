package manager

import (
	"context"
	"sync"
)

// OperationContextManager tracks cancel functions for in-flight operations.
type OperationContextManager struct {
	mu         sync.Mutex
	operations map[int64]context.CancelFunc
}

// NewOperationContextManager creates an empty operation cancellation registry.
func NewOperationContextManager() *OperationContextManager {
	return &OperationContextManager{operations: make(map[int64]context.CancelFunc)}
}

// Register stores a cancel function for an operation, replacing any existing one.
func (m *OperationContextManager) Register(operationID int64, cancel context.CancelFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations[operationID] = cancel
}

// Cancel triggers cancellation for an operation if it is registered.
func (m *OperationContextManager) Cancel(operationID int64) {
	m.mu.Lock()
	cancel, ok := m.operations[operationID]
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

// Cleanup removes an operation from the registry.
func (m *OperationContextManager) Cleanup(operationID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.operations, operationID)
}package manager

import (
	"context"
	"sync"
)

// OperationContextManager tracks cancellable operation contexts by operation ID.
type OperationContextManager struct {
	mu         sync.Mutex
	operations map[int64]context.CancelFunc
}

// NewOperationContextManager creates an empty operation cancellation registry.
func NewOperationContextManager() *OperationContextManager {
	return &OperationContextManager{
		operations: make(map[int64]context.CancelFunc),
	}
}

// Register stores a cancel function for an operation ID.
func (m *OperationContextManager) Register(opID int64, cancel context.CancelFunc) {
	if cancel == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.operations[opID] = cancel
}

// Cancel cancels a registered operation if it exists.
func (m *OperationContextManager) Cancel(opID int64) {
	m.mu.Lock()
	cancel, ok := m.operations[opID]
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

// Cleanup removes an operation from the registry.
func (m *OperationContextManager) Cleanup(opID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.operations, opID)
}
