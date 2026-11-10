package dtu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrAckDropped is returned when a remote operation executed successfully,
// but the transport acknowledgement or response was dropped or lost in transit.
var ErrAckDropped = errors.New("remote execution succeeded but acknowledgement was dropped/lost in transit")

// HostileAckLossConfig configures hostile twin behavior for simulating acknowledgement loss.
type HostileAckLossConfig struct {
	DropAckActions map[string]bool `json:"drop_ack_actions"`
	DropAckKeys    map[string]bool `json:"drop_ack_keys"`
}

// RemoteEffectState tracks the server-side applied state of an operation within the hostile twin.
type RemoteEffectState struct {
	Key            string         `json:"key"`
	Action         string         `json:"action"`
	ExecutionCount int            `json:"execution_count"`
	Payload        map[string]any `json:"payload,omitempty"`
	Status         string         `json:"status"`
}

// HostileAckLossTwin is an EnvironmentProvider that emulates remote services with dropped acknowledgements.
type HostileAckLossTwin struct {
	mu           sync.RWMutex
	meta         ProviderMetadata
	cfg          HostileAckLossConfig
	seed         int64
	effects      map[string]*RemoteEffectState
	droppedCount int
}

// NewHostileAckLossTwin creates a new HostileAckLossTwin provider.
func NewHostileAckLossTwin(meta ProviderMetadata, cfg HostileAckLossConfig) *HostileAckLossTwin {
	if !meta.Fidelity.Valid() {
		meta.Fidelity = FidelityT2Adversarial
	}
	cfgCopy := HostileAckLossConfig{
		DropAckActions: cloneBoolMap(cfg.DropAckActions),
		DropAckKeys:    cloneBoolMap(cfg.DropAckKeys),
	}
	return &HostileAckLossTwin{
		meta:    cloneMetadata(meta),
		cfg:     cfgCopy,
		effects: make(map[string]*RemoteEffectState),
	}
}

func (t *HostileAckLossTwin) Metadata() ProviderMetadata {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return cloneMetadata(t.meta)
}

func (t *HostileAckLossTwin) Mode() EnvironmentMode {
	return EnvModeTwin
}

// Seed returns the deterministic seed set during Reset.
func (t *HostileAckLossTwin) Seed() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.seed
}

func (t *HostileAckLossTwin) Reset(ctx context.Context, seed int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seed = seed
	t.effects = make(map[string]*RemoteEffectState)
	t.droppedCount = 0
	return nil
}

type hostileTwinSnapshot struct {
	Effects      map[string]*RemoteEffectState `json:"effects"`
	DroppedCount int                           `json:"dropped_count"`
	Seed         int64                         `json:"seed"`
}

func (t *HostileAckLossTwin) Snapshot(ctx context.Context) ([]byte, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	effectsCopy := make(map[string]*RemoteEffectState, len(t.effects))
	for k, v := range t.effects {
		if v == nil {
			continue
		}
		effectsCopy[k] = &RemoteEffectState{
			Key:            v.Key,
			Action:         v.Action,
			ExecutionCount: v.ExecutionCount,
			Payload:        cloneAnyMap(v.Payload),
			Status:         v.Status,
		}
	}

	snap := hostileTwinSnapshot{
		Effects:      effectsCopy,
		DroppedCount: t.droppedCount,
		Seed:         t.seed,
	}
	return json.Marshal(snap)
}

func (t *HostileAckLossTwin) Restore(ctx context.Context, snapshot []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var snap hostileTwinSnapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return fmt.Errorf("failed to restore hostile twin snapshot: %w", err)
	}

	sanitizedEffects := make(map[string]*RemoteEffectState, len(snap.Effects))
	for k, v := range snap.Effects {
		if v != nil {
			sanitizedEffects[k] = &RemoteEffectState{
				Key:            v.Key,
				Action:         v.Action,
				ExecutionCount: v.ExecutionCount,
				Payload:        cloneAnyMap(v.Payload),
				Status:         v.Status,
			}
		}
	}

	t.effects = sanitizedEffects
	t.droppedCount = snap.DroppedCount
	t.seed = snap.Seed
	return nil
}

func (t *HostileAckLossTwin) Execute(ctx context.Context, req InvocationRequest) (InvocationResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Handle explicit reconciliation queries
	if req.Action == "reconcile" {
		targetKey, _ := req.Payload["target_key"].(string)
		if targetKey == "" {
			targetKey = req.IdempotencyKey
		}
		effect, exists := t.effects[targetKey]
		if !exists {
			return InvocationResponse{
				Success:    true,
				StatusCode: 200,
				Data: map[string]any{
					"status":          "NOT_FOUND",
					"execution_count": 0,
					"key":             targetKey,
				},
			}, nil
		}

		return InvocationResponse{
			Success:    true,
			StatusCode: 200,
			Data: map[string]any{
				"status":          effect.Status,
				"execution_count": effect.ExecutionCount,
				"key":             targetKey,
			},
		}, nil
	}

	// Normal action: stateful execution
	key := req.IdempotencyKey
	if key == "" {
		key = req.Action
	}

	effect, exists := t.effects[key]
	if !exists {
		effect = &RemoteEffectState{
			Key:            key,
			Action:         req.Action,
			ExecutionCount: 0,
			Payload:        cloneAnyMap(req.Payload),
			Status:         "APPLIED",
		}
		t.effects[key] = effect
	}
	effect.ExecutionCount++

	// Check if ACK should be dropped
	drop := false
	if t.cfg.DropAckActions != nil && t.cfg.DropAckActions[req.Action] {
		drop = true
	}
	if t.cfg.DropAckKeys != nil && t.cfg.DropAckKeys[key] {
		drop = true
	}

	if drop {
		t.droppedCount++
		return InvocationResponse{
			Success: false,
			Error:   "acknowledgement dropped in transit",
		}, ErrAckDropped
	}

	return InvocationResponse{
		Success:    true,
		StatusCode: 200,
		Data: map[string]any{
			"status":          effect.Status,
			"execution_count": effect.ExecutionCount,
			"key":             key,
		},
	}, nil
}

func (t *HostileAckLossTwin) ExecutionCount(key string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if effect, exists := t.effects[key]; exists {
		return effect.ExecutionCount
	}
	return 0
}

func (t *HostileAckLossTwin) DroppedCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.droppedCount
}

func cloneBoolMap(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
