package acceptance_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

var acceptanceNow = time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)

type mutablePolicy struct {
	mu      sync.Mutex
	outcome domain.PolicyOutcome
	hash    string
}

func (p *mutablePolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return domain.PolicyDecision{
		ID:                     domain.NewID("decision"),
		Outcome:                p.outcome,
		ReasonCodes:            []string{"acceptance"},
		PolicySetID:            domain.ID("policy_acceptance"),
		PolicySetHash:          p.hash,
		PolicyCapabilitiesHash: "caps_acceptance",
		InputDigest:            "input_acceptance",
		EvaluatedAt:            in.Now,
	}, nil
}

func (p *mutablePolicy) set(outcome domain.PolicyOutcome, hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outcome = outcome
	p.hash = hash
}

type fakeOperationProvider struct {
	mu                  sync.Mutex
	name                string
	loseAcknowledgement bool
	dispatchCount       int
	lookupCount         int
	outcomes            map[domain.ID]operations.ProviderOutcome
}

func newFakeOperationProvider(name string) *fakeOperationProvider {
	return &fakeOperationProvider{name: name, outcomes: make(map[domain.ID]operations.ProviderOutcome)}
}

func (p *fakeOperationProvider) Name() string       { return p.name }
func (p *fakeOperationProvider) Capability() string { return "external:" + p.name }
func (p *fakeOperationProvider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementEnforced
}
func (p *fakeOperationProvider) AdapterVersion() string                   { return "acceptance-v1" }
func (p *fakeOperationProvider) AdapterVersionSemanticallyRelevant() bool { return false }
func (p *fakeOperationProvider) SettleWindow() time.Duration              { return 0 }
func (p *fakeOperationProvider) MinimumRisk() string                      { return "LOW" }

func (p *fakeOperationProvider) CanonicalIntent(descriptor operations.IntentDescriptor) ([]byte, error) {
	intent, ok := descriptor.(testutil.PurchaseIntent)
	if !ok || intent.SKU == "" || intent.Quantity <= 0 {
		return nil, errors.New("invalid purchase intent")
	}
	return json.Marshal(intent)
}

func (p *fakeOperationProvider) CostProfile(descriptor operations.IntentDescriptor) (operations.CostProfile, error) {
	intent, ok := descriptor.(testutil.PurchaseIntent)
	if !ok || intent.Quantity <= 0 {
		return operations.CostProfile{}, errors.New("invalid purchase intent")
	}
	return operations.CostProfile{
		MaxExposure: int64(intent.Quantity * 10),
		Enforceability: resources.Enforceability{
			CostControl:    resources.CostTechnicallyCapped,
			RequireHardCap: true,
			Source:         "acceptance-provider-cap",
		},
	}, nil
}

func (p *fakeOperationProvider) Dispatch(_ context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dispatchCount++
	var intent testutil.PurchaseIntent
	if err := json.Unmarshal(request.CanonicalIntent, &intent); err != nil {
		return operations.ProviderOutcome{}, err
	}
	outcome := operations.ProviderOutcome{
		State:             domain.OperationConfirmedEffect,
		ProviderReference: "acceptance-" + string(request.OperationID),
		ActualCost:        int64(intent.Quantity * 10),
	}
	p.outcomes[request.OperationID] = outcome
	if p.loseAcknowledgement {
		return operations.ProviderOutcome{}, domain.ErrOutcomeUnknown
	}
	return outcome, nil
}

func (p *fakeOperationProvider) LookupOutcome(_ context.Context, request operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lookupCount++
	outcome, ok := p.outcomes[request.OperationID]
	if !ok {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, domain.ErrOutcomeUnknown
	}
	return outcome, nil
}

func (p *fakeOperationProvider) setLoseAcknowledgement(value bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loseAcknowledgement = value
}

func (p *fakeOperationProvider) counts() (dispatch, lookup int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dispatchCount, p.lookupCount
}

type fakeCapabilityProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *fakeCapabilityProvider) Name() string { return "acceptance-capabilities" }
func (p *fakeCapabilityProvider) Call(context.Context, string, any) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return "called", nil
}
func (p *fakeCapabilityProvider) Advertise(context.Context) ([]capabilities.Definition, error) {
	return []capabilities.Definition{{
		ID:          domain.ID("cap_safe_read"),
		Skill:       capabilities.Skill{Name: "safe.read", Version: "v1"},
		Access:      capabilities.Access{Provider: p.Name(), Context: "acceptance"},
		Authority:   capabilities.AuthorityRequirement{Capabilities: []string{"safe.read"}},
		Environment: capabilities.EnvironmentRequirement{MinimumEnforcement: domain.EnforcementEnforced},
	}}, nil
}
func (p *fakeCapabilityProvider) Probe(context.Context, capabilities.Definition) (capabilities.ProbeResult, error) {
	return capabilities.ProbeResult{
		Enforcement:  domain.EnforcementEnforced,
		Evidence:     []string{"acceptance-probe"},
		CostMetadata: map[string]any{"unit": "test"},
		Health:       capabilities.HealthHealthy,
		Available:    true,
	}, nil
}
func (p *fakeCapabilityProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type fixture struct {
	ctx       context.Context
	box       *summa42runtime.Box
	config    summa42runtime.Config
	clock     *testutil.Clock
	policy    *mutablePolicy
	provider  *fakeOperationProvider
	caps      *fakeCapabilityProvider
	missionID domain.ID
	task      domain.Task
	attempt   domain.Attempt
	envelope  domain.ID
}

func newFixture(t *testing.T, hardLimit int64) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	clk := testutil.NewClock(acceptanceNow)
	pol := &mutablePolicy{outcome: domain.PolicyAllow, hash: "policy-v1"}
	provider := newFakeOperationProvider("fake")
	caps := &fakeCapabilityProvider{}
	config := summa42runtime.Config{
		StatePath:           filepath.Join(dir, "state.db"),
		EvidencePath:        filepath.Join(dir, "evidence"),
		Clock:               clk,
		CollectiveID:        domain.ID("collective_acceptance"),
		OwnerPrincipalID:    domain.ID("owner_acceptance"),
		PolicyEngine:        pol,
		OperationProviders:  []operations.Provider{provider},
		CapabilityProviders: []capabilities.Provider{caps},
		LeaseDuration:       time.Minute,
	}
	box, err := summa42runtime.Open(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close() })

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := box.Store.DB().ExecContext(ctx, `
		INSERT INTO policy_sets(policy_set_id, version, module_name, module, policy_hash, capabilities_hash, active, created_at)
		VALUES ('policy_acceptance', 1, 'acceptance.rego', 'package acceptance', 'policy-v1', 'caps_acceptance', 1, ?)`, now); err != nil {
		t.Fatal(err)
	}
	envelope := domain.ID("envelope_acceptance")
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`, envelope, hardLimit, now); err != nil {
		t.Fatal(err)
	}
	missionID, err := box.Purpose.CreateMission(ctx, "Exercise MVC acceptance semantics")
	if err != nil {
		t.Fatal(err)
	}
	task, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"verified"},
		RequiredCapabilities: []string{provider.Capability()},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{provider.Capability()},
		ResourceEnvelopeID:   envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := box.Execution.StartAttempt(ctx, task.ID, "acceptance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{ctx: ctx, box: box, config: config, clock: clk, policy: pol, provider: provider, caps: caps, missionID: missionID, task: task, attempt: attempt, envelope: envelope}
}

func (f *fixture) prepare(t *testing.T, attemptID domain.ID, key string, quantity int) domain.ExternalOperation {
	t.Helper()
	op, err := f.box.Operations.Prepare(f.ctx, operations.PrepareRequest{
		AttemptID:      attemptID,
		Provider:       f.provider.Name(),
		TrustedSlotKey: key,
		Intent:         testutil.PurchaseIntent{SKU: "sku-1", Quantity: quantity},
		Risk:           "LOW",
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (f *fixture) replacementAttempt(t *testing.T) domain.Attempt {
	t.Helper()
	if err := f.box.Execution.RevokeLease(f.ctx, f.attempt.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.box.Execution.StartAttempt(f.ctx, f.task.ID, "replacement", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return replacement
}

func TestReviewedFailureSemantics(t *testing.T) {
	cases := []struct {
		name string
		run  func(*testing.T)
	}{
		{"unknown_external_result_reconciles_without_duplicate", testUnknownExternalResultReconcilesWithoutDuplicate},
		{"unresolved_billing_exposure_blocks_overcommit", testUnresolvedBillingExposureBlocksOvercommit},
		{"stale_attempt_rejected", testStaleAttemptRejected},
		{"expired_lease_with_matching_fence_rejected", testExpiredLeaseWithMatchingFenceRejected},
		{"crash_before_completion_record_does_not_fake_completion", testCrashBeforeCompletionRecordDoesNotFakeCompletion},
		{"completion_record_resumes_verification", testCompletionRecordResumesVerification},
		{"capability_bypass_blocked_in_enforced_profile", testCapabilityBypassBlockedInEnforcedProfile},
		{"constraint_precedence_blocks_illegal_goal_path", testConstraintPrecedenceBlocksIllegalGoalPath},
		{"hard_budget_requires_enforceable_ceiling", testHardBudgetRequiresEnforceableCeiling},
		{"confirmed_effect_survives_attempt_replacement", testConfirmedEffectSurvivesAttemptReplacement},
		{"effect_slot_parameter_drift_conflicts", testEffectSlotParameterDriftConflicts},
		{"prepared_revocation_prevents_dispatch", testPreparedRevocationPreventsDispatch},
		{"opa_http_send_rejected", testOPAHTTPSendRejected},
		{"opa_error_timeout_invalid_output_fail_closed", testOPAErrorTimeoutInvalidOutputFailClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

func testUnknownExternalResultReconcilesWithoutDuplicate(t *testing.T) {
	f := newFixture(t, 100)
	f.provider.setLoseAcknowledgement(true)
	op := f.prepare(t, f.attempt.ID, "purchase-primary", 2)
	unknown, err := f.box.Operations.Dispatch(f.ctx, op.ID, f.attempt.ID)
	if !errors.Is(err, domain.ErrOutcomeUnknown) || unknown.State != domain.OperationOutcomeUnknown {
		t.Fatalf("dispatch = state:%s err:%v", unknown.State, err)
	}
	var reservationState domain.ReservationState
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT state FROM resource_reservations WHERE reservation_id = ?`, unknown.ReservationID).Scan(&reservationState); err != nil {
		t.Fatal(err)
	}
	if reservationState != domain.ReservationUnresolved {
		t.Fatalf("reservation state = %s, want UNRESOLVED", reservationState)
	}
	replacement := f.replacementAttempt(t)
	resolved, err := f.box.Operations.Dispatch(f.ctx, op.ID, replacement.ID)
	if err != nil || resolved.State != domain.OperationConfirmedEffect {
		t.Fatalf("reconciliation = state:%s err:%v", resolved.State, err)
	}
	dispatch, lookup := f.provider.counts()
	if dispatch != 1 || lookup != 1 {
		t.Fatalf("provider calls dispatch=%d lookup=%d, want 1/1", dispatch, lookup)
	}
}

func testUnresolvedBillingExposureBlocksOvercommit(t *testing.T) {
	f := newFixture(t, 25)
	f.provider.setLoseAcknowledgement(true)
	op := f.prepare(t, f.attempt.ID, "purchase-primary", 2)
	if _, err := f.box.Operations.Dispatch(f.ctx, op.ID, f.attempt.ID); !errors.Is(err, domain.ErrOutcomeUnknown) {
		t.Fatalf("dispatch err=%v, want ErrOutcomeUnknown", err)
	}
	available, err := f.box.Resources.Available(f.ctx, f.envelope)
	if err != nil {
		t.Fatal(err)
	}
	if available != 5 {
		t.Fatalf("available=%d, want 5 with unresolved 20 exposure", available)
	}
	_, err = reserveInOwnTx(f.ctx, f.box, f.envelope, 10, resources.Enforceability{
		CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "acceptance",
	})
	if !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("overcommit err=%v, want ErrBudgetExceeded", err)
	}
	dispatch, _ := f.provider.counts()
	if dispatch != 1 {
		t.Fatalf("provider dispatches=%d, want exactly 1", dispatch)
	}
}

func testStaleAttemptRejected(t *testing.T) {
	f := newFixture(t, 100)
	old := f.attempt
	_ = f.replacementAttempt(t)
	called := 0
	err := f.box.Execution.WithGuardedAttempt(f.ctx, old.ID, []domain.TaskState{domain.TaskExecuting}, func(*sql.Tx, execution.GuardedAttempt) error {
		called++
		return nil
	})
	if !errors.Is(err, domain.ErrStaleAttempt) || called != 0 {
		t.Fatalf("stale guard err=%v callback=%d", err, called)
	}
}

func testExpiredLeaseWithMatchingFenceRejected(t *testing.T) {
	f := newFixture(t, 100)
	f.clock.Advance(2 * time.Minute)
	called := 0
	err := f.box.Execution.WithGuardedAttempt(f.ctx, f.attempt.ID, []domain.TaskState{domain.TaskExecuting}, func(*sql.Tx, execution.GuardedAttempt) error {
		called++
		return nil
	})
	if !errors.Is(err, domain.ErrLeaseInactive) || called != 0 {
		t.Fatalf("expired guard err=%v callback=%d", err, called)
	}
	var currentFence int64
	if err := f.box.Store.DB().QueryRowContext(f.ctx, `SELECT current_fence FROM tasks WHERE task_id = ?`, f.task.ID).Scan(&currentFence); err != nil {
		t.Fatal(err)
	}
	if currentFence != f.attempt.FenceGeneration {
		t.Fatalf("test lost matching-fence premise: task=%d attempt=%d", currentFence, f.attempt.FenceGeneration)
	}
}

func testCrashBeforeCompletionRecordDoesNotFakeCompletion(t *testing.T) {
	f := newFixture(t, 100)
	if _, err := f.box.Evidence.Put(f.ctx, strings.NewReader("staged output"), evidence.Metadata{MediaType: "text/plain", Kind: "ATTEMPT_OUTPUT"}); err != nil {
		t.Fatal(err)
	}
	if err := f.box.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := summa42runtime.Open(f.ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	var taskState domain.TaskState
	var completions int
	if err := restarted.Store.DB().QueryRowContext(f.ctx, `SELECT state FROM tasks WHERE task_id = ?`, f.task.ID).Scan(&taskState); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM attempt_completion_records WHERE attempt_id = ?`, f.attempt.ID).Scan(&completions); err != nil {
		t.Fatal(err)
	}
	if taskState != domain.TaskExecuting || completions != 0 {
		t.Fatalf("restart inferred completion: task=%s completions=%d", taskState, completions)
	}
}

func testCompletionRecordResumesVerification(t *testing.T) {
	f := newFixture(t, 100)
	ev, err := f.box.Evidence.Put(f.ctx, strings.NewReader("completed output"), evidence.Metadata{MediaType: "text/plain", Kind: "ATTEMPT_OUTPUT"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.box.Verification.CompleteAttempt(f.ctx, f.attempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.box.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := summa42runtime.Open(f.ctx, f.config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	var taskState domain.TaskState
	var pending int
	if err := restarted.Store.DB().QueryRowContext(f.ctx, `SELECT state FROM tasks WHERE task_id = ?`, f.task.ID).Scan(&taskState); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM verification_work WHERE task_id = ? AND state = 'PENDING'`, f.task.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if taskState != domain.TaskAwaitingVerification || pending != 1 {
		t.Fatalf("restart task=%s pending=%d", taskState, pending)
	}
}

func testCapabilityBypassBlockedInEnforcedProfile(t *testing.T) {
	f := newFixture(t, 100)
	if _, err := f.box.Capabilities.AssessProvider(f.ctx, f.caps.Name()); err != nil {
		t.Fatal(err)
	}
	task, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: f.missionID},
		AcceptanceCriteria:   []string{"capability access mediated"},
		RequiredCapabilities: []string{"safe.read"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"safe.read"},
		ResourceEnvelopeID:   f.envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := f.box.Execution.StartAttempt(f.ctx, task.ID, "capability-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.box.Capabilities.OpenSession(f.ctx, f.config.CollectiveID, attempt.ID, []string{"safe.read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(f.ctx, "unsafe.exec", map[string]any{"cmd": "escape"}); !errors.Is(err, domain.ErrPolicyDenied) {
		t.Fatalf("bypass err=%v, want policy denial", err)
	}
	if calls := f.caps.callCount(); calls != 0 {
		t.Fatalf("provider calls=%d, bypass reached provider", calls)
	}
}

func testConstraintPrecedenceBlocksIllegalGoalPath(t *testing.T) {
	f := newFixture(t, 100)
	parent, err := f.box.Execution.CreateTask(f.ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: f.missionID},
		AcceptanceCriteria:   []string{"stay within authority"},
		RequiredCapabilities: []string{"read"},
		RequiredEnforcement:  domain.EnforcementPartial,
		AuthorityCeiling:     []string{"read"},
		ResourceEnvelopeID:   f.envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.box.Execution.CreateChildTask(f.ctx, parent.ID, execution.TaskRequest{
		AcceptanceCriteria:   []string{"illegal escalation"},
		RequiredCapabilities: []string{"admin"},
	})
	if err == nil {
		t.Fatal("child path widened higher-level authority")
	}
	var children int
	if err := f.box.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM tasks WHERE parent_task_id = ?`, parent.ID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("illegal child persisted: %d", children)
	}
}

func testHardBudgetRequiresEnforceableCeiling(t *testing.T) {
	f := newFixture(t, 100)
	_, err := reserveInOwnTx(f.ctx, f.box, f.envelope, 10, resources.Enforceability{
		CostControl:    resources.CostEstimatedOnly,
		RequireHardCap: true,
		Source:         "model-estimate",
	})
	if !errors.Is(err, resources.ErrHardCapUnavailable) {
		t.Fatalf("reserve err=%v, want ErrHardCapUnavailable", err)
	}
	var reservations int
	if err := f.box.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM resource_reservations WHERE envelope_id = ?`, f.envelope).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 0 {
		t.Fatalf("unenforceable hard budget persisted %d reservations", reservations)
	}
}

func testConfirmedEffectSurvivesAttemptReplacement(t *testing.T) {
	f := newFixture(t, 100)
	op := f.prepare(t, f.attempt.ID, "purchase-primary", 1)
	confirmed, err := f.box.Operations.Dispatch(f.ctx, op.ID, f.attempt.ID)
	if err != nil || confirmed.State != domain.OperationConfirmedEffect {
		t.Fatalf("first dispatch state=%s err=%v", confirmed.State, err)
	}
	replacement := f.replacementAttempt(t)
	reused := f.prepare(t, replacement.ID, "purchase-primary", 1)
	if reused.ID != confirmed.ID || reused.State != domain.OperationConfirmedEffect {
		t.Fatalf("replacement did not reuse confirmed effect: first=%s/%s second=%s/%s", confirmed.ID, confirmed.State, reused.ID, reused.State)
	}
	dispatch, lookup := f.provider.counts()
	if dispatch != 1 || lookup != 0 {
		t.Fatalf("provider calls dispatch=%d lookup=%d, want 1/0", dispatch, lookup)
	}
}

func testEffectSlotParameterDriftConflicts(t *testing.T) {
	f := newFixture(t, 100)
	first := f.prepare(t, f.attempt.ID, "purchase-primary", 1)
	replacement := f.replacementAttempt(t)
	second, err := f.box.Operations.Prepare(f.ctx, operations.PrepareRequest{
		AttemptID:      replacement.ID,
		Provider:       f.provider.Name(),
		TrustedSlotKey: "purchase-primary",
		Intent:         testutil.PurchaseIntent{SKU: "sku-1", Quantity: 2},
		Risk:           "LOW",
	})
	if !errors.Is(err, domain.ErrIntentConflict) {
		t.Fatalf("drift err=%v, want ErrIntentConflict", err)
	}
	if second.EffectSlotID != "" && second.EffectSlotID != first.EffectSlotID {
		t.Fatalf("drift minted a second slot %s from %s", second.EffectSlotID, first.EffectSlotID)
	}
	var slots int
	if err := f.box.Store.DB().QueryRowContext(f.ctx,
		`SELECT count(*) FROM effect_slots WHERE task_id = ? AND trusted_slot_key = 'purchase-primary'`, f.task.ID).Scan(&slots); err != nil {
		t.Fatal(err)
	}
	if slots != 1 {
		t.Fatalf("effect slots=%d, want 1", slots)
	}
	dispatch, _ := f.provider.counts()
	if dispatch != 0 {
		t.Fatalf("drift triggered provider dispatch %d times", dispatch)
	}
}

func testPreparedRevocationPreventsDispatch(t *testing.T) {
	f := newFixture(t, 100)
	op := f.prepare(t, f.attempt.ID, "purchase-primary", 1)
	f.policy.set(domain.PolicyDeny, "policy-v2")
	if _, err := f.box.Store.DB().ExecContext(f.ctx, `UPDATE policy_sets SET policy_hash = 'policy-v2' WHERE active = 1`); err != nil {
		t.Fatal(err)
	}
	_, err := f.box.Operations.Dispatch(f.ctx, op.ID, f.attempt.ID)
	if !errors.Is(err, domain.ErrPolicyDenied) {
		t.Fatalf("dispatch err=%v, want policy denied", err)
	}
	var state domain.OperationState
	if err := f.box.Store.DB().QueryRowContext(f.ctx, `SELECT state FROM external_operations WHERE operation_id = ?`, op.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == domain.OperationDispatched {
		t.Fatal("revoked PREPARED operation crossed dispatch boundary")
	}
	dispatch, _ := f.provider.counts()
	if dispatch != 0 {
		t.Fatalf("provider calls=%d, want 0", dispatch)
	}
}

func testOPAHTTPSendRejected(t *testing.T) {
	module := `package summa42
	decision := {"outcome": "ALLOW", "reason_codes": ["unsafe"]}
	probe := http.send({"method": "get", "url": "https://example.invalid"}) if { false }`
	engine := policy.NewOPAEngine(policy.OPAConfig{ModuleName: "acceptance.rego", Module: module, PolicySetID: "policy_acceptance", Timeout: 250 * time.Millisecond})
	_, err := engine.Evaluate(context.Background(), policy.PolicyInput{Risk: "LOW", AuthorityValid: true, Now: acceptanceNow})
	if err == nil {
		t.Fatal("http.send policy compiled/evaluated under restricted capabilities")
	}
}

func testOPAErrorTimeoutInvalidOutputFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		module string
		ctx    context.Context
	}{
		{"runtime_error", `package summa42
	default decision := {"outcome": "DENY", "reason_codes": ["fallback"]}
	decision := {"outcome": "ALLOW", "reason_codes": ["bad"]} if { x := 1 / 0; x == 0 }`, context.Background()},
		{"undefined", `package summa42
	decision := {"outcome": "ALLOW", "reason_codes": ["bad"]} if { false }`, context.Background()},
		{"invalid_output", `package summa42
	decision := {"outcome": "MAYBE", "reason_codes": ["bad"]}`, context.Background()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := policy.NewOPAEngine(policy.OPAConfig{ModuleName: "acceptance.rego", Module: tc.module, PolicySetID: "policy_acceptance", Timeout: 250 * time.Millisecond})
			if _, err := engine.Evaluate(tc.ctx, policy.PolicyInput{Risk: "LOW", AuthorityValid: true, Now: acceptanceNow}); err == nil {
				t.Fatalf("%s authorized on evaluator failure", tc.name)
			}
		})
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	engine := policy.NewOPAEngine(policy.OPAConfig{
		ModuleName: "acceptance.rego",
		Module: `package summa42
	decision := {"outcome": "ALLOW", "reason_codes": ["bad"]}`,
		PolicySetID: "policy_acceptance",
		Timeout:     250 * time.Millisecond,
	})
	if _, err := engine.Evaluate(ctx, policy.PolicyInput{Risk: "LOW", AuthorityValid: true, Now: acceptanceNow}); err == nil {
		t.Fatal("expired evaluation context authorized policy")
	}
}
