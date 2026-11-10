package dtu_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/dtu"
	_ "modernc.org/sqlite"
)

func TestBlueGreen_ConcurrentCorpusExecutionAndPromotion(t *testing.T) {
	ctx := context.Background()

	gate := dtu.NewPromotionGate(dtu.NewInMemoryPromotionStore())

	corpus := []dtu.ScenarioEvaluationRequest{
		{
			ScenarioID: "scen_1",
			TaskClass:  "data_ingest",
			RiskTier:   "tier_low",
			Capability: "storage.read",
		},
		{
			ScenarioID: "scen_2",
			TaskClass:  "data_ingest",
			RiskTier:   "tier_low",
			Capability: "storage.read",
		},
	}

	// Blue (Champion): Acceptance 100%, 0 violations, Cost 500, Latency 20ms
	blueExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{
			ScenarioID:           req.ScenarioID,
			TaskClass:            req.TaskClass,
			RiskTier:             req.RiskTier,
			Capability:           req.Capability,
			AcceptancePassed:     true,
			HardInvariantsPassed: true,
			CostTokens:           500,
			Latency:              20 * time.Millisecond,
		}
	}

	// Green (Challenger): Acceptance 100%, 0 violations, Cost 300 (superior), Latency 15ms
	greenExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{
			ScenarioID:           req.ScenarioID,
			TaskClass:            req.TaskClass,
			RiskTier:             req.RiskTier,
			Capability:           req.Capability,
			AcceptancePassed:     true,
			HardInvariantsPassed: true,
			CostTokens:           300,
			Latency:              15 * time.Millisecond,
		}
	}

	engine := dtu.NewBlueGreenEngine(blueExec, greenExec, gate)

	scope := dtu.PromotionScope{
		TaskClass:  "data_ingest",
		RiskTier:   "tier_low",
		Capability: "storage.read",
	}

	report, decision, err := engine.EvaluateAndDecide(ctx, corpus, "champion_v1", "challenger_v2", scope)
	if err != nil {
		t.Fatalf("evaluation failed: %v", err)
	}

	if decision.Status != dtu.PromotionStatusPromoted {
		t.Errorf("expected candidate to be PROMOTED, got %s: %s", decision.Status, decision.Reason)
	}
	if len(decision.Vetoes) != 0 {
		t.Errorf("expected 0 vetoes, got %v", decision.Vetoes)
	}
	if report.GreenAvgCost >= report.BlueAvgCost {
		t.Errorf("expected green cost to be lower")
	}

	// Verify durable provenance record
	records, err := gate.Store().ListByChallenger("challenger_v2")
	if err != nil {
		t.Fatalf("failed to query promotion records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 promotion record, got %d", len(records))
	}
	if records[0].EvidenceHash == "" {
		t.Errorf("expected non-empty evidence hash")
	}
	if records[0].Scope.TaskClass != "data_ingest" {
		t.Errorf("expected granular scope data_ingest, got %s", records[0].Scope.TaskClass)
	}
}

func TestBlueGreen_HardInvariantVeto(t *testing.T) {
	ctx := context.Background()

	gate := dtu.NewPromotionGate(dtu.NewInMemoryPromotionStore())

	corpus := []dtu.ScenarioEvaluationRequest{
		{
			ScenarioID: "scen_sec_1",
			TaskClass:  "code_exec",
			RiskTier:   "tier_high",
			Capability: "compute.exec",
		},
	}

	// Blue (Champion): Acceptance true, hard invariants true
	blueExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{
			ScenarioID:           req.ScenarioID,
			AcceptancePassed:     true,
			HardInvariantsPassed: true,
			CostTokens:           1000,
		}
	}

	// Green (Challenger): Acceptance true, Cost 200 (much better), BUT hard invariant violated!
	greenExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{
			ScenarioID:           req.ScenarioID,
			AcceptancePassed:     true,
			HardInvariantsPassed: false, // VIOLATION!
			InvariantsViolated:   []string{"AUTHORITY_CEILING_BREACH"},
			CostTokens:           200,
		}
	}

	engine := dtu.NewBlueGreenEngine(blueExec, greenExec, gate)

	scope := dtu.PromotionScope{
		TaskClass:  "code_exec",
		RiskTier:   "tier_high",
		Capability: "compute.exec",
	}

	_, decision, err := engine.EvaluateAndDecide(ctx, corpus, "champ_1", "challenger_regressed", scope)
	if err != nil {
		t.Fatalf("evaluation failed: %v", err)
	}

	// Hard Invariant VETO: candidate must be VETOED despite superior cost!
	if decision.Status != dtu.PromotionStatusVetoed {
		t.Fatalf("expected candidate to be VETOED due to hard invariant regression, got %s", decision.Status)
	}
	if len(decision.Vetoes) == 0 {
		t.Errorf("expected non-empty veto list")
	}

	// Verify recorded provenance confirms veto
	records, err := gate.Store().ListByChallenger("challenger_regressed")
	if err != nil {
		t.Fatalf("list records error: %v", err)
	}
	if len(records) != 1 || records[0].Decision.Status != dtu.PromotionStatusVetoed {
		t.Errorf("expected durable VETOED record, got %+v", records)
	}
}

func TestBlueGreen_ZeroAcceptanceAndLatencyVeto(t *testing.T) {
	ctx := context.Background()
	gate := dtu.NewPromotionGate(dtu.NewInMemoryPromotionStore())

	corpus := []dtu.ScenarioEvaluationRequest{
		{ScenarioID: "s1", TaskClass: "calc"},
	}

	// 1. Both fail acceptance (0%) -> MUST NOT PROMOTE
	blueExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{AcceptancePassed: false, HardInvariantsPassed: true, CostTokens: 100}
	}
	greenExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{AcceptancePassed: false, HardInvariantsPassed: true, CostTokens: 50}
	}

	engine := dtu.NewBlueGreenEngine(blueExec, greenExec, gate)
	_, decision, err := engine.EvaluateAndDecide(ctx, corpus, "champ", "chal_zero", dtu.PromotionScope{TaskClass: "calc"})
	if err != nil {
		t.Fatalf("evaluate error: %v", err)
	}
	if decision.Status != dtu.PromotionStatusVetoed {
		t.Errorf("expected VETOED for zero acceptance rate candidate, got %s", decision.Status)
	}

	// 2. Latency regression veto
	blueFast := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{AcceptancePassed: true, HardInvariantsPassed: true, CostTokens: 100, Latency: 10 * time.Millisecond}
	}
	greenSlow := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{AcceptancePassed: true, HardInvariantsPassed: true, CostTokens: 50, Latency: 50 * time.Millisecond} // 5x slower!
	}
	engineLatency := dtu.NewBlueGreenEngine(blueFast, greenSlow, gate)
	_, decLatency, err := engineLatency.EvaluateAndDecide(ctx, corpus, "champ", "chal_slow", dtu.PromotionScope{TaskClass: "calc"})
	if err != nil {
		t.Fatalf("evaluate latency error: %v", err)
	}
	if decLatency.Status != dtu.PromotionStatusVetoed {
		t.Errorf("expected VETOED for high latency regression, got %s", decLatency.Status)
	}
}

func TestBlueGreen_ShadowExecutionSideEffectContainment(t *testing.T) {
	ctx := context.Background()

	// Live environment provider with side-effect capabilities
	liveMeta := dtu.ProviderMetadata{
		ServiceID:  "billing",
		ProviderID: "billing-live",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}
	liveProv := dtu.NewMockEnvironmentProvider(liveMeta, dtu.EnvModeLive)

	shadowRouter := dtu.NewShadowExecutionRouter(liveProv)

	// 1. Authoritative (Blue) call has side-effect authority
	blueReq := dtu.InvocationRequest{
		Action:         "charge",
		IdempotencyKey: "charge_blue_1",
		Payload:        map[string]any{"amount": 100},
	}
	blueResp, err := shadowRouter.ExecuteAuthoritative(ctx, blueReq)
	if err != nil {
		t.Fatalf("authoritative execute failed: %v", err)
	}
	if !blueResp.Success {
		t.Errorf("expected authoritative call to succeed")
	}

	// 2. Non-authoritative (Green/Shadow) call attempting side-effects
	shadowReq := dtu.InvocationRequest{
		Action:         "charge",
		IdempotencyKey: "charge_shadow_1",
		Payload:        map[string]any{"amount": 100},
	}
	// Invariant: Shadow execution must NOT possess external side-effect authority!
	_, err = shadowRouter.ExecuteShadow(ctx, shadowReq)
	if !errors.Is(err, dtu.ErrShadowSideEffectDenied) {
		t.Fatalf("expected ErrShadowSideEffectDenied for shadow call attempting side effect, got: %v", err)
	}

	// Invariant verification: Verify through snapshot that the shadow side-effect was NEVER sent to live provider!
	snapBytes, err := liveProv.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	var snapData struct {
		CallHistory []dtu.InvocationRequest `json:"call_history"`
	}
	if err := json.Unmarshal(snapBytes, &snapData); err != nil {
		t.Fatalf("unmarshal snapshot error: %v", err)
	}
	for _, call := range snapData.CallHistory {
		if call.IdempotencyKey == "charge_shadow_1" {
			t.Fatalf("CONTAINMENT VIOLATION: shadow side effect 'charge_shadow_1' was executed on live provider!")
		}
	}

	// 3. Fail-closed: tricky unlisted action with 'get' in the name like 'get_and_drain' is blocked
	trickyReq := dtu.InvocationRequest{
		Action: "get_and_drain_account",
	}
	_, err = shadowRouter.ExecuteShadow(ctx, trickyReq)
	if !errors.Is(err, dtu.ErrShadowSideEffectDenied) {
		t.Errorf("expected fail-closed ErrShadowSideEffectDenied for unlisted action 'get_and_drain_account', got %v", err)
	}

	// 4. Read-only / Query action explicitly allowed in shadow mode
	queryReq := dtu.InvocationRequest{
		Action: "get_balance",
	}
	queryResp, err := shadowRouter.ExecuteShadow(ctx, queryReq)
	if err != nil {
		t.Fatalf("read-only shadow query failed: %v", err)
	}
	if !queryResp.Success {
		t.Errorf("expected query in shadow mode to succeed")
	}
}

func TestBlueGreen_SQLitePromotionStoreAndRollback(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	defer db.Close()

	store, err := dtu.NewSQLitePromotionStore(db)
	if err != nil {
		t.Fatalf("new sqlite store failed: %v", err)
	}
	gate := dtu.NewPromotionGate(store)

	record := dtu.PromotionProvenanceRecord{
		RecordID:     "promo_rec_1",
		ChampionID:   "champ_1",
		ChallengerID: "challenger_1",
		Decision: dtu.PromotionDecision{
			Status: dtu.PromotionStatusPromoted,
			Reason: "Passed all evaluations",
			Vetoes: []string{},
			Scope: dtu.PromotionScope{
				TaskClass: "ingest",
			},
		},
		EvidenceHash: "hash_evidence_123",
		Scope: dtu.PromotionScope{
			TaskClass: "ingest",
		},
		Timestamp: time.Now().UTC().Truncate(time.Second),
	}

	// 1. Save record
	if err := store.Save(record); err != nil {
		t.Fatalf("save record failed: %v", err)
	}

	// 2. Duplicate save rejection (append-only invariant)
	if err := store.Save(record); !errors.Is(err, dtu.ErrDuplicatePromotionRecord) {
		t.Errorf("expected ErrDuplicatePromotionRecord on duplicate insert, got %v", err)
	}

	// 3. Rollback validation: empty reason rejected
	if err := gate.Rollback("promo_rec_1", ""); !errors.Is(err, dtu.ErrRollbackReasonRequired) {
		t.Errorf("expected ErrRollbackReasonRequired on empty reason, got %v", err)
	}

	// 4. Valid rollback
	if err := gate.Rollback("promo_rec_1", "Production latency anomaly detected"); err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	// 5. Subsequent rollback rejected
	if err := gate.Rollback("promo_rec_1", "second rollback attempt"); !errors.Is(err, dtu.ErrInvalidRollbackState) {
		t.Errorf("expected ErrInvalidRollbackState on already rolled back record, got %v", err)
	}

	saved, err := store.Get("promo_rec_1")
	if err != nil {
		t.Fatalf("get record failed: %v", err)
	}
	if !saved.RolledBack {
		t.Errorf("expected record to be marked as RolledBack")
	}
	if saved.RollbackReason != "Production latency anomaly detected" {
		t.Errorf("expected rollback reason to match, got %s", saved.RollbackReason)
	}

	// 6. List by challenger
	list, err := store.ListByChallenger("challenger_1")
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 record in list, got %d", len(list))
	}
}

func TestBlueGreen_ScopeFilteringAndEmptyCorpus(t *testing.T) {
	ctx := context.Background()
	gate := dtu.NewPromotionGate(dtu.NewInMemoryPromotionStore())

	corpus := []dtu.ScenarioEvaluationRequest{
		{ScenarioID: "s1", TaskClass: "class_a"},
		{ScenarioID: "s2", TaskClass: "class_b"},
	}

	dummyExec := func(ctx context.Context, req dtu.ScenarioEvaluationRequest) dtu.ExecutionEvaluation {
		return dtu.ExecutionEvaluation{AcceptancePassed: true, HardInvariantsPassed: true}
	}

	engine := dtu.NewBlueGreenEngine(dummyExec, dummyExec, gate)

	// Scope for non-existent class_c in corpus must return ErrEmptyCorpus
	_, _, err := engine.EvaluateAndDecide(ctx, corpus, "champ", "chal", dtu.PromotionScope{TaskClass: "class_c"})
	if !errors.Is(err, dtu.ErrEmptyCorpus) {
		t.Errorf("expected ErrEmptyCorpus when no scenarios match scope, got %v", err)
	}
}
