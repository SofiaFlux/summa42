package dtu_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/dtu"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	"github.com/SofiaFlux/summa42/internal/teb"
	"github.com/SofiaFlux/summa42/internal/testutil"
	_ "modernc.org/sqlite"
)

func TestDTU_FidelityValidation(t *testing.T) {
	validFidelities := []dtu.FidelityLevel{
		dtu.FidelityT0Contract,
		dtu.FidelityT1Behavioral,
		dtu.FidelityT2Adversarial,
		dtu.FidelityT3HighFidelity,
		dtu.FidelityT4Universe,
	}

	for _, f := range validFidelities {
		if !f.Valid() {
			t.Errorf("expected fidelity %s to be valid", f)
		}
	}

	invalidFidelities := []dtu.FidelityLevel{
		"",
		"T5_GOD_MODE",
		"PRODUCTION_EQUIVALENT",
	}

	for _, f := range invalidFidelities {
		if f.Valid() {
			t.Errorf("expected fidelity %s to be invalid", f)
		}
	}
}

func TestDTU_MockEnvironmentProviderLifecycle(t *testing.T) {
	ctx := context.Background()

	provider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:        "github",
		ProviderID:       "mock-github-twin",
		Version:          "1.0.0",
		Fidelity:         dtu.FidelityT2Adversarial,
		KnownDivergences: []string{"rate_limit_reset_instantaneous"},
	}, dtu.EnvModeTwin)

	// Verify metadata and mode
	meta := provider.Metadata()
	if meta.ServiceID != "github" || meta.ProviderID != "mock-github-twin" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
	if provider.Mode() != dtu.EnvModeTwin {
		t.Fatalf("expected TWIN mode, got %s", provider.Mode())
	}

	// 1. Initial execution
	req1 := dtu.InvocationRequest{
		Action: "issues.get",
		Payload: map[string]any{
			"number": 42,
		},
	}
	res1, err := provider.Execute(ctx, req1)
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	if !res1.Success {
		t.Errorf("expected initial execution to succeed")
	}

	// 2. Snapshot
	snap, err := provider.Snapshot(ctx)
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}
	if len(snap) == 0 {
		t.Fatalf("expected non-empty snapshot bytes")
	}

	// 3. Mutate provider state (inject fault)
	provider.SetFault("issues.get", errors.New("injected connection reset"))
	_, err = provider.Execute(ctx, req1)
	if err == nil {
		t.Fatalf("expected execution to fail due to injected fault")
	}

	// 4. Restore snapshot
	err = provider.Restore(ctx, snap)
	if err != nil {
		t.Fatalf("failed to restore snapshot: %v", err)
	}

	// Should succeed again after restoring snapshot
	resAfterRestore, err := provider.Execute(ctx, req1)
	if err != nil {
		t.Fatalf("unexpected error after restore: %v", err)
	}
	if !resAfterRestore.Success {
		t.Errorf("expected success after snapshot restore")
	}

	// 5. Reset mechanics with seed
	err = provider.Reset(ctx, 1337)
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if provider.Seed() != 1337 {
		t.Errorf("expected seed 1337, got %d", provider.Seed())
	}
}

func TestDTU_DeepCopyDataIsolation(t *testing.T) {
	ctx := context.Background()
	provider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "db",
		ProviderID: "db-twin",
		Version:    "1.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}, dtu.EnvModeTwin)

	provider.SetResponse("query", dtu.InvocationResponse{
		Success: true,
		Data: map[string]any{
			"key": "initial_value",
		},
	})

	res, err := provider.Execute(ctx, dtu.InvocationRequest{Action: "query"})
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}

	// Mutate caller's returned response map
	res.Data["key"] = "tampered_value"

	// Subsequent call must receive untampered initial value
	res2, err := provider.Execute(ctx, dtu.InvocationRequest{Action: "query"})
	if err != nil {
		t.Fatalf("execute error: %v", err)
	}
	if res2.Data["key"] != "initial_value" {
		t.Errorf("data map was mutated across calls: got %v, want initial_value", res2.Data["key"])
	}
}

func TestDTU_EnvironmentRouter_RoutingAndPolicy(t *testing.T) {
	ctx := context.Background()
	router := dtu.NewEnvironmentRouter(dtu.RouterConfig{
		DefaultMode: dtu.EnvModeTwin,
	})

	liveProvider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "storage-service",
		ProviderID: "live-s3-client",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT4Universe,
	}, dtu.EnvModeLive)

	twinProvider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "storage-service",
		ProviderID: "twin-s3-emulator",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}, dtu.EnvModeTwin)

	if err := router.RegisterProvider(liveProvider); err != nil {
		t.Fatalf("failed to register live provider: %v", err)
	}
	if err := router.RegisterProvider(twinProvider); err != nil {
		t.Fatalf("failed to register twin provider: %v", err)
	}

	// Duplicate registration must fail closed
	if err := router.RegisterProvider(twinProvider); !errors.Is(err, dtu.ErrProviderDuplicate) {
		t.Errorf("expected ErrProviderDuplicate on duplicate registration, got: %v", err)
	}

	// 1. Request TWIN
	pTwin, err := router.Route(ctx, "storage-service", dtu.EnvModeTwin)
	if err != nil {
		t.Fatalf("failed to route to TWIN: %v", err)
	}
	if pTwin.Mode() != dtu.EnvModeTwin {
		t.Errorf("expected TWIN provider, got %s", pTwin.Mode())
	}
	if pTwin.Metadata().ProviderID != "twin-s3-emulator" {
		t.Errorf("expected twin-s3-emulator, got %s", pTwin.Metadata().ProviderID)
	}

	// 2. Request LIVE with explicit permission
	pLive, err := router.Route(ctx, "storage-service", dtu.EnvModeLive)
	if err != nil {
		t.Fatalf("failed to route to LIVE: %v", err)
	}
	if pLive.Mode() != dtu.EnvModeLive {
		t.Errorf("expected LIVE provider, got %s", pLive.Mode())
	}

	// 3. Request unknown service -> fail closed
	_, err = router.Route(ctx, "non-existent-service", dtu.EnvModeTwin)
	if !errors.Is(err, dtu.ErrProviderNotFound) {
		t.Errorf("expected ErrProviderNotFound for missing service, got: %v", err)
	}

	// 4. Request unavailable mode for service -> fail closed, no silent fallback
	onlyTwinService := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "isolated-service",
		ProviderID: "twin-only",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT1Behavioral,
	}, dtu.EnvModeTwin)
	if err := router.RegisterProvider(onlyTwinService); err != nil {
		t.Fatalf("failed to register provider: %v", err)
	}

	_, err = router.Route(ctx, "isolated-service", dtu.EnvModeLive)
	if !errors.Is(err, dtu.ErrModeUnavailable) {
		t.Errorf("expected ErrModeUnavailable when live mode is absent, got: %v", err)
	}
}

func TestDTU_RunManifestProvenancePersistence(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC))

	rmSvc := runmanifest.New(store, runmanifest.StaticContext{
		Build: runmanifest.BuildMetadata{
			RuntimeVersion: "v1.0.0",
			RuntimeCommit:  "abc1234",
		},
		TEBProfile: teb.PartialProfile("oci-test", teb.GuaranteeNonRoot),
	})

	purposes := purpose.New(store, clk)
	missionID, err := purposes.CreateMission(ctx, "Test DTU provenance")
	if err != nil {
		t.Fatalf("create mission: %v", err)
	}

	execSvc := execution.New(store, clk, purposes, rmSvc)
	task, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:   []string{"output"},
		RequiredCapabilities: []string{},
		RequiredEnforcement:  domain.EnforcementUnenforced,
		AuthorityCeiling:     []string{},
		ResourceEnvelopeID:   "env_test",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	provider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:        "github",
		ProviderID:       "github-dtu-twin",
		Version:          "2.4.0",
		Fidelity:         dtu.FidelityT2Adversarial,
		KnownDivergences: []string{"webhooks_not_simulated", "secondary_rate_limits_disabled"},
	}, dtu.EnvModeTwin)

	envSnapshot := dtu.CreateEnvironmentSnapshot(provider, 4242)

	now := clk.Now().UTC()
	attemptID := domain.NewID("att")

	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempts (attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind)
		VALUES (?, ?, 'LEASED', 1, 'ACTIVE', ?, ?, 'agent')
	`, string(attemptID), string(task.ID), now.Add(time.Minute).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	attempt := domain.Attempt{
		ID:              attemptID,
		TaskID:          task.ID,
		FenceGeneration: 1,
		ExecutorKind:    "agent",
		StartedAt:       now,
	}

	err = rmSvc.RecordAttemptStartWithEnvironmentInTx(ctx, tx, attempt, task, &envSnapshot)
	if err != nil {
		t.Fatalf("RecordAttemptStartWithEnvironmentInTx failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Query persisted manifest and verify environment provenance
	record, err := rmSvc.Manifest(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("failed to retrieve manifest: %v", err)
	}

	if record.Manifest.Environment == nil {
		t.Fatalf("expected Environment in manifest, but was nil")
	}
	if record.Manifest.Environment.Mode != "TWIN" {
		t.Errorf("expected mode TWIN, got %s", record.Manifest.Environment.Mode)
	}
	if record.Manifest.Environment.ProviderID != "github-dtu-twin" {
		t.Errorf("expected provider ID github-dtu-twin, got %s", record.Manifest.Environment.ProviderID)
	}
	if record.Manifest.Environment.Fidelity != "T2_ADVERSARIAL" {
		t.Errorf("expected fidelity T2_ADVERSARIAL, got %s", record.Manifest.Environment.Fidelity)
	}
	if record.Manifest.Environment.ScenarioSeed != 4242 {
		t.Errorf("expected seed 4242, got %d", record.Manifest.Environment.ScenarioSeed)
	}
	if len(record.Manifest.Environment.DivergenceProfile) != 2 {
		t.Errorf("expected 2 divergences, got %d", len(record.Manifest.Environment.DivergenceProfile))
	}
}

func TestDTU_ConcurrentRoutingAndExecution(t *testing.T) {
	ctx := context.Background()
	router := dtu.NewEnvironmentRouter(dtu.RouterConfig{
		DefaultMode: dtu.EnvModeTwin,
	})

	provider := dtu.NewMockEnvironmentProvider(dtu.ProviderMetadata{
		ServiceID:  "shared-service",
		ProviderID: "concurrent-twin",
		Version:    "1.0.0",
		Fidelity:   dtu.FidelityT2Adversarial,
	}, dtu.EnvModeTwin)

	if err := router.RegisterProvider(provider); err != nil {
		t.Fatalf("failed to register provider: %v", err)
	}

	const concurrency = 20
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				p, err := router.Route(ctx, "shared-service", dtu.EnvModeTwin)
				if err != nil {
					t.Errorf("worker %d failed to route: %v", workerID, err)
					return
				}
				req := dtu.InvocationRequest{
					Action: "ping",
					Payload: map[string]any{
						"worker": workerID,
						"seq":    j,
					},
				}
				res, err := p.Execute(ctx, req)
				if err != nil || !res.Success {
					t.Errorf("worker %d execution failed: %v, resp: %+v", workerID, err, res)
					return
				}
			}
		}(i)
	}

	wg.Wait()
}
