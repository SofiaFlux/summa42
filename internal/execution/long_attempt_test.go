package execution_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/wake"
)

type longAttemptHarness struct {
	ctx         context.Context
	store       *state.Store
	clk         *testutil.Clock
	ownerID     domain.ID
	envelopeID  domain.ID
	missionID   domain.ID
	exec        *execution.Service
	resources   *resources.Service
	emergency   *emergency.Service
	containment *containment.Service
	evidence    *evidence.Store
	verify      *verification.Service
	wake        *wake.Service
}

func newLongAttemptHarness(t *testing.T, envelopeLimit int64) *longAttemptHarness {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC))
	ownerID := domain.ID("owner_long_attempt")
	nowStr := domain.FormatTime(clk.Now())
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		VALUES (?, 'HUMAN', X'00', 'LOCAL', ?) ON CONFLICT DO NOTHING`, ownerID, nowStr); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at)
		VALUES (1, X'00', 'hash-long', X'00', ?, 1, ?) ON CONFLICT DO NOTHING`, ownerID, nowStr); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at)
		VALUES (1, 'collective-1', ?, ?, ?, 1, ?) ON CONFLICT DO NOTHING`, ownerID, ownerID, ownerID, nowStr); err != nil {
		t.Fatal(err)
	}

	purposes := purpose.New(store, clk)
	missionID, err := purposes.CreateMission(ctx, "Long attempt lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	envelopeID := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelopeID, envelopeLimit, nowStr,
	); err != nil {
		t.Fatal(err)
	}

	execSvc := execution.New(store, clk, purposes)
	resSvc := resources.New(store, clk)
	emgSvc := emergency.NewService(store, clk, ownerID)
	if err := emgSvc.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	execSvc.SetLeaseGate(emgSvc)
	execSvc.SetEnvelopeChecker(resSvc)

	contSvc := containment.NewService(store, clk, ownerID)
	evStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	verifSvc := verification.New(store, clk, execSvc)
	schedSvc := scheduler.New(store, clk, purposes, execSvc, resSvc, 10*time.Minute)
	wakeSvc := wake.New(store, clk, schedSvc)

	return &longAttemptHarness{
		ctx: ctx, store: store, clk: clk, ownerID: ownerID, envelopeID: envelopeID, missionID: missionID,
		exec: execSvc, resources: resSvc, emergency: emgSvc, containment: contSvc,
		evidence: evStore, verify: verifSvc, wake: wakeSvc,
	}
}

func (h *longAttemptHarness) startAttempt(t *testing.T, lease time.Duration) (domain.Task, domain.Attempt) {
	t.Helper()
	task, err := h.exec.CreateTask(h.ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeMission, ID: h.missionID},
		Objective:            "Run bounded long attempt",
		AcceptanceCriteria:   []string{"verified completion evidence"},
		RequiredCapabilities: []string{"workspace.repo.write"},
		RequiredEnforcement:  domain.EnforcementPartial,
		ResourceEnvelopeID:   h.envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := h.exec.StartAttempt(h.ctx, task.ID, "agentic-test", lease)
	if err != nil {
		t.Fatal(err)
	}
	return task, attempt
}

func TestHeartbeatKeepsLeaseForHours(t *testing.T) {
	h := newLongAttemptHarness(t, 10)
	task, attempt := h.startAttempt(t, 10*time.Minute)
	initialFence := attempt.FenceGeneration

	const steps = 60 // 60 * 3m = 3 hours on a 10m lease
	for i := 0; i < steps; i++ {
		h.clk.Advance(3 * time.Minute)
		if err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute); err != nil {
			t.Fatalf("step %d RenewLease failed: %v", i, err)
		}
		got, err := h.exec.Attempt(h.ctx, attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.FenceGeneration != initialFence {
			t.Fatalf("step %d fence changed: got %d, want %d", i, got.FenceGeneration, initialFence)
		}
		if got.LeaseState != domain.LeaseActive {
			t.Fatalf("step %d lease state = %s, want ACTIVE", i, got.LeaseState)
		}
		wantExpiry := h.clk.Now().UTC().Add(10 * time.Minute)
		if !got.LeaseExpiresAt.Equal(wantExpiry) {
			t.Fatalf("step %d expiry = %s, want %s", i, got.LeaseExpiresAt, wantExpiry)
		}
	}

	ev, err := h.evidence.Put(h.ctx, strings.NewReader("verified 3h output"), evidence.Metadata{MediaType: "text/plain", Kind: "TEXT"})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := h.verify.CompleteAttempt(h.ctx, attempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}})
	if err != nil {
		t.Fatalf("CompleteAttempt after 3h heartbeat failed: %v", err)
	}
	if rec.TaskID != task.ID {
		t.Fatalf("completion TaskID = %s, want %s", rec.TaskID, task.ID)
	}
}

func TestMissedHeartbeatExpires(t *testing.T) {
	h := newLongAttemptHarness(t, 10)
	task, attempt := h.startAttempt(t, 10*time.Minute)

	h.clk.Advance(11 * time.Minute)
	if err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute); !errors.Is(err, domain.ErrLeaseInactive) {
		t.Fatalf("RenewLease after missed heartbeat = %v, want ErrLeaseInactive", err)
	}

	recovered, err := h.wake.RecoverExpiredLeases(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, want 1", recovered)
	}
	gotAttempt, err := h.exec.Attempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttempt.LeaseState != domain.LeaseExpired {
		t.Fatalf("attempt lease state = %s, want EXPIRED", gotAttempt.LeaseState)
	}
	gotTask, err := h.exec.Task(h.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotTask.State != domain.TaskEligible {
		t.Fatalf("task state = %s, want ELIGIBLE", gotTask.State)
	}
}

func TestFreezeStopsRenewal(t *testing.T) {
	t.Run("break glass freeze stops renewal and revokes lease", func(t *testing.T) {
		h := newLongAttemptHarness(t, 10)
		_, attempt := h.startAttempt(t, 10*time.Minute)
		beforeExpiry := attempt.LeaseExpiresAt

		if err := h.emergency.TriggerBreakGlass(h.ctx, h.ownerID, "stop long attempt"); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(2 * time.Minute)
		err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute)
		if err == nil {
			t.Fatal("RenewLease succeeded after break glass freeze")
		}
		got, err := h.exec.Attempt(h.ctx, attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LeaseState != domain.LeaseRevoked || !got.LeaseExpiresAt.Equal(beforeExpiry) {
			t.Fatalf("attempt after freeze: lease=%s expiry=%s, want REVOKED and unchanged %s", got.LeaseState, got.LeaseExpiresAt, beforeExpiry)
		}
	})

	t.Run("safe mode lease gate stops active lease renewal inside tx", func(t *testing.T) {
		h := newLongAttemptHarness(t, 10)
		// Set dead-man horizon to 1 hour so advancing 2 hours triggers safe mode
		// via LeaseGate without revoking the still-ACTIVE lease row first.
		if _, err := h.store.DB().ExecContext(h.ctx,
			`UPDATE dead_man_switch SET heartbeat_timeout_seconds = 3600 WHERE singleton = 1`,
		); err != nil {
			t.Fatal(err)
		}
		_, attempt := h.startAttempt(t, 3*time.Hour)
		beforeExpiry := attempt.LeaseExpiresAt

		h.clk.Advance(2 * time.Hour)
		err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute)
		if !errors.Is(err, emergency.ErrSafeModeRestricted) {
			t.Fatalf("RenewLease in safe mode = %v, want ErrSafeModeRestricted", err)
		}
		got, err := h.exec.Attempt(h.ctx, attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !got.LeaseExpiresAt.Equal(beforeExpiry) {
			t.Fatalf("safe mode renewal mutated expiry: got %s, want %s", got.LeaseExpiresAt, beforeExpiry)
		}
	})

	t.Run("containment terminate revokes lease and stops renewal", func(t *testing.T) {
		h := newLongAttemptHarness(t, 10)
		task, attempt := h.startAttempt(t, 10*time.Minute)
		if err := h.containment.ApplySignal(h.ctx, attempt.ID, task.ID, "egress_denied", "proxy", containment.StepTerminate, nil); err != nil {
			t.Fatal(err)
		}
		err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute)
		if !errors.Is(err, domain.ErrStaleAttempt) {
			t.Fatalf("RenewLease after containment terminate = %v, want ErrStaleAttempt", err)
		}
		got, err := h.exec.Attempt(h.ctx, attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LeaseState != domain.LeaseRevoked {
			t.Fatalf("attempt lease state after containment = %s, want REVOKED", got.LeaseState)
		}
	})
}

func TestRenewalRefusedWhenEnvelopeSpent(t *testing.T) {
	h := newLongAttemptHarness(t, 2)
	_, attempt := h.startAttempt(t, 10*time.Minute)

	// With 2 units remaining, renewal succeeds.
	h.clk.Advance(3 * time.Minute)
	if err := h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute); err != nil {
		t.Fatalf("RenewLease with remaining budget failed: %v", err)
	}
	afterGoodRenew, err := h.exec.Attempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Spend the entire envelope (2 units).
	if err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
		res, err := h.resources.ReserveInTx(h.ctx, tx, h.envelopeID, 2, resources.Enforceability{
			CostControl: resources.CostPrepaidQuota, RequireHardCap: true, Source: "profile:claude",
		})
		if err != nil {
			return err
		}
		return h.resources.SettleInTx(h.ctx, tx, res.ID, 2)
	}); err != nil {
		t.Fatal(err)
	}

	h.clk.Advance(3 * time.Minute)
	err = h.exec.RenewLease(h.ctx, attempt.ID, 10*time.Minute)
	if !errors.Is(err, execution.ErrEnvelopeExhausted) {
		t.Fatalf("RenewLease when envelope spent = %v, want ErrEnvelopeExhausted", err)
	}

	got, err := h.exec.Attempt(h.ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LeaseExpiresAt.Equal(afterGoodRenew.LeaseExpiresAt) {
		t.Fatalf("refused renewal mutated lease expiry: got %s, want %s", got.LeaseExpiresAt, afterGoodRenew.LeaseExpiresAt)
	}
}

func TestExpiredAttemptCannotRecordResult(t *testing.T) {
	h := newLongAttemptHarness(t, 10)
	task, expiredAttempt := h.startAttempt(t, 10*time.Minute)
	ev, err := h.evidence.Put(h.ctx, strings.NewReader("late output"), evidence.Metadata{MediaType: "text/plain", Kind: "TEXT"})
	if err != nil {
		t.Fatal(err)
	}

	h.clk.Advance(11 * time.Minute)
	if _, err := h.verify.CompleteAttempt(h.ctx, expiredAttempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); !errors.Is(err, domain.ErrLeaseInactive) {
		t.Fatalf("CompleteAttempt on expired attempt = %v, want ErrLeaseInactive", err)
	}
	var completions int
	if err := h.store.DB().QueryRowContext(h.ctx,
		`SELECT count(*) FROM attempt_completion_records WHERE attempt_id = ?`, expiredAttempt.ID,
	).Scan(&completions); err != nil {
		t.Fatal(err)
	}
	if completions != 0 {
		t.Fatalf("completions = %d, want 0 for expired attempt", completions)
	}

	// Revoked attempt cannot record a result either.
	if _, err := h.wake.RecoverExpiredLeases(h.ctx); err != nil {
		t.Fatal(err)
	}
	revokedAttempt, err := h.exec.StartAttempt(h.ctx, task.ID, "agentic-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.exec.RevokeLease(h.ctx, revokedAttempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.verify.CompleteAttempt(h.ctx, revokedAttempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); !errors.Is(err, domain.ErrLeaseInactive) {
		t.Fatalf("CompleteAttempt on revoked attempt = %v, want ErrLeaseInactive", err)
	}

	// Contained attempt cannot record a result either.
	containedAttempt, err := h.exec.StartAttempt(h.ctx, task.ID, "agentic-test", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.containment.ApplySignal(h.ctx, containedAttempt.ID, task.ID, "egress_denied", "proxy", containment.StepTerminate, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.verify.CompleteAttempt(h.ctx, containedAttempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{ev.ID}}); !errors.Is(err, domain.ErrStaleAttempt) {
		t.Fatalf("CompleteAttempt on contained attempt = %v, want ErrStaleAttempt", err)
	}
}
