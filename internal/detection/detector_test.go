package detection_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/detection"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	return c.now
}

func setupDetectionSuite(t *testing.T) (*state.Store, *containment.Service, *detection.Detector) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("principal_owner")
	contSvc := containment.NewService(store, clk, ownerID)
	detector := detection.NewDetector(store, clk, contSvc)

	return store, contSvc, detector
}

func seedAttempt(t *testing.T, store *state.Store, attemptID, taskID domain.ID) {
	t.Helper()
	now := "2026-11-06T12:00:00Z"
	_, err := store.DB().Exec(`
		INSERT INTO resource_envelopes (envelope_id, hard_limit, created_at)
		VALUES ('env_1', 1000, ?)`,
		now,
	)
	if err != nil {
		t.Fatalf("seed envelope: %v", err)
	}
	_, err = store.DB().Exec(`
		INSERT INTO tasks (
			task_id, purpose_kind, purpose_id, state,
			current_fence, acceptance_criteria_json, required_capabilities_json,
			required_enforcement, authority_ceiling_json, resource_envelope_id,
			priority, created_at, updated_at
		) VALUES (
			?, 'MISSION', 'm1', 'EXECUTING',
			0, '["ok"]', '["fs.read"]',
			'ENFORCED', '["fs.read"]', 'env_1',
			10, ?, ?
		)`,
		taskID, now, now,
	)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	_, err = store.DB().Exec(`
		INSERT INTO attempts (
			attempt_id, task_id, state, fence_generation,
			lease_state, lease_expires_at, started_at, executor_kind
		) VALUES (
			?, ?, 'RUNNING', 1,
			'ACTIVE', '2026-11-06T13:00:00Z', ?, 'TEST'
		)`,
		attemptID, taskID, now,
	)
	if err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
}

func TestDetector_DetectAuthorityWidening(t *testing.T) {
	ctx := context.Background()
	store, contSvc, detector := setupDetectionSuite(t)
	attemptID := domain.ID("att_det_1")
	taskID := domain.ID("task_det_1")
	seedAttempt(t, store, attemptID, taskID)

	// Single attempt to use ungranted capability -> WARN ladder step
	err := detector.ReportUngrantedCapability(ctx, attemptID, taskID, "host.root_exec")
	if err != nil {
		t.Fatalf("report ungranted capability: %v", err)
	}

	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateWarning {
		t.Fatalf("expected StateWarning, got %s", rec.LadderState)
	}

	// Repeated violations -> RESTRICT or TERMINATE
	for i := 0; i < 3; i++ {
		_ = detector.ReportUngrantedCapability(ctx, attemptID, taskID, "host.docker_socket")
	}

	rec, err = contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected StateSuspectedRogue after repeated violations, got %s", rec.LadderState)
	}
}

func TestDetector_DetectCriticalControlEgress(t *testing.T) {
	ctx := context.Background()
	store, contSvc, detector := setupDetectionSuite(t)
	attemptID := domain.ID("att_det_2")
	taskID := domain.ID("task_det_2")
	seedAttempt(t, store, attemptID, taskID)

	// Access attempt to owner private key or docker socket is instant TERMINATE
	err := detector.ReportSandboxBoundaryViolation(ctx, attemptID, taskID, "/var/run/docker.sock", "socket_probe")
	if err != nil {
		t.Fatalf("report boundary violation: %v", err)
	}

	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected instant StateSuspectedRogue on docker socket breach, got %s", rec.LadderState)
	}
}

func TestDetector_DetectStaleSessionReuse(t *testing.T) {
	ctx := context.Background()
	store, contSvc, detector := setupDetectionSuite(t)
	attemptID := domain.ID("att_det_3")
	taskID := domain.ID("task_det_3")
	seedAttempt(t, store, attemptID, taskID)

	// Stale / revoked session reuse attempt
	err := detector.ReportRevokedSessionAccess(ctx, attemptID, taskID, "session_revoked_123")
	if err != nil {
		t.Fatalf("report revoked session access: %v", err)
	}

	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateRestricted && rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected RESTRICTED or SUSPECTED_ROGUE, got %s", rec.LadderState)
	}
}
