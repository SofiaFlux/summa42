package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
)

const (
	ExecutorKind       = "github-issue-triage"
	TaskClass          = "github.issue.triage"
	RequiredCapability = "github.issue.read"
	DecisionMediaType  = "application/json"

	// stage2Attempts bounds the classifier to one retry inside a single lease.
	// An unbounded retry turns one poisoned issue into an infinite budget burn.
	stage2Attempts = 2

	// snapshotEvidenceKind is the kind ghissue.observe writes the canonical
	// issue snapshot under, and the only kind this executor reads. A decision
	// cites the snapshot it was derived from, so the cited object has to be one.
	snapshotEvidenceKind = "github.issue.snapshot"
)

type triagePayload struct {
	Repository         string    `json:"repository"`
	Issue              int64     `json:"issue"`
	Revision           string    `json:"revision"`
	SnapshotEvidenceID domain.ID `json:"issueSnapshot"`
}

type Executor struct {
	evidence *evidence.Store
	model    ClassifierModel
}

func NewExecutor(store *evidence.Store, model ClassifierModel) *Executor {
	return &Executor{evidence: store, model: model}
}

// Start runs the three deciding stages inside one lease and returns exactly
// one decision document. A stage 1 that resolves the issue never consults the
// model. A model response that does not validate is retried once with the same
// prepared input, and a second failure fails the attempt without fabricating a
// decision, because the worker treats any returning executor's evidence as a
// completed attempt.
func (e *Executor) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	if e == nil || e.evidence == nil || e.model == nil {
		return executors.ExecutionResult{}, errors.New("triage executor is not configured")
	}
	payload, err := decodePayload(envelope.PayloadJSON)
	if err != nil {
		return executors.ExecutionResult{}, err
	}
	snap, err := e.loadSnapshot(ctx, payload)
	if err != nil {
		return executors.ExecutionResult{}, err
	}

	decision := Decision{
		Schema:                  DecisionSchema,
		Repository:              payload.Repository,
		Issue:                   payload.Issue,
		Revision:                payload.Revision,
		SnapshotEvidenceID:      string(payload.SnapshotEvidenceID),
		TriageRulesVersion:      TriageRulesVersion,
		DispositionRulesVersion: DispositionRulesVersion,
		Stage1:                  Stage1(snap),
	}

	if !decision.Stage1.Resolved() {
		out, err := e.classify(ctx, BuildStage2Input(snap, decision.Stage1.Signals))
		if err != nil {
			return executors.ExecutionResult{}, err
		}
		stage3 := Stage3(out)
		decision.Stage2 = &out
		decision.Stage3 = &stage3
	}

	if err := decision.Validate(); err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("validate decision: %w", err)
	}
	raw, err := decision.Canonical()
	if err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("encode decision: %w", err)
	}
	return executors.ExecutionResult{
		Evidence: []executors.Evidence{{
			Kind:    executors.EvidenceKind(KindDecision),
			Content: string(raw),
		}},
		Stderr: fmt.Sprintf("triage %s#%d at %s: %s via %s\n",
			payload.Repository, payload.Issue, payload.Revision,
			decision.FinalDisposition(), decisionRule(decision)),
	}, nil
}

func (e *Executor) classify(ctx context.Context, input Stage2Input) (Stage2Output, error) {
	var lastErr error
	for attempt := 0; attempt < stage2Attempts; attempt++ {
		out, err := e.model.Classify(ctx, input)
		if err == nil {
			raw, marshalErr := json.Marshal(out)
			if marshalErr != nil {
				lastErr = marshalErr
				continue
			}
			parsed, parseErr := ParseStage2Output(raw)
			if parseErr == nil {
				return parsed, nil
			}
			lastErr = parseErr
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Stage2Output{}, fmt.Errorf("stage 2 aborted, lease %s: %w", ctxErr, err)
		}
		lastErr = err
	}
	return Stage2Output{}, fmt.Errorf(
		"stage 2 produced no conforming response after %d attempts: %w", stage2Attempts, lastErr)
}

// loadSnapshot reads the one evidence object the payload cites and proves it is
// that object: the right kind, canonical, and about the issue the task names.
// The decision's repository and issue are copied from the payload, not from the
// snapshot, so a snapshot of some other issue would otherwise produce a decision
// whose own fields contradict the snapshot evidence id it cites.
func (e *Executor) loadSnapshot(ctx context.Context, payload triagePayload) (Snapshot, error) {
	id := payload.SnapshotEvidenceID
	object, raw, err := e.evidence.Get(ctx, id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load issue snapshot %s: %w", id, err)
	}
	if object.Kind != snapshotEvidenceKind {
		return Snapshot{}, fmt.Errorf("evidence %s is a %q, not an issue snapshot", id, object.Kind)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode issue snapshot %s: %w", id, err)
	}
	if snap.Repo == "" || snap.Issue <= 0 {
		return Snapshot{}, fmt.Errorf("evidence %s is not a canonical issue snapshot", id)
	}
	if snap.Repo != payload.Repository || snap.Issue != payload.Issue {
		return Snapshot{}, fmt.Errorf("evidence %s is a snapshot of %s#%d, but the task payload names %s#%d",
			id, snap.Repo, snap.Issue, payload.Repository, payload.Issue)
	}
	return snap, nil
}

func decodePayload(raw json.RawMessage) (triagePayload, error) {
	var payload triagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return triagePayload{}, fmt.Errorf("decode triage task payload: %w", err)
	}
	if strings.TrimSpace(payload.Repository) == "" || payload.Issue <= 0 {
		return triagePayload{}, errors.New("triage task payload lacks a repository or issue number")
	}
	if strings.TrimSpace(payload.Revision) == "" {
		return triagePayload{}, errors.New("triage task payload lacks a revision")
	}
	if strings.TrimSpace(string(payload.SnapshotEvidenceID)) == "" {
		return triagePayload{}, errors.New("triage task payload lacks the issue snapshot evidence id")
	}
	return payload, nil
}

func decisionRule(d Decision) string {
	if d.Stage1.Resolved() {
		return d.Stage1.Rule
	}
	if d.Stage3 != nil {
		return d.Stage3.Rule
	}
	return ""
}
