package capacity

import (
	"context"
	"time"
)

// Outcome is how one model call ended, as the ledger records it.
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeRefused Outcome = "refused"
	OutcomeEmpty   Outcome = "empty"
	OutcomeTimeout Outcome = "timeout"
	OutcomeError   Outcome = "error"
	OutcomeGated   Outcome = "gated"
)

// Call is one row of the usage ledger.
type Call struct {
	Pool         string
	Harness      string
	Model        string
	Stage        string
	CaseID       string
	StartedAt    time.Time
	Duration     time.Duration
	Outcome      Outcome
	InputTokens  *int // nil when the CLI does not report tokens
	OutputTokens *int
	ResetAt      time.Time // zero when the reset time is unknown
	ResetSource  string    // "parsed", "default", or empty
	Message      string    // refusal excerpt, at most 300 bytes
}

// PoolState says whether a pool can take calls right now.
type PoolState struct {
	Pool string
	// Exhausted: the provider refused until ResetAt (a limit).
	Exhausted bool
	// Degraded: repeated timeouts recently, so the pool is not answering (a health
	// signal, not a limit). Routing treats it like an exhausted pool.
	Degraded bool
	ResetAt  time.Time
}

// Unavailable reports whether a pool should not be called right now.
func (s PoolState) Unavailable() bool { return s.Exhausted || s.Degraded }

// Ledger stores calls and answers whether a pool is exhausted.
type Ledger interface {
	RecordCall(ctx context.Context, c Call) error
	PoolState(ctx context.Context, pool string, now time.Time) (PoolState, error)
}
