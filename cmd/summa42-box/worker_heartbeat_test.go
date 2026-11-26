package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type systemExecutor struct {
	fn func(context.Context, executors.AttemptEnvelope) (executors.ExecutionResult, error)
}

func (s systemExecutor) Start(ctx context.Context, env executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	return s.fn(ctx, env)
}

func (s systemExecutor) ExecutionContract() executors.Contract {
	return executors.Contract{Capabilities: []string{"long-shell"}, Enforcement: domain.EnforcementPartial}
}

func TestWorkerHeartbeatFlags(t *testing.T) {
	t.Setenv("SUMMA42_MODEL_USER", "summa42-model")
	if _, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--heartbeat-interval=-1s"}); err == nil {
		t.Fatal("expected error for negative --heartbeat-interval")
	}
	if _, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--max-wall-time=-1s"}); err == nil {
		t.Fatal("expected error for negative --max-wall-time")
	}
	if _, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--max-wall-time=2h"}); err == nil {
		t.Fatal("expected error when --max-wall-time is set without --heartbeat-interval")
	}
	if _, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--heartbeat-interval=3m"}); err == nil {
		t.Fatal("expected error when --heartbeat-interval is set without --max-wall-time")
	}
	if _, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--lease-duration=10m", "--heartbeat-interval=6m", "--max-wall-time=2h"}); err == nil {
		t.Fatal("expected error when --heartbeat-interval exceeds half of --lease-duration")
	}
	wf, err := parseWorkerFlagsCtx(context.Background(), []string{"--workspace-root=/ws", "--lease-duration=10m", "--heartbeat-interval=3m", "--max-wall-time=2h"})
	if err != nil {
		t.Fatal(err)
	}
	if wf.heartbeatInterval != 3*time.Minute || wf.maxWallTime != 2*time.Hour {
		t.Fatalf("heartbeat=%s maxWall=%s, want 3m and 2h", wf.heartbeatInterval, wf.maxWallTime)
	}
}

func TestRuntimeBoxWiresEnvelopeCheckerToRenewLease(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC))
	dir := t.TempDir()
	const mod = `package summa42` + "\n" + `default decision := {"outcome": "DENY", "reason_codes": ["deny"]}`
	modSum := sha256.Sum256([]byte(mod))
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:        filepath.Join(dir, "state.db"),
		EvidencePath:     filepath.Join(dir, "evidence"),
		Clock:            clk,
		CollectiveID:     "collective-box-hb",
		OwnerPrincipalID: "owner-box-hb",
		PolicyEngine: policy.NewOPAEngine(policy.OPAConfig{
			ModuleName:    "summa42.rego",
			Module:        mod,
			PolicySetID:   "policy_test_v1",
			PolicySetHash: hex.EncodeToString(modSum[:]),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	nowStr := domain.FormatTime(clk.Now())
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES ('owner-box-hb', 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`,
		nowStr,
	); err != nil {
		t.Fatal(err)
	}
	missionID, err := box.Purpose.CreateMission(ctx, "Verify box envelope checker wiring")
	if err != nil {
		t.Fatal(err)
	}
	envelopeID := domain.NewID("envelope")
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 1, ?)`,
		envelopeID, nowStr,
	); err != nil {
		t.Fatal(err)
	}
	task, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		Objective:            "Renew while funded",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := box.Execution.StartAttempt(ctx, task.ID, "worker", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Execution.RenewLease(ctx, attempt.ID, 10*time.Minute); err != nil {
		t.Fatalf("RenewLease with funded envelope failed: %v", err)
	}
	if err := box.Store.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := box.Resources.ReserveInTx(ctx, tx, envelopeID, 1, resources.Enforceability{
			CostControl: resources.CostPrepaidQuota, RequireHardCap: true, Source: "profile:claude",
		})
		if err != nil {
			return err
		}
		return box.Resources.SettleInTx(ctx, tx, res.ID, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.Execution.RenewLease(ctx, attempt.ID, 10*time.Minute); !errors.Is(err, execution.ErrEnvelopeExhausted) {
		t.Fatalf("RenewLease when spent = %v, want ErrEnvelopeExhausted", err)
	}
}

func TestRunWorkerHeartbeatEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const mod = `package summa42` + "\n" + `default decision := {"outcome": "DENY", "reason_codes": ["deny"]}`
	modSum := sha256.Sum256([]byte(mod))

	// Use the real wall clock (clock.System) and real time.Ticker so wall-clock
	// heartbeat renewals keep a 120ms lease alive across a 280ms execution.
	exec := systemExecutor{fn: func(_ context.Context, _ executors.AttemptEnvelope) (executors.ExecutionResult, error) {
		time.Sleep(280 * time.Millisecond)
		return executors.ExecutionResult{
			ExitCode: 0,
			Stdout:   "wall-clock renewed",
			Evidence: []executors.Evidence{{Kind: executors.EvidenceAgentMessage, Content: "done"}},
		}, nil
	}}

	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:        filepath.Join(dir, "state.db"),
		EvidencePath:     filepath.Join(dir, "evidence"),
		Clock:            clock.System{},
		CollectiveID:     "collective-e2e-hb",
		OwnerPrincipalID: "owner-e2e-hb",
		LeaseDuration:    120 * time.Millisecond,
		Executors:        map[string]executors.Executor{"long-shell": exec},
		PolicyEngine: policy.NewOPAEngine(policy.OPAConfig{
			ModuleName:    "summa42.rego",
			Module:        mod,
			PolicySetID:   "policy_test_v1",
			PolicySetHash: hex.EncodeToString(modSum[:]),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	nowStr := domain.FormatTime(time.Now().UTC())
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES ('owner-e2e-hb', 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`,
		nowStr,
	); err != nil {
		t.Fatal(err)
	}
	missionID, err := box.Purpose.CreateMission(ctx, "Wall clock worker heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	envelopeID := domain.NewID("envelope")
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 5, ?)`,
		envelopeID, nowStr,
	); err != nil {
		t.Fatal(err)
	}
	task, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		Objective:            "survive past 120ms lease",
		AcceptanceCriteria:   []string{"done"},
		RequiredCapabilities: []string{"long-shell"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}

	worker, err := scheduler.NewWorker(box.Scheduler, box.Execution, box.Evidence, box.Verification, box.Executors, box.Clock, filepath.Join(dir, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	worker.WithLeaseHeartbeat(30*time.Millisecond, 2*time.Second)

	capSnap, err := workerCapacity(box)
	if err != nil {
		t.Fatal(err)
	}
	res, err := worker.StepOnce(ctx, capSnap)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != scheduler.StepCompleted || res.TaskID != task.ID {
		t.Fatalf("outcome=%s task=%s, want COMPLETED and %s (wall-clock heartbeat should have kept lease alive past 120ms)", res.Outcome, res.TaskID, task.ID)
	}
}
