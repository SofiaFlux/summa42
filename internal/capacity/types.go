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
	Pool      string
	Exhausted bool
	ResetAt   time.Time
}

// Ledger stores calls and answers whether a pool is exhausted.
type Ledger interface {
	RecordCall(ctx context.Context, c Call) error
	PoolState(ctx context.Context, pool string, now time.Time) (PoolState, error)
}
