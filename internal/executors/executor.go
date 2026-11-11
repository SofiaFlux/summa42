package executors

import (
	"context"
	"encoding/json"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type Executor interface {
	Start(ctx context.Context, envelope AttemptEnvelope) (ExecutionResult, error)
}

type AttemptEnvelope struct {
	TaskID              domain.ID
	AttemptID           domain.ID
	Objective           string
	PayloadJSON         json.RawMessage
	AcceptanceCriteria  []string
	Workspace           string
	VisibleCapabilities []string
	ResourceEnvelopeID  domain.ID
}

type EvidenceKind string

const (
	EvidenceStdout       EvidenceKind = "STDOUT"
	EvidenceStderr       EvidenceKind = "STDERR"
	EvidenceCommand      EvidenceKind = "COMMAND"
	EvidenceFileChange   EvidenceKind = "FILE_CHANGE"
	EvidenceAgentMessage EvidenceKind = "AGENT_MESSAGE"
)

type Evidence struct {
	Kind    EvidenceKind
	Content string
}

type Usage struct {
	// Reported distinguishes an explicit provider usage report from unknown counts.
	Reported              bool
	WallTime              time.Duration
	InputTokens           int64
	CachedInputTokens     int64
	CacheWriteInputTokens int64
	OutputTokens          int64
	ReasoningOutputTokens int64
}

type ExecutionResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Evidence []Evidence
	Usage    Usage
}
