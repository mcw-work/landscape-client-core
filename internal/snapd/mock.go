package snapd

import (
	"context"
	"fmt"
	"sync"
)

// MockClient implements Client with configurable return values.
// All fields are public for easy setup in tests.
type MockClient struct {
	mu             sync.Mutex
	Snaps          []SnapInfo
	Services       []ServiceInfo
	ChangeID       string // returned by Install/Remove/Refresh
	ChangeErr      error  // error returned by WaitForChange
	AssertionsData *Assertions
	RebootRequired bool
	InstallCalls   []string // snap names passed to InstallSnap
	RemoveCalls    []string
	RefreshCalls   []string
	ServiceActions []string // "start:snap.svc", "stop:snap.svc", "restart:snap.svc"
	Err            error    // if non-nil, all methods return this error
}

func (m *MockClient) ListSnaps(_ context.Context) ([]SnapInfo, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Snaps, nil
}

func (m *MockClient) ListServices(_ context.Context) ([]ServiceInfo, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Services, nil
}

func (m *MockClient) InstallSnap(_ context.Context, name string, _ InstallOptions) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	m.mu.Lock()
	m.InstallCalls = append(m.InstallCalls, name)
	m.mu.Unlock()
	return m.ChangeID, nil
}

func (m *MockClient) RemoveSnap(_ context.Context, name string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	m.mu.Lock()
	m.RemoveCalls = append(m.RemoveCalls, name)
	m.mu.Unlock()
	return m.ChangeID, nil
}

func (m *MockClient) RefreshSnap(_ context.Context, name string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	m.mu.Lock()
	m.RefreshCalls = append(m.RefreshCalls, name)
	m.mu.Unlock()
	return m.ChangeID, nil
}

func (m *MockClient) StartService(_ context.Context, snapName, serviceName string) error {
	if m.Err != nil {
		return m.Err
	}
	m.mu.Lock()
	m.ServiceActions = append(m.ServiceActions, fmt.Sprintf("start:%s.%s", snapName, serviceName))
	m.mu.Unlock()
	return nil
}

func (m *MockClient) StopService(_ context.Context, snapName, serviceName string) error {
	if m.Err != nil {
		return m.Err
	}
	m.mu.Lock()
	m.ServiceActions = append(m.ServiceActions, fmt.Sprintf("stop:%s.%s", snapName, serviceName))
	m.mu.Unlock()
	return nil
}

func (m *MockClient) RestartService(_ context.Context, snapName, serviceName string) error {
	if m.Err != nil {
		return m.Err
	}
	m.mu.Lock()
	m.ServiceActions = append(m.ServiceActions, fmt.Sprintf("restart:%s.%s", snapName, serviceName))
	m.mu.Unlock()
	return nil
}

func (m *MockClient) WaitForChange(_ context.Context, _ string) error {
	return m.ChangeErr
}

func (m *MockClient) GetAssertions(_ context.Context) (*Assertions, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.AssertionsData, nil
}

func (m *MockClient) GetRebootRequired(_ context.Context) (bool, error) {
	if m.Err != nil {
		return false, m.Err
	}
	return m.RebootRequired, nil
}
