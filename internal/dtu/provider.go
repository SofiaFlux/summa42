package dtu

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// EnvironmentProvider abstracts an execution environment backend (LIVE or TWIN).
type EnvironmentProvider interface {
	Metadata() ProviderMetadata
	Mode() EnvironmentMode
	Reset(ctx context.Context, seed int64) error
	Snapshot(ctx context.Context) ([]byte, error)
	Restore(ctx context.Context, snapshot []byte) error
	Execute(ctx context.Context, req InvocationRequest) (InvocationResponse, error)
}

// MockEnvironmentProvider is a thread-safe in-memory provider for testing and scenario emulation.
type MockEnvironmentProvider struct {
	mu          sync.RWMutex
	meta        ProviderMetadata
	mode        EnvironmentMode
	seed        int64
	responses   map[string]InvocationResponse
	faults      map[string]string
	callHistory []InvocationRequest
}

type mockSnapshotState struct {
	Responses   map[string]InvocationResponse `json:"responses"`
	Faults      map[string]string             `json:"faults"`
	CallHistory []InvocationRequest           `json:"call_history"`
	Seed        int64                         `json:"seed"`
}

// NewMockEnvironmentProvider creates a new MockEnvironmentProvider with validated fidelity.
func NewMockEnvironmentProvider(meta ProviderMetadata, mode EnvironmentMode) *MockEnvironmentProvider {
	if !meta.Fidelity.Valid() {
		meta.Fidelity = FidelityT1Behavioral
	}
	return &MockEnvironmentProvider{
		meta:        cloneMetadata(meta),
		mode:        mode,
		responses:   make(map[string]InvocationResponse),
		faults:      make(map[string]string),
		callHistory: make([]InvocationRequest, 0),
	}
}

func (p *MockEnvironmentProvider) Metadata() ProviderMetadata {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cloneMetadata(p.meta)
}

func (p *MockEnvironmentProvider) Mode() EnvironmentMode {
	return p.mode
}

// Seed returns the deterministic seed set during Reset.
func (p *MockEnvironmentProvider) Seed() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.seed
}

// SetFault configures an action to fail with the given error.
func (p *MockEnvironmentProvider) SetFault(action string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.faults, action)
	} else {
		p.faults[action] = err.Error()
	}
}

// SetResponse configures a canned response for an action with deep copying.
func (p *MockEnvironmentProvider) SetResponse(action string, resp InvocationResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responses[action] = cloneResponse(resp)
}

// Reset clears transient state and stores the deterministic seed.
func (p *MockEnvironmentProvider) Reset(ctx context.Context, seed int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seed = seed
	p.faults = make(map[string]string)
	p.callHistory = make([]InvocationRequest, 0)
	return nil
}

// Snapshot serializes internal mock state into JSON bytes.
func (p *MockEnvironmentProvider) Snapshot(ctx context.Context) ([]byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state := mockSnapshotState{
		Responses:   cloneResponsesMap(p.responses),
		Faults:      cloneStringMap(p.faults),
		CallHistory: cloneRequestsSlice(p.callHistory),
		Seed:        p.seed,
	}
	return json.Marshal(state)
}

// Restore deserializes internal mock state from JSON bytes.
func (p *MockEnvironmentProvider) Restore(ctx context.Context, snapshot []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var state mockSnapshotState
	if err := json.Unmarshal(snapshot, &state); err != nil {
		return fmt.Errorf("failed to restore snapshot: %w", err)
	}

	p.responses = cloneResponsesMap(state.Responses)
	p.faults = cloneStringMap(state.Faults)
	p.callHistory = cloneRequestsSlice(state.CallHistory)
	p.seed = state.Seed
	return nil
}

// Execute performs an invocation against the mock provider with defensive copying.
func (p *MockEnvironmentProvider) Execute(ctx context.Context, req InvocationRequest) (InvocationResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.callHistory = append(p.callHistory, cloneRequest(req))

	// Check if fault injected
	if faultMsg, hasFault := p.faults[req.Action]; hasFault {
		return InvocationResponse{
			Success: false,
			Error:   faultMsg,
		}, fmt.Errorf("injected fault on %s: %s", req.Action, faultMsg)
	}

	// Check if canned response registered
	if resp, hasResp := p.responses[req.Action]; hasResp {
		return cloneResponse(resp), nil
	}

	// Default success response
	return InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data: map[string]any{
			"status": "ok",
			"action": req.Action,
		},
	}, nil
}

func cloneMetadata(m ProviderMetadata) ProviderMetadata {
	cp := m
	if m.KnownDivergences != nil {
		cp.KnownDivergences = make([]string, len(m.KnownDivergences))
		copy(cp.KnownDivergences, m.KnownDivergences)
	}
	if m.SupportedProtocols != nil {
		cp.SupportedProtocols = make([]string, len(m.SupportedProtocols))
		copy(cp.SupportedProtocols, m.SupportedProtocols)
	}
	if m.Attributes != nil {
		cp.Attributes = cloneStringMap(m.Attributes)
	}
	return cp
}

func cloneRequest(r InvocationRequest) InvocationRequest {
	cp := r
	if r.Payload != nil {
		cp.Payload = cloneAnyMap(r.Payload)
	}
	if r.Headers != nil {
		cp.Headers = cloneStringMap(r.Headers)
	}
	return cp
}

func cloneResponse(r InvocationResponse) InvocationResponse {
	cp := r
	if r.Data != nil {
		cp.Data = cloneAnyMap(r.Data)
	}
	if r.Headers != nil {
		cp.Headers = cloneStringMap(r.Headers)
	}
	if r.RawBody != nil {
		cp.RawBody = make([]byte, len(r.RawBody))
		copy(cp.RawBody, r.RawBody)
	}
	return cp
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func cloneResponsesMap(m map[string]InvocationResponse) map[string]InvocationResponse {
	if m == nil {
		return make(map[string]InvocationResponse)
	}
	cp := make(map[string]InvocationResponse, len(m))
	for k, v := range m {
		cp[k] = cloneResponse(v)
	}
	return cp
}

func cloneRequestsSlice(s []InvocationRequest) []InvocationRequest {
	if s == nil {
		return make([]InvocationRequest, 0)
	}
	cp := make([]InvocationRequest, len(s))
	for i, v := range s {
		cp[i] = cloneRequest(v)
	}
	return cp
}
