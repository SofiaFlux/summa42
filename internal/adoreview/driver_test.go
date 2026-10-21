package adoreview

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/adoeffects"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type fakeLookupProvider struct {
	name     string
	outcome  domain.OperationState
	requests []operations.ProviderDispatchRequest
}

type fakeCompletions struct {
	provenance runmanifest.Provenance
	err        error
}

func (f *fakeCompletions) Provenance(context.Context, domain.ID) (runmanifest.Provenance, error) {
	return f.provenance, f.err
}

type blockingDriverCaller struct {
	started chan struct{}
}

func (c *blockingDriverCaller) Call(ctx context.Context, _ string, _ any) (any, error) {
	close(c.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeLookupProvider) Name() string { return f.name }

func (f *fakeLookupProvider) LookupOutcome(_ context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	f.requests = append(f.requests, request)
	return operations.ProviderOutcome{State: f.outcome}, nil
}

type driverHarness struct {
	ctx            context.Context
	store          *state.Store
	cases          *workflowcase.Service
	execution      *execution.Service
	evidence       *evidence.Store
	verification   *verification.Service
	clock          *testutil.Clock
	caller         *fakeCaller
	comment        *fakeLookupProvider
	vote           *fakeLookupProvider
	driver         *Driver
	mission        domain.ID
	driverCaseID   domain.ID
	decision       domain.ID
	work1Task      domain.Task
	work1Attempt   domain.Attempt
	reviewEvidence evidence.EvidenceObject
}

func setupDriverHarness(t *testing.T, decision ReviewDecision) *driverHarness {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	manifests := runmanifest.New(store, runmanifest.StaticContext{
		Build:      runmanifest.BuildMetadata{RuntimeVersion: "v-test", RuntimeCommit: "commit-test"},
		TEBProfile: teb.PartialProfile("driver-test", teb.GuaranteeNonRoot),
	})
	execSvc := execution.New(store, clk, purposes, manifests)
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verificationSvc := verification.New(store, clk, execSvc)
	cases := workflowcase.New(store, clk, purposes)
	mission, err := purposes.CreateMission(ctx, "drive ADO publication")
	if err != nil {
		t.Fatal(err)
	}
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, clk.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	comment := &fakeLookupProvider{name: commentProviderName, outcome: domain.OperationConfirmedEffect}
	vote := &fakeLookupProvider{name: voteProviderName, outcome: domain.OperationConfirmedEffect}
	pages := make([]any, 32)
	for index := range pages {
		pages[index] = map[string]any{"project": "proj"}
	}
	caller := &fakeCaller{pages: pages}
	driver, err := NewDriver(cases, execSvc, evidenceStore, manifests, DriverConfig{
		MissionID: mission, ResourceEnvelopeID: envelope, Comment: comment, Vote: vote, Caller: caller,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &driverHarness{
		ctx: ctx, store: store, cases: cases, execution: execSvc, evidence: evidenceStore,
		verification: verificationSvc, clock: clk, caller: caller, comment: comment, vote: vote, driver: driver,
		mission: mission,
	}
	grant := workflow.Grant{
		Capabilities: []string{"read", effectCommentCapability, effectApproveCapability},
		Actions:      []string{effectCommentCapability, effectApproveCapability},
	}
	initial, err := cases.Ensure(ctx, workflowcase.Observation{
		MissionID: mission, Source: "ado", ObjectID: "shop#1", RevisionID: "a:b", EvidenceID: "ev-review-work-1",
		FirstWork: workflow.WorkProposal{Kind: "ado.pr.review", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:     grant, MaxSteps: 3, RemainingBudget: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	work1Payload, err := json.Marshal(map[string]any{"repo": "shop", "pr": 1, "sourceCommit": "a", "targetCommit": "b"})
	if err != nil {
		t.Fatal(err)
	}
	work1, err := cases.MaterializeTask(ctx, execSvc, initial.ID, initial.CurrentWorkID, execution.TaskRequest{
		Objective: "Review ADO PR shop#1", PayloadJSON: work1Payload,
		AcceptanceCriteria: []string{"review evidence recorded for a:b"}, ResourceEnvelopeID: envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewEvidence := putDriverEvidence(t, evidenceStore, "review completed", "text/plain", executors.EvidenceAgentMessage)
	work1Attempt := completeDriverTask(t, h, work1, reviewEvidence.ID)
	h.work1Task = work1
	h.work1Attempt = work1Attempt
	h.reviewEvidence = reviewEvidence
	h.decision = putDecision(t, evidenceStore, ctx, decision)
	capability := effectApproveCapability
	if decision.Action == DecisionCommentAction {
		capability = effectCommentCapability
	}
	result, err := cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: initial.ID, WorkID: initial.CurrentWorkID, RemainingBudget: 9, ProgressSignature: "review-complete",
		Assessment: workflow.Assessment{
			Verdict: workflow.Continue, EvidenceIDs: []string{string(reviewEvidence.ID), string(h.decision)},
			Next: &workflow.WorkProposal{
				Kind: "publish-decision", RequiredCapabilities: []string{capability},
				AuthorityCeiling: []string{capability}, ProposedActions: []string{capability},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.driverCaseID = result.Case.ID
	return h
}

func putDriverEvidence(t *testing.T, store *evidence.Store, content, mediaType string, kind executors.EvidenceKind) evidence.EvidenceObject {
	t.Helper()
	object, err := store.Put(t.Context(), strings.NewReader(content), evidence.Metadata{MediaType: mediaType, Kind: string(kind)})
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func completeDriverTask(t *testing.T, h *driverHarness, task domain.Task, evidenceID domain.ID) domain.Attempt {
	t.Helper()
	attempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.verification.CompleteAttempt(h.ctx, attempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{evidenceID}}); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func materializeDriverWork2(t *testing.T, h *driverHarness, c workflowcase.Case) domain.Task {
	t.Helper()
	if _, err := h.driver.StepOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	task, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("Work 2 task for %s was not materialized", c.CurrentWorkID)
	}
	return task
}

func driverPublisherContent(t *testing.T, entries ...publishEvidenceEntry) string {
	t.Helper()
	result, err := publishExecutionResult(entries)
	if err != nil {
		t.Fatal(err)
	}
	return result.Evidence[0].Content
}

func setDriverCompletionEvidence(t *testing.T, h *driverHarness, ids ...domain.ID) {
	t.Helper()
	body, err := json.Marshal(struct {
		Version     int         `json:"version"`
		EvidenceIDs []domain.ID `json:"evidence_ids"`
	}{Version: 1, EvidenceIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	if _, err := h.store.DB().ExecContext(h.ctx,
		`UPDATE attempt_completion_records SET manifest_hash = ?, manifest_json = ? WHERE attempt_id = ?`,
		hex.EncodeToString(digest[:]), string(body), h.work1Attempt.ID,
	); err != nil {
		t.Fatal(err)
	}
}

func setDriverAssessmentEvidence(t *testing.T, h *driverHarness, ids ...domain.ID) {
	t.Helper()
	records, err := h.cases.ListAssessments(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.WorkID != string(h.work1Task.IdempotencyKey) {
			continue
		}
		var request workflowcase.AssessmentRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
			t.Fatal(err)
		}
		request.Assessment.EvidenceIDs = make([]string, 0, len(ids))
		for _, id := range ids {
			request.Assessment.EvidenceIDs = append(request.Assessment.EvidenceIDs, string(id))
		}
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.DB().ExecContext(h.ctx,
			`UPDATE workflow_assessments SET request_json = ? WHERE assessment_id = ?`, string(body), record.ID,
		); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("assessment for Work 1 %s not found", h.work1Task.IdempotencyKey)
}

func driverAssessmentEvidenceIDs(t *testing.T, h *driverHarness, caseID, workID domain.ID) []string {
	t.Helper()
	records, err := h.cases.ListAssessments(h.ctx, caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.WorkID != string(workID) {
			continue
		}
		var request workflowcase.AssessmentRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
			t.Fatal(err)
		}
		return request.Assessment.EvidenceIDs
	}
	t.Fatalf("assessment for work %s not found", workID)
	return nil
}

func driverAssessmentReason(t *testing.T, h *driverHarness, caseID, workID domain.ID) string {
	t.Helper()
	records, err := h.cases.ListAssessments(h.ctx, caseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.WorkID != string(workID) {
			continue
		}
		var request workflowcase.AssessmentRequest
		if err := json.Unmarshal([]byte(record.RequestJSON), &request); err != nil {
			t.Fatal(err)
		}
		return request.Assessment.Reason
	}
	t.Fatalf("assessment for work %s not found", workID)
	return ""
}

func TestDriverMaterializesWork2Idempotently(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.driver.StepOnce(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.driver.StepOnce(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Work 2 task not found")
	}
	if len(first.Materialized) != 1 || len(second.Materialized) != 1 || first.Materialized[0] != task.ID || second.Materialized[0] != task.ID {
		t.Fatalf("materialized results = %+v, %+v; want task %s", first, second, task.ID)
	}
	var count int
	if err := h.store.DB().QueryRowContext(h.ctx, `SELECT count(*) FROM tasks WHERE idempotency_key = ?`, string(c.CurrentWorkID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Work 2 task count = %d, want 1", count)
	}
	var projectCalls int
	for _, call := range h.caller.Calls() {
		if call.capability == "ado.pr.get" {
			projectCalls++
		}
	}
	if projectCalls != 1 {
		t.Fatalf("ado.pr.get calls = %d, want 1 across repeated ticks", projectCalls)
	}
	var payload PublishPayload
	if err := json.Unmarshal(task.PayloadJSON, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Decision != string(h.decision) || payload.CaseID != string(c.ID) || payload.WorkID != string(c.CurrentWorkID) ||
		payload.Project != "proj" || payload.Repo != "shop" || payload.PR != 1 || payload.Revision != "a:b" {
		t.Fatalf("Work 2 payload = %+v", payload)
	}
}

func TestDriverRejectsUnboundReviewEvidence(t *testing.T) {
	t.Run("non-agent evidence in completion", func(t *testing.T) {
		h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
		extra := putDriverEvidence(t, h.evidence, "unrelated", "text/plain", executors.EvidenceStdout)
		setDriverCompletionEvidence(t, h, extra.ID)
		setDriverAssessmentEvidence(t, h, extra.ID, h.decision)

		result, err := h.driver.StepOnce(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("materialized = %v, want none", result.Materialized)
		}
	})

	t.Run("evidence absent from completion", func(t *testing.T) {
		h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
		extra := putDriverEvidence(t, h.evidence, "unrelated", "text/plain", executors.EvidenceStdout)
		setDriverAssessmentEvidence(t, h, extra.ID, h.decision)

		result, err := h.driver.StepOnce(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("materialized = %v, want none", result.Materialized)
		}
	})

	t.Run("multiple agent evidence", func(t *testing.T) {
		h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
		first := putDriverEvidence(t, h.evidence, "first", "text/plain", executors.EvidenceAgentMessage)
		second := putDriverEvidence(t, h.evidence, "second", "text/plain", executors.EvidenceAgentMessage)
		setDriverCompletionEvidence(t, h, first.ID, second.ID)
		setDriverAssessmentEvidence(t, h, first.ID, second.ID, h.decision)

		result, err := h.driver.StepOnce(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("materialized = %v, want none", result.Materialized)
		}
	})
}

func TestDriverRejectsAmbiguousReviewEvidence(t *testing.T) {
	t.Run("multiple decision blobs", func(t *testing.T) {
		h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
		secondDecision := putDecision(t, h.evidence, h.ctx, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "other"})
		setDriverAssessmentEvidence(t, h, h.reviewEvidence.ID, h.decision, secondDecision)

		result, err := h.driver.StepOnce(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("materialized = %v, want none for multiple decision blobs", result.Materialized)
		}
	})

	t.Run("mixed bound and unbound evidence", func(t *testing.T) {
		h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
		extra := putDriverEvidence(t, h.evidence, "unbound review", "text/plain", executors.EvidenceAgentMessage)
		setDriverAssessmentEvidence(t, h, h.reviewEvidence.ID, extra.ID, h.decision)

		result, err := h.driver.StepOnce(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Materialized) != 0 {
			t.Fatalf("materialized = %v, want none for mixed bound and unbound evidence", result.Materialized)
		}
	})
}

func TestDriverRejectsMalformedPRGetResponse(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	h.driver.config.Project = "configured-project"
	h.caller.SetPages([]any{[]any{"malformed"}})

	_, err := h.driver.StepOnce(h.ctx)
	if err == nil || !strings.Contains(err.Error(), "not an object") {
		t.Fatalf("error = %v, want malformed PR response error", err)
	}
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	if task, found, findErr := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID)); findErr != nil {
		t.Fatal(findErr)
	} else if found {
		t.Fatalf("malformed response materialized task %+v", task)
	}
	h.caller.SetPages([]any{map[string]any{"title": "valid object"}})
	result, err := h.driver.StepOnce(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Materialized) != 1 {
		t.Fatalf("valid object fallback materialized = %v, want one task", result.Materialized)
	}
}

func TestDriverPropagatesProvenanceErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "database failure", err: errors.New("provenance database failure")},
		{name: "incomplete", err: runmanifest.ErrIncomplete},
		{name: "sql no rows", err: sql.ErrNoRows},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
			h.driver.manifests = &fakeCompletions{err: test.err}

			result, err := h.driver.StepOnce(h.ctx)
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			if len(result.Materialized) != 0 {
				t.Fatalf("materialized = %v, want none", result.Materialized)
			}
		})
	}
}

func TestDriverSkipsUnavailableProvenance(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "not found", err: runmanifest.ErrNotFound},
		{name: "wrapped not found", err: fmt.Errorf("provenance lookup: %w", runmanifest.ErrNotFound)},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
			h.driver.manifests = &fakeCompletions{err: test.err}

			result, err := h.driver.StepOnce(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Materialized) != 0 {
				t.Fatalf("materialized = %v, want none", result.Materialized)
			}
		})
	}
}

func TestDriverAssessesVerifiedPublication(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	task := materializeDriverWork2(t, h, c)
	content := driverPublisherContent(t, publishEvidenceEntry{
		Slot: "ado.pr.approve:proj/shop#1:a:b", Operation: "op-1", State: domain.OperationConfirmedEffect, Reference: "vote-1",
	})
	evidenceObject := putDriverEvidence(t, h.evidence, content, "text/plain", executors.EvidenceAgentMessage)
	completeDriverTask(t, h, task, evidenceObject.ID)
	result, err := h.driver.StepOnce(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.cases.Get(h.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != workflowcase.ReadyForVerification {
		t.Fatalf("case state = %q, want READY_FOR_VERIFICATION", stored.State)
	}
	if len(result.ReadyForVerification) != 1 || result.ReadyForVerification[0] != c.ID {
		t.Fatalf("driver result = %+v, want ready case %s", result, c.ID)
	}
	if len(h.vote.requests) != 1 || len(h.comment.requests) != 0 {
		t.Fatalf("lookup counts = vote:%d comment:%d", len(h.vote.requests), len(h.comment.requests))
	}
	var intent adoeffects.VoteIntent
	if err := json.Unmarshal(h.vote.requests[0].CanonicalIntent, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.Project != "proj" || intent.Repository != "shop" || intent.PR != 1 || intent.Vote != 10 {
		t.Fatalf("vote lookup intent = %+v", intent)
	}
}

func TestDriverHoldsUnverifiedPublication(t *testing.T) {
	tests := []struct {
		name       string
		decision   ReviewDecision
		entries    []publishEvidenceEntry
		content    string
		comment    domain.OperationState
		vote       domain.OperationState
		reasonPart string
	}{
		{
			name: "lookup unknown", decision: ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "nit"}}},
			entries: []publishEvidenceEntry{{Slot: "ado.pr.comment:proj/shop#1:a:b:0", Operation: "op-1", State: domain.OperationConfirmedEffect}},
			comment: domain.OperationOutcomeUnknown, reasonPart: "OUTCOME_UNKNOWN",
		},
		{
			name: "entry skipped", decision: ReviewDecision{Action: DecisionApproveAction, Vote: "approve"},
			entries:    []publishEvidenceEntry{{Slot: "ado.pr.approve:proj/shop#1:a:b", Skipped: true}},
			reasonPart: "skipped",
		},
		{
			name: "missing slot", decision: ReviewDecision{Action: DecisionCommentAction, Comments: []DecisionComment{{Path: "a.go", Line: 1, Body: "first"}, {Path: "b.go", Line: 2, Body: "second"}}},
			entries:    []publishEvidenceEntry{{Slot: "ado.pr.comment:proj/shop#1:a:b:0", Operation: "op-1", State: domain.OperationConfirmedEffect}},
			reasonPart: "missing slot",
		},
		{
			name: "malformed entry", decision: ReviewDecision{Action: DecisionApproveAction, Vote: "approve"},
			content: "not-json", reasonPart: "decode publisher evidence",
		},
		{
			name: "extra entry", decision: ReviewDecision{Action: DecisionApproveAction, Vote: "approve"},
			entries: []publishEvidenceEntry{
				{Slot: "ado.pr.approve:proj/shop#1:a:b", Operation: "op-1", State: domain.OperationConfirmedEffect},
				{Slot: "unexpected", Operation: "op-2", State: domain.OperationConfirmedEffect},
			},
			reasonPart: "unexpected slot",
		},
		{
			name: "duplicate entry", decision: ReviewDecision{Action: DecisionApproveAction, Vote: "approve"},
			entries: []publishEvidenceEntry{
				{Slot: "ado.pr.approve:proj/shop#1:a:b", Operation: "op-1", State: domain.OperationConfirmedEffect},
				{Slot: "ado.pr.approve:proj/shop#1:a:b", Operation: "op-2", State: domain.OperationConfirmedEffect},
			},
			reasonPart: "duplicate publisher slot",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := setupDriverHarness(t, test.decision)
			if test.comment != "" {
				h.comment.outcome = test.comment
			}
			if test.vote != "" {
				h.vote.outcome = test.vote
			}
			c, err := h.cases.Get(h.ctx, h.driverCaseID)
			if err != nil {
				t.Fatal(err)
			}
			task := materializeDriverWork2(t, h, c)
			content := test.content
			if content == "" {
				content = driverPublisherContent(t, test.entries...)
			}
			evidenceObject := putDriverEvidence(t, h.evidence, content, "text/plain", executors.EvidenceAgentMessage)
			completeDriverTask(t, h, task, evidenceObject.ID)
			if _, err := h.driver.StepOnce(h.ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := h.cases.Get(h.ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != workflowcase.Blocked {
				t.Fatalf("case state = %q, want BLOCKED", stored.State)
			}
			reason := driverAssessmentReason(t, h, c.ID, c.CurrentWorkID)
			if !strings.Contains(reason, test.reasonPart) {
				t.Fatalf("assessment reason = %q, want substring %q", reason, test.reasonPart)
			}
		})
	}
}

func TestDriverHoldsBlockedWork2Task(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	task := materializeDriverWork2(t, h, c)
	for index := 0; index < 2; index++ {
		attempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.execution.FailAttempt(h.ctx, attempt.ID, domain.FailureExecution, "publisher-failed", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.driver.StepOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	stored, err := h.cases.Get(h.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != workflowcase.Blocked {
		t.Fatalf("case state = %q, want BLOCKED", stored.State)
	}
	if reason := driverAssessmentReason(t, h, c.ID, c.CurrentWorkID); !strings.Contains(reason, "work2 BLOCKED") {
		t.Fatalf("assessment reason = %q, want work2 BLOCKED", reason)
	}
	evidenceIDs := driverAssessmentEvidenceIDs(t, h, c.ID, c.CurrentWorkID)
	if len(evidenceIDs) != 1 || evidenceIDs[0] == string(task.ID) {
		t.Fatalf("terminal assessment evidence = %v, want one non-task evidence ID", evidenceIDs)
	}
	object, _, err := h.evidence.Get(h.ctx, domain.ID(evidenceIDs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if object.Kind != "ado.workflow.terminal" {
		t.Fatalf("terminal evidence kind = %q, want ado.workflow.terminal", object.Kind)
	}
}

func TestDriverHoldsTerminalWork2StatesWithoutEvidence(t *testing.T) {
	for _, test := range []struct {
		name  string
		state domain.TaskState
	}{
		{name: "blocked", state: domain.TaskBlocked},
		{name: "challenged", state: domain.TaskChallenged},
		{name: "cancelled", state: domain.TaskCancelled},
		{name: "expired", state: domain.TaskExpired},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
			c, err := h.cases.Get(h.ctx, h.driverCaseID)
			if err != nil {
				t.Fatal(err)
			}
			task := materializeDriverWork2(t, h, c)
			attempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			switch test.state {
			case domain.TaskBlocked:
				if err := h.execution.FailAttempt(h.ctx, attempt.ID, domain.FailureExecution, "publisher-failed", nil); err != nil {
					t.Fatal(err)
				}
				retry, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.execution.FailAttempt(h.ctx, retry.ID, domain.FailureExecution, "publisher-failed", nil); err != nil {
					t.Fatal(err)
				}
			case domain.TaskChallenged:
				if _, err := h.execution.ChallengeTaskIfInStates(h.ctx, task.ID, testutil.AnyTaskState, domain.ChallengeTask, "publisher challenged", nil); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := h.store.DB().ExecContext(h.ctx,
					`UPDATE tasks SET state = ? WHERE task_id = ?`, test.state, task.ID); err != nil {
					t.Fatal(err)
				}
			}

			result, err := h.driver.StepOnce(h.ctx)
			if err != nil {
				t.Fatalf("terminal state %s returned error: %v", test.state, err)
			}
			if len(result.Blocked) != 1 || result.Blocked[0] != c.ID {
				t.Fatalf("result = %+v, want blocked case %s", result, c.ID)
			}
			stored, err := h.cases.Get(h.ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != workflowcase.Blocked {
				t.Fatalf("case state = %q, want BLOCKED", stored.State)
			}
			reason := driverAssessmentReason(t, h, c.ID, c.CurrentWorkID)
			if !strings.Contains(reason, "work2 "+string(test.state)) {
				t.Fatalf("assessment reason = %q, want terminal state %s", reason, test.state)
			}
			evidenceIDs := driverAssessmentEvidenceIDs(t, h, c.ID, c.CurrentWorkID)
			if len(evidenceIDs) != 1 {
				t.Fatalf("terminal assessment evidence = %v, want one evidence ID", evidenceIDs)
			}
			object, _, err := h.evidence.Get(h.ctx, domain.ID(evidenceIDs[0]))
			if err != nil {
				t.Fatal(err)
			}
			if object.Kind != "ado.workflow.terminal" {
				t.Fatalf("terminal evidence kind = %q, want ado.workflow.terminal", object.Kind)
			}
		})
	}
}

func TestDriverTerminalHoldUsesFailureEvidence(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	task := materializeDriverWork2(t, h, c)
	first := putDriverEvidence(t, h.evidence, "first failure", "text/plain", executors.EvidenceStdout)
	h.clock.Advance(time.Second)
	second := putDriverEvidence(t, h.evidence, "second failure", "text/plain", executors.EvidenceStdout)
	for index, evidenceID := range []domain.ID{first.ID, second.ID} {
		attempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.execution.FailAttempt(h.ctx, attempt.ID, domain.FailureExecution, "publisher-failed", []domain.ID{evidenceID}); err != nil {
			t.Fatalf("failure %d: %v", index, err)
		}
	}
	if _, err := h.driver.StepOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	evidenceIDs := driverAssessmentEvidenceIDs(t, h, c.ID, c.CurrentWorkID)
	if len(evidenceIDs) != 1 || evidenceIDs[0] != string(second.ID) {
		t.Fatalf("terminal assessment evidence = %v, want latest failure %s", evidenceIDs, second.ID)
	}
}

func TestDriverTerminalHoldBreaksFailureEvidenceTiesByID(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	task := materializeDriverWork2(t, h, c)
	first := putDriverEvidence(t, h.evidence, "first tied failure", "text/plain", executors.EvidenceStdout)
	second := putDriverEvidence(t, h.evidence, "second tied failure", "text/plain", executors.EvidenceStdout)
	firstAttempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.execution.FailAttempt(h.ctx, firstAttempt.ID, domain.FailureExecution, "publisher-failed", nil); err != nil {
		t.Fatal(err)
	}
	secondAttempt, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.execution.FailAttempt(h.ctx, secondAttempt.ID, domain.FailureExecution, "publisher-failed", []domain.ID{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.driver.StepOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	want := first.ID
	if second.ID > want {
		want = second.ID
	}
	evidenceIDs := driverAssessmentEvidenceIDs(t, h, c.ID, c.CurrentWorkID)
	if len(evidenceIDs) != 1 || evidenceIDs[0] != string(want) {
		t.Fatalf("terminal assessment evidence = %v, want tied failure evidence %s", evidenceIDs, want)
	}
}

func TestDriverSkipsNonPublishCases(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
	otherMission := domain.NewID("mission")
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := h.store.DB().ExecContext(h.ctx,
		`INSERT INTO missions(mission_id, statement, active, created_at, deactivated_at) VALUES (?, 'other', 0, ?, ?)`,
		otherMission, now, now,
	); err != nil {
		t.Fatal(err)
	}
	otherWorkID := domain.NewID("work")
	if _, err := h.store.DB().ExecContext(h.ctx,
		`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id, observation_evidence_id,
		 initial_request_json, grant_json, state, current_work_id, next_work_json, completed_steps, max_steps,
		 remaining_budget, progress_signature, created_at, updated_at)
		 VALUES (?, ?, 'ado', 'shop#2', 'c:d', 'other-mission', '{}',
		 '{"Capabilities":["read","ado.pr.approve","ado.pr.comment"],"Actions":["ado.pr.approve","ado.pr.comment"]}',
		 'ACTIVE', ?, '{"Kind":"publish-decision","RequiredCapabilities":null,"AuthorityCeiling":null,"ProposedActions":null}',
		 0, 3, 10, '', ?, ?)`,
		domain.NewID("case"), otherMission, otherWorkID, now, now,
	); err != nil {
		t.Fatal(err)
	}
	grant := defaultReviewGrant()
	otherSource, err := h.cases.Ensure(h.ctx, workflowcase.Observation{
		MissionID: h.mission, Source: "github", ObjectID: "shop#3", RevisionID: "e:f", EvidenceID: "other-source",
		FirstWork: workflow.WorkProposal{Kind: "publish-decision", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:     grant, MaxSteps: 3, RemainingBudget: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherKind, err := h.cases.Ensure(h.ctx, workflowcase.Observation{
		MissionID: h.mission, Source: "ado", ObjectID: "shop#4", RevisionID: "g:h", EvidenceID: "other-kind",
		FirstWork: workflow.WorkProposal{Kind: "ado.pr.review", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:     grant, MaxSteps: 3, RemainingBudget: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.driver.StepOnce(h.ctx); err != nil {
		t.Fatal(err)
	}
	for _, workID := range []domain.ID{otherWorkID, otherSource.CurrentWorkID, otherKind.CurrentWorkID} {
		if task, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(workID)); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatalf("non-publish case materialized task %+v", task)
		}
	}
}

func TestDriverWaitsForWork2Execution(t *testing.T) {
	for _, test := range []struct {
		name      string
		execute   bool
		wantState domain.TaskState
	}{
		{name: "eligible", wantState: domain.TaskEligible},
		{name: "executing", execute: true, wantState: domain.TaskExecuting},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
			c, err := h.cases.Get(h.ctx, h.driverCaseID)
			if err != nil {
				t.Fatal(err)
			}
			task := materializeDriverWork2(t, h, c)
			if test.execute {
				if _, err := h.execution.StartAttempt(h.ctx, task.ID, "publisher", time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			result, err := h.driver.StepOnce(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			storedTask, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID))
			if err != nil {
				t.Fatal(err)
			}
			if !found || storedTask.State != test.wantState {
				t.Fatalf("task = %+v, found=%v, want state %s", storedTask, found, test.wantState)
			}
			storedCase, err := h.cases.Get(h.ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedCase.State != workflowcase.Active {
				t.Fatalf("case state = %q, want ACTIVE", storedCase.State)
			}
			if len(result.ReadyForVerification) != 0 || len(result.Blocked) != 0 {
				t.Fatalf("result = %+v, want wait-only result", result)
			}
		})
	}
}

func TestDriverRunStopsOnMidFlightCancellation(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve", Reason: "clean"})
	caller := &blockingDriverCaller{started: make(chan struct{})}
	h.driver.config.Caller = caller
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- h.driver.Run(runCtx, time.Hour)
	}()
	select {
	case <-caller.started:
	case <-time.After(time.Second):
		t.Fatal("driver did not reach the in-flight PR call")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("driver did not stop after mid-flight cancellation")
	}
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	if task, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("cancelled Run materialized task %+v", task)
	}
}

func TestDriverRunStopsOnCancel(t *testing.T) {
	h := setupDriverHarness(t, ReviewDecision{Action: DecisionApproveAction, Vote: "approve"})
	c, err := h.cases.Get(h.ctx, h.driverCaseID)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.driver.Run(runCtx, time.Minute); err != nil {
		t.Fatal(err)
	}
	if task, found, err := h.execution.FindByIdempotencyKey(h.ctx, string(c.CurrentWorkID)); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatalf("cancelled Run materialized task %+v", task)
	}
}
