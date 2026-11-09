package invariant_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/attention"
	"github.com/SofiaFlux/summa42/internal/company"
	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/invariant"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type mockClock struct {
	now time.Time
}

func (m *mockClock) Now() time.Time {
	return m.now
}

func (m *mockClock) Set(t time.Time) {
	m.now = t
}

type mockContainmentEmitter struct {
	calls []containmentCall
}

type containmentCall struct {
	AttemptID  domain.ID
	TaskID     domain.ID
	SignalName string
	EnforcedBy string
	Step       containment.LadderStep
	Details    map[string]any
}

func (m *mockContainmentEmitter) ApplySignal(ctx context.Context, attemptID, taskID domain.ID, signalName, enforcedBy string, step containment.LadderStep, details map[string]any) error {
	m.calls = append(m.calls, containmentCall{
		AttemptID:  attemptID,
		TaskID:     taskID,
		SignalName: signalName,
		EnforcedBy: enforcedBy,
		Step:       step,
		Details:    details,
	})
	return nil
}

type suite struct {
	store          *state.Store
	clock          *mockClock
	companySvc     *company.Service
	containmentSvc *mockContainmentEmitter
	attentionSvc   *attention.Service
	evidenceStore  *evidence.Store
	service        *invariant.Service
	tmpDir         string
}

func setupSuite(t *testing.T, initialTime time.Time) *suite {
	t.Helper()
	ctx := context.Background()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "summa42_test.db")
	evidencePath := filepath.Join(tmpDir, "evidence")

	store, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.DB().Close()
	})

	// Ensure resource_envelopes table has envelope_default
	_, _ = store.DB().ExecContext(ctx, `
		INSERT OR IGNORE INTO resource_envelopes (envelope_id, hard_limit, created_at)
		VALUES ('envelope_default', '{"cpu":1000}', '2026-11-09T00:00:00Z')`,
	)

	clk := &mockClock{now: initialTime}
	evStore, err := evidence.New(store, evidencePath, clk)
	if err != nil {
		t.Fatalf("new evidence store: %v", err)
	}

	companySvc := company.NewService(store, clk)
	mockCont := &mockContainmentEmitter{}
	attnSvc := attention.NewService(store, clk, "principal_owner")

	invSvc := invariant.NewService(store, clk, companySvc, mockCont, attnSvc, evStore)

	return &suite{
		store:          store,
		clock:          clk,
		companySvc:     companySvc,
		containmentSvc: mockCont,
		attentionSvc:   attnSvc,
		evidenceStore:  evStore,
		service:        invSvc,
		tmpDir:         tmpDir,
	}
}

func createEntityDirect(t *testing.T, s *suite, entityID domain.ID, kind company.EntityKind, name string, attrs map[string]any, exposure int64) {
	t.Helper()
	ctx := context.Background()
	attrsBytes, _ := json.Marshal(attrs)
	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO company_entities (
			entity_id, kind, name, status, attributes_json, exposure_amount, created_at, updated_at
		) VALUES (?, ?, ?, 'ACTIVE', ?, ?, '2026-11-09T00:00:00Z', '2026-11-09T00:00:00Z')`,
		entityID, string(kind), name, string(attrsBytes), exposure,
	)
	if err != nil {
		t.Fatalf("insert entity: %v", err)
	}
}

// 1. Critical SLA / Contract Expiration Invariant: Approaching & Breached
func TestEvaluate_ExpirationInvariant_ApproachingAndBreached(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := setupSuite(t, now)
	ctx := context.Background()

	// Entity contract expiring in 12 hours
	contractID := domain.ID("ent_contract_acme")
	createEntityDirect(t, s, contractID, company.KindContract, "Enterprise SLA", map[string]any{
		"expires_at": now.Add(12 * time.Hour).Format(time.RFC3339),
	}, 10000)

	// Register expiration invariant with 24h lead window
	_, err := s.service.RegisterInvariant(ctx, invariant.BusinessInvariant{
		Name:        "Contract SLA Expiry Guard",
		Description: "Warns if contract expires within 24h and requires renewal task",
		TargetKind:  string(company.KindContract),
		RuleType:    invariant.RuleExpiration,
		RuleConfig: invariant.RuleConfig{
			LeadWindowSeconds: 86400, // 24 hours
		},
		Severity:   invariant.SeverityWarning,
		ActionType: invariant.ActionRemediationTask,
		ActionConfig: invariant.ActionConfig{
			RemediationTaskTitle:     "Renew Contract",
			RemediationTaskStatement: "Negotiate and renew contract SLA with Acme",
		},
	})
	if err != nil {
		t.Fatalf("register invariant: %v", err)
	}

	// 1st Evaluation: Approaching breach (expires in 12h, within 24h lead window)
	evals, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("evaluate all: %v", err)
	}
	if len(evals) != 1 {
		t.Fatalf("expected 1 evaluation, got %d", len(evals))
	}
	if evals[0].Status != invariant.StatusApproachingBreach {
		t.Fatalf("expected APPROACHING_BREACH, got %s", evals[0].Status)
	}
	if evals[0].TriggeredTaskID == "" {
		t.Fatalf("expected remediation task to be triggered")
	}
	firstTaskID := evals[0].TriggeredTaskID

	// Verify remediation task was stored in tasks table with RECOVERY purpose
	var purposeKind, taskState string
	err = s.store.DB().QueryRowContext(ctx, `SELECT purpose_kind, state FROM tasks WHERE task_id = ?`, firstTaskID).Scan(&purposeKind, &taskState)
	if err != nil {
		t.Fatalf("query task: %v", err)
	}
	if purposeKind != "RECOVERY" || taskState != "ELIGIBLE" {
		t.Fatalf("expected RECOVERY ELIGIBLE task, got %s %s", purposeKind, taskState)
	}

	// Advance time past expiry -> Full breach
	expiredTime := now.Add(13 * time.Hour)
	s.clock.Set(expiredTime)

	evals2, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("evaluate all 2: %v", err)
	}
	if len(evals2) != 1 {
		t.Fatalf("expected 1 evaluation, got %d", len(evals2))
	}
	if evals2[0].Status != invariant.StatusBreached {
		t.Fatalf("expected BREACHED, got %s", evals2[0].Status)
	}
	// Idempotency: the active remediation task should be reused
	if evals2[0].TriggeredTaskID != firstTaskID {
		t.Fatalf("expected active task to be reused, got %s vs %s", evals2[0].TriggeredTaskID, firstTaskID)
	}
}

// 2. Deterministic Financial Exposure / Cash Runway Threshold
func TestEvaluate_CashRunwayAndExposureLimit_Deterministic(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := setupSuite(t, now)
	ctx := context.Background()

	// Entity with cash runway of 1.5 months
	assetID := domain.ID("ent_treasury_1")
	createEntityDirect(t, s, assetID, company.KindAsset, "Corporate Treasury", map[string]any{
		"runway_months": 1.5,
	}, 500000)

	// Invariant: Cash runway must not drop below 3.0 months
	_, err := s.service.RegisterInvariant(ctx, invariant.BusinessInvariant{
		Name:        "Minimum Cash Runway",
		Description: "Cash runway must remain above 3 months",
		TargetKind:  string(company.KindAsset),
		RuleType:    invariant.RuleThreshold,
		RuleConfig: invariant.RuleConfig{
			MetricPath: "attributes.runway_months",
			Operator:   "<", // violated when runway < 3.0
			Threshold:  3.0,
		},
		Severity:   invariant.SeverityBreach,
		ActionType: invariant.ActionCombined,
	})
	if err != nil {
		t.Fatalf("register invariant: %v", err)
	}

	evals, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("evaluate all: %v", err)
	}
	if len(evals) != 1 {
		t.Fatalf("expected 1 eval, got %d", len(evals))
	}
	if evals[0].Status != invariant.StatusBreached {
		t.Fatalf("expected BREACHED, got %s", evals[0].Status)
	}
	if evals[0].EvidenceID == "" {
		t.Fatalf("expected evidence ID to be recorded")
	}

	// Verify evidence object exists and is cryptographically verified
	evObj, data, err := s.evidenceStore.Get(ctx, evals[0].EvidenceID)
	if err != nil {
		t.Fatalf("get evidence: %v", err)
	}
	if evObj.Kind != "INVARIANT_EVALUATION" {
		t.Fatalf("expected kind INVARIANT_EVALUATION, got %s", evObj.Kind)
	}
	if len(data) == 0 {
		t.Fatalf("expected non-empty evidence payload")
	}
	if evals[0].TriggeredPacketID == "" {
		t.Fatalf("expected attention decision packet for owner escalation")
	}
}

// 3. Severe Exposure Triggers Containment Ladder
func TestEvaluate_SevereExposure_TriggersContainmentLadder(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := setupSuite(t, now)
	ctx := context.Background()

	// High risk entity with exposure of $2,500,000 exceeding severe threshold ($1,000,000)
	riskID := domain.ID("ent_risk_subprime")
	createEntityDirect(t, s, riskID, company.KindRisk, "Counterparty Default Risk", map[string]any{}, 2500000)

	_, err := s.service.RegisterInvariant(ctx, invariant.BusinessInvariant{
		Name:        "Maximum Counterparty Risk Exposure",
		Description: "Total risk exposure must not exceed threshold; severe triggers RESTRICT",
		TargetKind:  string(company.KindRisk),
		RuleType:    invariant.RuleExposureLimit,
		RuleConfig: invariant.RuleConfig{
			Threshold:               500000,
			SevereExposureThreshold: 1000000,
		},
		Severity:   invariant.SeverityCritical,
		ActionType: invariant.ActionContainmentAlert,
		ActionConfig: invariant.ActionConfig{
			ContainmentStep:       containment.StepRestrict,
			ContainmentSignalName: "EXPOSURE_CAP_BREACH",
		},
	})
	if err != nil {
		t.Fatalf("register invariant: %v", err)
	}

	evals, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("evaluate all: %v", err)
	}
	if len(evals) != 1 {
		t.Fatalf("expected 1 eval, got %d", len(evals))
	}
	if evals[0].Status != invariant.StatusBreached {
		t.Fatalf("expected BREACHED, got %s", evals[0].Status)
	}
	if evals[0].ContainmentSignal != string(containment.StepRestrict) {
		t.Fatalf("expected containment signal RESTRICT, got %s", evals[0].ContainmentSignal)
	}

	// Verify containment service was invoked without coupling containment to company types
	if len(s.containmentSvc.calls) != 1 {
		t.Fatalf("expected 1 containment call, got %d", len(s.containmentSvc.calls))
	}
	call := s.containmentSvc.calls[0]
	if call.Step != containment.StepRestrict {
		t.Fatalf("expected RESTRICT step, got %s", call.Step)
	}
	if call.SignalName != "EXPOSURE_CAP_BREACH" {
		t.Fatalf("expected EXPOSURE_CAP_BREACH, got %s", call.SignalName)
	}
	if call.EnforcedBy != "INVARIANT_ENGINE" {
		t.Fatalf("expected enforced by INVARIANT_ENGINE, got %s", call.EnforcedBy)
	}
}

// 4. Backup Redundancy / Availability Invariant
func TestEvaluate_BackupRedundancy_BreachAndEscalation(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := setupSuite(t, now)
	ctx := context.Background()

	// Asset with only 1 backup replica (requires 3)
	assetID := domain.ID("ent_asset_db_primary")
	createEntityDirect(t, s, assetID, company.KindAsset, "Primary PostgreSQL Database", map[string]any{
		"redundancy_count": 1,
	}, 20000)

	_, err := s.service.RegisterInvariant(ctx, invariant.BusinessInvariant{
		Name:        "Database Backup Redundancy",
		Description: "Production databases must maintain at least 3 redundant replicas",
		TargetKind:  string(company.KindAsset),
		RuleType:    invariant.RuleRedundancy,
		RuleConfig: invariant.RuleConfig{
			MinRedundancy: 3,
		},
		Severity:   invariant.SeverityBreach,
		ActionType: invariant.ActionEscalationNotice,
		ActionConfig: invariant.ActionConfig{
			AttentionCategory: attention.CategorySafetyOverride,
			AttentionUrgency:  attention.UrgencyHigh,
		},
	})
	if err != nil {
		t.Fatalf("register invariant: %v", err)
	}

	evals, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("evaluate all: %v", err)
	}
	if len(evals) != 1 {
		t.Fatalf("expected 1 eval, got %d", len(evals))
	}
	if evals[0].Status != invariant.StatusBreached {
		t.Fatalf("expected BREACHED, got %s", evals[0].Status)
	}
	if evals[0].TriggeredPacketID == "" {
		t.Fatalf("expected attention decision packet to be generated")
	}

	// Verify packet was stored in owner_decision_packets
	var category, urgency, status string
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT category, urgency, status FROM owner_decision_packets WHERE packet_id = ?`,
		evals[0].TriggeredPacketID,
	).Scan(&category, &urgency, &status)
	if err != nil {
		t.Fatalf("query decision packet: %v", err)
	}
	if category != string(attention.CategorySafetyOverride) {
		t.Fatalf("expected category SAFETY_OVERRIDE, got %s", category)
	}
	if urgency != string(attention.UrgencyHigh) {
		t.Fatalf("expected urgency HIGH, got %s", urgency)
	}
	if status != string(attention.StatusPending) {
		t.Fatalf("expected PENDING packet, got %s", status)
	}
}

// 5. Idempotency: Multiple evaluation cycles do not spam tasks or owner attention packets
func TestEvaluate_Idempotency_NoDuplicateRemediationTasksOrPackets(t *testing.T) {
	now := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	s := setupSuite(t, now)
	ctx := context.Background()

	policyID := domain.ID("ent_policy_soc2")
	createEntityDirect(t, s, policyID, company.KindPolicy, "SOC2 Compliance Policy", map[string]any{
		"expires_at": now.Add(2 * time.Hour).Format(time.RFC3339),
	}, 0)

	_, err := s.service.RegisterInvariant(ctx, invariant.BusinessInvariant{
		Name:        "SOC2 Policy Validity Guard",
		Description: "SOC2 policy certification must be renewed before expiration",
		TargetKind:  string(company.KindPolicy),
		RuleType:    invariant.RuleExpiration,
		RuleConfig: invariant.RuleConfig{
			LeadWindowSeconds: 86400,
		},
		Severity:   invariant.SeverityBreach,
		ActionType: invariant.ActionCombined,
	})
	if err != nil {
		t.Fatalf("register invariant: %v", err)
	}

	// Cycle 1: creates remediation task & attention packet
	evals1, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("eval 1: %v", err)
	}
	task1 := evals1[0].TriggeredTaskID
	packet1 := evals1[0].TriggeredPacketID

	// Cycle 2: 10 minutes later
	s.clock.Set(now.Add(10 * time.Minute))
	evals2, err := s.service.EvaluateAll(ctx)
	if err != nil {
		t.Fatalf("eval 2: %v", err)
	}
	task2 := evals2[0].TriggeredTaskID
	packet2 := evals2[0].TriggeredPacketID

	if task1 != task2 {
		t.Fatalf("idempotency violation: task1=%s, task2=%s", task1, task2)
	}
	if packet1 != packet2 {
		t.Fatalf("idempotency violation: packet1=%s, packet2=%s", packet1, packet2)
	}

	// Verify total count in database
	var taskCount, packetCount int
	_ = s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE purpose_kind = 'RECOVERY'`).Scan(&taskCount)
	_ = s.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM owner_decision_packets`).Scan(&packetCount)

	if taskCount != 1 {
		t.Fatalf("expected exactly 1 task in tasks table, got %d", taskCount)
	}
	if packetCount != 1 {
		t.Fatalf("expected exactly 1 packet in owner_decision_packets, got %d", packetCount)
	}
}
