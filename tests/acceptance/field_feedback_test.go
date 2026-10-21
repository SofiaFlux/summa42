package acceptance_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type feedbackAcceptancePolicy struct {
	mu       sync.Mutex
	outcome  domain.PolicyOutcome
	hash     string
	required []domain.ID
}

func (p *feedbackAcceptancePolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return domain.PolicyDecision{
		ID: domain.NewID("decision"), Outcome: p.outcome,
		RequiredApprovals: append([]domain.ID(nil), p.required...),
		ReasonCodes:       []string{"field-feedback-acceptance"},
		PolicySetID:       "policy_feedback_acceptance", PolicySetHash: p.hash,
		PolicyCapabilitiesHash: "caps_feedback_acceptance",
		InputDigest:            "feedback-input", EvaluatedAt: in.Now,
	}, nil
}

func (p *feedbackAcceptancePolicy) set(outcome domain.PolicyOutcome, hash string, required ...domain.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcome = outcome
	p.hash = hash
	p.required = append([]domain.ID(nil), required...)
}

type acceptanceFeedbackSink struct {
	mu          sync.Mutex
	createCount int
	lookupCount int
	loseAck     bool
	created     map[string]string
	payloads    []fieldfeedback.IssuePayload
}

func newAcceptanceFeedbackSink(loseAck bool) *acceptanceFeedbackSink {
	return &acceptanceFeedbackSink{loseAck: loseAck, created: map[string]string{}}
}

func (s *acceptanceFeedbackSink) Create(_ context.Context, payload fieldfeedback.IssuePayload) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCount++
	s.payloads = append(s.payloads, payload)
	ref := "issue-" + payload.Marker
	s.created[payload.Marker] = ref
	if s.loseAck {
		return "", 0, errors.New("simulated lost acknowledgement")
	}
	return ref, 0, nil
}

func (s *acceptanceFeedbackSink) FindByMarker(_ context.Context, marker string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookupCount++
	ref, ok := s.created[marker]
	return ref, ok, nil
}

func (s *acceptanceFeedbackSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCount, s.lookupCount
}

func (s *acceptanceFeedbackSink) lastPayload() fieldfeedback.IssuePayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.payloads) == 0 {
		return fieldfeedback.IssuePayload{}
	}
	return s.payloads[len(s.payloads)-1]
}

type feedbackStatusProvider struct{ box *summa42runtime.Box }

func (p feedbackStatusProvider) Status(context.Context) (control.StatusDTO, error) {
	return control.StatusDTO{CollectiveID: p.box.CollectiveID, State: "ACTIVE"}, nil
}

type feedbackAcceptanceFixture struct {
	ctx       context.Context
	box       *summa42runtime.Box
	clock     *testutil.Clock
	policy    *feedbackAcceptancePolicy
	sink      *acceptanceFeedbackSink
	owner     *identity.LocalEd25519
	candidate domain.FeedbackCandidate
	artifact  domain.SanitizedFeedback
	emitTask  domain.Task
}

func newFeedbackAcceptanceFixture(t *testing.T, mode localconfig.FeedbackMode, loseAck bool, createMaintenanceBudget bool) *feedbackAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	clk := testutil.NewClock(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	owner, err := identity.NewLocalEd25519(filepath.Join(root, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	pol := &feedbackAcceptancePolicy{outcome: domain.PolicyAllow, hash: "policy-feedback-v1"}
	sink := newAcceptanceFeedbackSink(loseAck)
	cfg := summa42runtime.Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		Clock: clk, CollectiveID: "collective-feedback-acceptance", OwnerPrincipalID: owner.PrincipalID(),
		PolicyEngine: pol,
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: true, Mode: mode, Provider: "github", Destination: "owner/repo",
			MaintenanceEnvelopeID: "feedback-maintenance", RequiredEnforcement: domain.EnforcementEnforced,
		},
		FeedbackSink:  sink,
		LeaseDuration: time.Hour,
	}
	box, err := summa42runtime.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := box.Store.DB().ExecContext(ctx, `
		INSERT INTO policy_sets(
			policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at
		) VALUES ('policy_feedback_acceptance', 1, 'feedback.rego', 'package feedback', 'policy-feedback-v1', 'caps_feedback_acceptance', 1, ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('work-feedback', 100, ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if createMaintenanceBudget {
		if _, err := box.Store.DB().ExecContext(ctx,
			`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('feedback-maintenance', 20, ?)`, now,
		); err != nil {
			t.Fatal(err)
		}
	}

	task, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "field-dogfood"},
		TaskClass:            "repo.debug",
		AcceptanceCriteria:   []string{"work completes"},
		RequiredCapabilities: []string{"repo.read"}, RequiredEnforcement: domain.EnforcementEnforced,
		AuthorityCeiling: []string{"repo.read"}, ResourceEnvelopeID: "work-feedback",
	})
	if err != nil {
		t.Fatal(err)
	}
	const localSecret = "/home/client-x/private-repo/secret.go token=LOCAL-ONLY-SECRET"
	first, err := box.Execution.StartAttempt(ctx, task.ID, "codex", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Execution.FailAttempt(ctx, first.ID, domain.FailureExecution, localSecret, nil); err != nil {
		t.Fatal(err)
	}
	second, err := box.Execution.StartAttempt(ctx, task.ID, "codex", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Execution.FailAttempt(ctx, second.ID, domain.FailureExecution, localSecret, nil); err != nil {
		t.Fatal(err)
	}
	observations, err := box.FieldObserver.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var repeated domain.FieldObservation
	for _, observation := range observations {
		if observation.Category == "REPEATED_ATTEMPT_FAILURE" {
			repeated = observation
			break
		}
	}
	if repeated.ID == "" {
		t.Fatalf("repeated-failure observation not found: %+v", observations)
	}
	if !strings.Contains(repeated.SummaryLocal, "LOCAL-ONLY-SECRET") {
		t.Fatalf("test lost local-sensitive premise: %q", repeated.SummaryLocal)
	}

	controlServer, err := control.NewServer(control.ServerConfig{
		AuthToken: "field-product-path", OwnerPrincipalID: owner.PrincipalID(),
		OwnerPublicKey: owner.PublicKey(), ChallengeTTL: time.Minute, Now: box.Clock.Now,
	}, control.Dependencies{
		Status: feedbackStatusProvider{box: box}, Tasks: box.Execution, Missions: box.Purpose, Approvals: box.Approvals,
		Feedback: box.Feedback, Sanitizer: box.Sanitizer, FieldObserver: box.FieldObserver, Experience: box.Experience,
		Attempts: box.Execution, Operations: box.Operations, Shutdown: box,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(controlServer.Handler())
	defer httpServer.Close()
	client := control.NewClient(httpServer.URL, "field-product-path", httpServer.Client())

	created, err := client.FeedbackCandidateCreate(ctx, control.FeedbackCandidateCreateRequest{
		ObservationIDs: []domain.ID{repeated.ID}, GenericTaskClass: domain.GenericTaskDebugging,
		Category: "RECOVERY_FRICTION", ExpectedBehavior: "recover automatically after a failed attempt",
		ObservedBehavior: "repeated execution failure required another attempt",
		StateTransitions: []string{"EXECUTING", "FAILED", "ELIGIBLE", "EXECUTING", "FAILED"},
		Metrics:          fieldfeedback.NormalizedMetrics{}, HumanIntervention: false, RecoveryResult: "FAILED",
		RuntimeVersion: "acceptance", ExecutorKind: "codex", ExecutorVersion: "acceptance",
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := box.Feedback.Candidate(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	sanitized, sanitizeErr := client.FeedbackSanitize(ctx, candidate.ID)
	artifact, found, loadErr := box.Feedback.LatestSanitizedFeedbackForCandidate(ctx, candidate.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !found || artifact.ID == "" {
		t.Fatalf("product-path sanitization created no immutable artifact: dto=%+v err=%v", sanitized, sanitizeErr)
	}
	if !createMaintenanceBudget {
		if sanitizeErr == nil {
			t.Fatal("product-path sanitization unexpectedly scheduled emission without maintenance budget")
		}
		return &feedbackAcceptanceFixture{ctx: ctx, box: box, clock: clk, policy: pol, sink: sink, owner: owner, candidate: candidate, artifact: artifact}
	}
	if sanitizeErr != nil {
		t.Fatal(sanitizeErr)
	}
	if sanitized.Outcome != domain.SanitizationPass || sanitized.Artifact == nil || sanitized.Artifact.ID != artifact.ID {
		t.Fatalf("product-path sanitization=%+v artifact=%+v", sanitized, artifact)
	}
	if mode == localconfig.FeedbackModeLocalOnly {
		return &feedbackAcceptanceFixture{
			ctx: ctx, box: box, clock: clk, policy: pol, sink: sink, owner: owner,
			candidate: candidate, artifact: artifact,
		}
	}
	var emitTaskID domain.ID
	if err := box.Store.DB().QueryRowContext(ctx,
		`SELECT task_id FROM feedback_emissions WHERE feedback_id = ?`, artifact.ID,
	).Scan(&emitTaskID); err != nil {
		t.Fatal(err)
	}
	emitTask, err := box.Execution.Task(ctx, emitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	return &feedbackAcceptanceFixture{
		ctx: ctx, box: box, clock: clk, policy: pol, sink: sink, owner: owner,
		candidate: candidate, artifact: artifact, emitTask: emitTask,
	}
}

func (f *feedbackAcceptanceFixture) leaseEmitter(t *testing.T) domain.Attempt {
	t.Helper()
	capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"feedback.github.issue.create": {Accessible: true, Enforcement: domain.EnforcementEnforced},
	}}
	next, err := f.box.Scheduler.Next(f.ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if next == nil || next.Task.ID != f.emitTask.ID {
		t.Fatalf("scheduler candidate=%v, want feedback task %s", next, f.emitTask.ID)
	}
	executorKind, err := f.box.Scheduler.ChooseExecutor(f.ctx, next.Task, []string{"feedback-emitter"})
	if err != nil {
		t.Fatal(err)
	}
	if executorKind != "feedback-emitter" {
		t.Fatalf("executor=%q", executorKind)
	}
	attempt, err := f.box.Scheduler.Lease(f.ctx, f.emitTask.ID, executorKind)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func (f *feedbackAcceptanceFixture) startEmitter(t *testing.T, attempt domain.Attempt) (executors.ExecutionResult, error) {
	t.Helper()
	executor := f.box.Executors["feedback-emitter"]
	if executor == nil {
		t.Fatal("feedback-emitter is not registered")
	}
	return executor.Start(f.ctx, executors.AttemptEnvelope{TaskID: f.emitTask.ID, AttemptID: attempt.ID})
}

func (f *feedbackAcceptanceFixture) operationForEmit(t *testing.T) domain.ExternalOperation {
	t.Helper()
	var id domain.ID
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT operation_id FROM external_operations WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, f.emitTask.ID,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	op, err := f.box.Operations.Operation(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f *feedbackAcceptanceFixture) signedApprove(t *testing.T, approvalID domain.ID) {
	t.Helper()
	server, err := control.NewServer(control.ServerConfig{
		AuthToken: "acceptance-control-token", OwnerPrincipalID: f.owner.PrincipalID(),
		OwnerPublicKey: f.owner.PublicKey(), ChallengeTTL: time.Minute, Now: f.clock.Now,
	}, control.Dependencies{
		Status: feedbackStatusProvider{box: f.box}, Tasks: f.box.Execution, Missions: f.box.Purpose, Approvals: f.box.Approvals,
		Feedback: f.box.Feedback, Sanitizer: f.box.Sanitizer, FieldObserver: f.box.FieldObserver, Experience: f.box.Experience,
		Attempts: f.box.Execution, Operations: f.box.Operations, Shutdown: f.box,
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := control.NewClient(httpServer.URL, "acceptance-control-token", httpServer.Client())
	pending, err := client.Approval(f.ctx, approvalID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := client.Approve(f.ctx, approvalID, f.owner, pending.RequestDigest)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != string(domain.ApprovalApproved) {
		t.Fatalf("approval=%+v", approved)
	}
}

func TestFieldFeedbackVerticalSlice(t *testing.T) {
	f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeRequireApproval, false, true)
	attempt := f.leaseEmitter(t)

	_, err := f.startEmitter(t, attempt)
	if !errors.Is(err, domain.ErrPolicyDenied) {
		t.Fatalf("pre-approval emitter err=%v, want policy denial", err)
	}
	op := f.operationForEmit(t)
	if op.State != domain.OperationPrepared || op.ApprovalID == "" {
		t.Fatalf("pre-approval operation=%+v", op)
	}
	if creates, _ := f.sink.counts(); creates != 0 {
		t.Fatalf("sink create count=%d before approval", creates)
	}

	f.signedApprove(t, op.ApprovalID)
	result, err := f.startEmitter(t, attempt)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("approved emitter result=%+v err=%v", result, err)
	}
	if creates, _ := f.sink.counts(); creates != 1 {
		t.Fatalf("sink create count=%d, want 1", creates)
	}
	payload := f.sink.lastPayload()
	if strings.Contains(payload.Body, "LOCAL-ONLY-SECRET") || strings.Contains(payload.Body, "/home/client-x") {
		t.Fatalf("sink payload leaked local context: %s", payload.Body)
	}
	var canonical string
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT canonical_intent_json FROM effect_slots WHERE effect_slot_id = ?`, op.EffectSlotID,
	).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(canonical, "LOCAL-ONLY-SECRET") || strings.Contains(canonical, "/home/client-x") {
		t.Fatalf("canonical external intent leaked local context: %s", canonical)
	}
	reported, err := f.box.Feedback.Candidate(f.ctx, f.candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reported.State != domain.FeedbackStateReported {
		t.Fatalf("candidate state=%s, want REPORTED", reported.State)
	}

	ev := putTextEvidence(t, f.ctx, f.box, result.Evidence[0].Content, "FEEDBACK_EMIT_RESULT")
	completeAndAccept(t, f.ctx, f.box, f.emitTask.ID, attempt.ID, f.owner.PrincipalID(), ev.ID)
	var auditCount int
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM audit_events WHERE kind IN ('FIELD_OBSERVATION_RECORDED','FEEDBACK_REPORTED')`,
	).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 2 {
		t.Fatalf("feedback audit events=%d, want observation + report", auditCount)
	}
}

func TestFieldFeedbackUnknownOutcomeReconcilesExactlyOnce(t *testing.T) {
	f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, true, true)
	attempt := f.leaseEmitter(t)
	if _, err := f.startEmitter(t, attempt); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("first emitter err=%v, want ErrOutcomeUnknown", err)
	}
	if err := f.box.Execution.RevokeLease(f.ctx, attempt.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.box.Execution.StartAttempt(f.ctx, f.emitTask.ID, "feedback-emitter", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.sink.mu.Lock()
	f.sink.loseAck = false
	f.sink.mu.Unlock()
	result, err := f.startEmitter(t, replacement)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("reconciliation result=%+v err=%v", result, err)
	}
	createCount, lookupCount := f.sink.counts()
	if createCount != 1 || lookupCount != 1 {
		t.Fatalf("sink calls create=%d lookup=%d, want 1/1", createCount, lookupCount)
	}
}

func TestFieldFeedbackNegativeAcceptance(t *testing.T) {
	t.Run("raw_observation_cannot_be_emitted", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, true)
		if strings.Contains(f.artifact.ContentJSON, "LOCAL-ONLY-SECRET") {
			t.Fatal("sanitized artifact contains raw local observation")
		}
	})

	t.Run("modified_sanitized_artifact_cannot_dispatch", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, true)
		if _, err := f.box.Store.DB().ExecContext(f.ctx,
			`UPDATE sanitized_feedback SET content_json='{"leak":"secret"}' WHERE feedback_id = ?`, f.artifact.ID,
		); err == nil {
			t.Fatal("immutable sanitized artifact was modified")
		}
	})

	t.Run("uncertain_sanitizer_retains_local_only", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeLocalOnly, false, true)
		candidate, err := f.box.Feedback.CreateCandidate(f.ctx, fieldfeedback.CandidateInput{
			ObservationIDs:   []domain.ID{candidateObservationIDAcceptance(t, f, f.candidate.ID)},
			GenericTaskClass: domain.GenericTaskTesting, Category: "TEST",
			ExpectedBehavior: "safe", ObservedBehavior: "safe",
			StateTransitions: []string{"EXECUTING"}, Enforcement: domain.EnforcementEnforced,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.box.Store.DB().ExecContext(f.ctx,
			`UPDATE feedback_candidates SET state_transition_json = '{' WHERE candidate_id = ?`, candidate.ID,
		); err != nil {
			t.Fatal(err)
		}
		artifact, result, err := f.box.Sanitizer.Sanitize(f.ctx, candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome != domain.SanitizationUncertain || artifact.ID != "" {
			t.Fatalf("uncertain result=%+v artifact=%+v", result, artifact)
		}
		got, err := f.box.Feedback.Candidate(f.ctx, candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != domain.FeedbackStateLocalOnly {
			t.Fatalf("uncertain candidate state=%s, want LOCAL_ONLY", got.State)
		}
	})

	t.Run("approval_for_feedback_a_cannot_authorize_b", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeRequireApproval, false, true)
		attemptA := f.leaseEmitter(t)
		if _, err := f.startEmitter(t, attemptA); !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatal(err)
		}
		opA := f.operationForEmit(t)
		f.signedApprove(t, opA.ApprovalID)

		artifactB, taskB := addSecondFeedbackArtifact(t, f)
		attemptB, err := f.box.Execution.StartAttempt(f.ctx, taskB.ID, "feedback-emitter", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		executor := f.box.Executors["feedback-emitter"]
		if _, err := executor.Start(f.ctx, executors.AttemptEnvelope{TaskID: taskB.ID, AttemptID: attemptB.ID}); !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatalf("feedback B initial error=%v", err)
		}
		var opB domain.ID
		if err := f.box.Store.DB().QueryRowContext(f.ctx,
			`SELECT operation_id FROM external_operations WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, taskB.ID,
		).Scan(&opB); err != nil {
			t.Fatal(err)
		}
		if _, err := f.box.Store.DB().ExecContext(f.ctx,
			`UPDATE external_operations SET approval_id = ? WHERE operation_id = ?`, opA.ApprovalID, opB,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := f.box.Operations.Dispatch(f.ctx, opB, attemptB.ID); !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatalf("approval A authorized B artifact=%s err=%v", artifactB.ID, err)
		}
	})

	t.Run("expired_approval_cannot_dispatch", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeRequireApproval, false, true)
		attempt := f.leaseEmitter(t)
		_, _ = f.startEmitter(t, attempt)
		op := f.operationForEmit(t)
		f.signedApprove(t, op.ApprovalID)
		f.clock.Advance(25 * time.Hour)
		if _, err := f.box.Operations.Dispatch(f.ctx, op.ID, attempt.ID); err == nil {
			t.Fatal("expired approval dispatched an external operation")
		}
	})

	t.Run("policy_deny_after_approval_wins", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeRequireApproval, false, true)
		attempt := f.leaseEmitter(t)
		_, _ = f.startEmitter(t, attempt)
		op := f.operationForEmit(t)
		f.signedApprove(t, op.ApprovalID)
		f.policy.set(domain.PolicyDeny, "policy-feedback-v2")
		if _, err := f.box.Store.DB().ExecContext(f.ctx,
			`UPDATE policy_sets SET policy_hash='policy-feedback-v2' WHERE active=1`,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := f.box.Operations.Dispatch(f.ctx, op.ID, attempt.ID); !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatalf("policy revocation dispatch err=%v", err)
		}
		if creates, _ := f.sink.counts(); creates != 0 {
			t.Fatalf("provider called after policy deny: %d", creates)
		}
	})

	t.Run("feedback_provider_bypass_blocked_in_enforced_profile", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, true)
		task, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
			Purpose:            domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "bypass"},
			AcceptanceCriteria: []string{"no feedback authority"}, RequiredCapabilities: []string{"repo.read"},
			RequiredEnforcement: domain.EnforcementEnforced, AuthorityCeiling: []string{"repo.read"},
			ResourceEnvelopeID: "work-feedback",
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := f.box.Execution.StartAttempt(f.ctx, task.ID, "untrusted", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.box.Operations.Prepare(f.ctx, operations.PrepareRequest{
			AttemptID: attempt.ID, Provider: "github", TrustedSlotKey: "bypass",
			Intent: fieldfeedback.EmitIntent{SanitizedFeedbackID: f.artifact.ID, Destination: "owner/repo"}, Risk: "LOW",
		})
		if !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatalf("provider bypass err=%v, want policy denial", err)
		}
	})

	t.Run("require_approval_mode_tightens_policy_allow", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeRequireApproval, false, true)
		attempt := f.leaseEmitter(t)
		if _, err := f.startEmitter(t, attempt); !errors.Is(err, domain.ErrPolicyDenied) {
			t.Fatalf("policy ALLOW bypassed environment REQUIRE_APPROVAL: %v", err)
		}
		if f.operationForEmit(t).ApprovalID == "" {
			t.Fatal("REQUIRE_APPROVAL did not create durable approval")
		}
	})

	t.Run("auto_mode_grants_no_authority", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, true)
		if len(f.emitTask.AuthorityCeiling) != 1 || f.emitTask.AuthorityCeiling[0] != "feedback.github.issue.create" {
			t.Fatalf("AUTO authority ceiling=%v", f.emitTask.AuthorityCeiling)
		}
	})

	t.Run("missing_maintenance_budget_creates_no_task", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, false)
		var count int
		if err := f.box.Store.DB().QueryRowContext(f.ctx,
			`SELECT count(*) FROM tasks WHERE task_class='collective.feedback.emit'`,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("emit tasks=%d without maintenance budget", count)
		}
	})

	t.Run("credential_never_enters_executor_context", func(t *testing.T) {
		f := newFeedbackAcceptanceFixture(t, localconfig.FeedbackModeAutoIfAllowed, false, true)
		attempt := f.leaseEmitter(t)
		if _, err := f.startEmitter(t, attempt); err != nil {
			t.Fatal(err)
		}
		op := f.operationForEmit(t)
		var canonical string
		if err := f.box.Store.DB().QueryRowContext(f.ctx,
			`SELECT canonical_intent_json FROM effect_slots WHERE effect_slot_id = ?`, op.EffectSlotID,
		).Scan(&canonical); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"token", "credential", "Authorization", "LOCAL-ONLY-SECRET"} {
			if strings.Contains(canonical, forbidden) {
				t.Fatalf("canonical executor/provider context contains %q: %s", forbidden, canonical)
			}
		}
	})
}

func candidateObservationIDAcceptance(t *testing.T, f *feedbackAcceptanceFixture, candidateID domain.ID) domain.ID {
	t.Helper()
	var id domain.ID
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT observation_id FROM feedback_candidate_observations WHERE candidate_id = ? ORDER BY observation_id LIMIT 1`,
		candidateID,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func addSecondFeedbackArtifact(t *testing.T, f *feedbackAcceptanceFixture) (domain.SanitizedFeedback, domain.Task) {
	t.Helper()
	observationID := candidateObservationIDAcceptance(t, f, f.candidate.ID)
	candidate, err := f.box.Feedback.CreateCandidate(f.ctx, fieldfeedback.CandidateInput{
		ObservationIDs: []domain.ID{observationID}, GenericTaskClass: domain.GenericTaskDebugging,
		Category: "RECOVERY_FRICTION", ExpectedBehavior: "safe expectation b",
		ObservedBehavior: "safe behavior b", StateTransitions: []string{"EXECUTING", "FAILED"},
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, result, err := f.box.Sanitizer.Sanitize(f.ctx, candidate.ID)
	if err != nil || result.Outcome != domain.SanitizationPass {
		t.Fatalf("second sanitizer result=%+v err=%v", result, err)
	}
	if err := f.box.Feedback.OnSanitized(f.ctx, artifact.ID); err != nil {
		t.Fatal(err)
	}
	var taskID domain.ID
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT task_id FROM feedback_emissions WHERE feedback_id = ?`, artifact.ID,
	).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	task, err := f.box.Execution.Task(f.ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return artifact, task
}
