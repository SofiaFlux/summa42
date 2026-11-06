package securityregression_test

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/detection"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
)

type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Now() time.Time {
	return c.now
}

func setupSecurityTestEnvironment(t *testing.T) (*state.Store, domain.ID, *containment.Service, *detection.Detector) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "security_test.db"))
	if err != nil {
		t.Fatalf("setup test store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.DB().Close()
	})

	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
	ownerID := domain.ID("principal_sec_owner")
	contSvc := containment.NewService(store, clk, ownerID)
	detector := detection.NewDetector(store, clk, contSvc)

	return store, ownerID, contSvc, detector
}

// 1. Invariant: Audit Trail Immutability (no-update, no-delete triggers must abort with specific message and leave rows intact)
func TestSecurityRegression_AuditTrailTamperResistance(t *testing.T) {
	store, ownerID, contSvc, _ := setupSecurityTestEnvironment(t)
	ctx := context.Background()
	now := "2026-11-06T12:00:00Z"

	db := store.DB()
	_, err := db.Exec(`
		INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at)
		VALUES (?, 'HUMAN', X'00', 'LOCAL', ?)`,
		ownerID, now,
	)
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	attemptID := domain.ID("att_tamper_1")
	taskID := domain.ID("task_tamper_1")

	_, _ = db.Exec(`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('env_t', 100, ?)`, now)
	_, _ = db.Exec(`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json, required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at) VALUES (?, 'MISSION', 'm1', 'EXECUTING', 0, '[]', '[]', 'ENFORCED', '[]', 'env_t', 0, ?, ?)`, taskID, now, now)
	_, _ = db.Exec(`INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind) VALUES (?, ?, 'RUNNING', 1, 'ACTIVE', '2026-11-06T13:00:00Z', ?, 'TEST')`, attemptID, taskID, now)

	// Apply signal to create audit event
	err = contSvc.ApplySignal(ctx, attemptID, taskID, "TEST_SIGNAL", "tester", containment.StepWarn, nil)
	if err != nil {
		t.Fatalf("apply signal: %v", err)
	}

	// Invariant: Direct UPDATE on attempt_containment_events must fail specifically on immutability trigger
	_, err = db.Exec(`UPDATE attempt_containment_events SET signal_name = 'SUPPRESSED' WHERE attempt_id = ?`, attemptID)
	if err == nil || !strings.Contains(err.Error(), "containment audit trail is immutable") {
		t.Fatalf("expected trigger abort 'containment audit trail is immutable', got %v", err)
	}

	// Invariant: Direct DELETE on attempt_containment_events must fail
	_, err = db.Exec(`DELETE FROM attempt_containment_events WHERE attempt_id = ?`, attemptID)
	if err == nil || !strings.Contains(err.Error(), "containment audit trail is immutable") {
		t.Fatalf("expected trigger abort 'containment audit trail is immutable', got %v", err)
	}

	// Invariant: Direct DELETE on attempt_containments must fail
	_, err = db.Exec(`DELETE FROM attempt_containments WHERE attempt_id = ?`, attemptID)
	if err == nil || !strings.Contains(err.Error(), "containment records cannot be deleted") {
		t.Fatalf("expected trigger abort 'containment records cannot be deleted', got %v", err)
	}

	// Verify original row data survived intact
	var signalName string
	err = db.QueryRow(`SELECT signal_name FROM attempt_containment_events WHERE attempt_id = ?`, attemptID).Scan(&signalName)
	if err != nil {
		t.Fatalf("query event row after tamper attempts: %v", err)
	}
	if signalName != "TEST_SIGNAL" {
		t.Fatalf("expected row data preserved as 'TEST_SIGNAL', got %s", signalName)
	}
}

// 2. Invariant: SSRF / Non-Public IP Rejection during Egress
func TestSecurityRegression_EgressRejectsWhitelistedDomainResolvingToPrivateIP(t *testing.T) {
	// Whitelisted host in policy
	policy := teb.EgressPolicy{
		AllowedHosts: map[string]struct{}{
			"api.partner.com": {},
		},
	}

	privateAddrs := []string{
		"169.254.169.254", // Cloud instance metadata
		"127.0.0.1",       // Local loopback
		"10.0.0.1",        // RFC1918 class A
		"192.168.1.1",     // RFC1918 class C
		"::1",             // IPv6 loopback
	}

	for _, addrStr := range privateAddrs {
		t.Run(addrStr, func(t *testing.T) {
			dialed := false
			lookup := func(context.Context, string) ([]netip.Addr, error) {
				// Simulated DNS rebinding or malicious DNS answering with non-public IP
				return []netip.Addr{netip.MustParseAddr(addrStr)}, nil
			}
			dial := func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, errors.New("dial should not be reached")
			}

			// Egress dial must reject non-public IP before connecting
			_, err := teb.DialAllowedTargetForTest(context.Background(), policy, "api.partner.com:443", lookup, dial)
			if err == nil {
				t.Fatalf("expected non-public IP %s to be denied, got nil", addrStr)
			}
			if dialed {
				t.Fatalf("dial was executed despite non-public IP %s", addrStr)
			}
		})
	}
}

// 3. Invariant: Authority and Capability Escalation Denied in Child Tasks
func TestSecurityRegression_ChildTaskEscalationDenied(t *testing.T) {
	store, ownerID, _, _ := setupSecurityTestEnvironment(t)
	ctx := context.Background()
	clk := &fixedClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}

	purpSvc := purpose.New(store, clk)
	execSvc := execution.New(store, clk, purpSvc, nil)
	now := "2026-11-06T12:00:00Z"

	db := store.DB()
	_, _ = db.Exec(`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES (?, 'HUMAN', X'00', 'LOCAL', ?)`, ownerID, now)
	_, _ = db.Exec(`INSERT INTO missions(mission_id, statement, active, created_at) VALUES ('m1', 'Mission 1', 1, ?)`, now)
	_, _ = db.Exec(`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('env_sec', 500, ?)`, now)

	parentTask, err := execSvc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: "MISSION", ID: "m1"},
		Objective:            "Parent Task",
		TaskClass:            "default",
		RequiredCapabilities: []string{"lostack.purchase"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"lostack.purchase"},
		ResourceEnvelopeID:   "env_sec",
		AcceptanceCriteria:   []string{"must succeed"},
	})
	if err != nil {
		t.Fatalf("create parent task: %v", err)
	}

	t.Run("RequiredCapabilityEscalation", func(t *testing.T) {
		_, err := execSvc.CreateChildTask(ctx, parentTask.ID, execution.TaskRequest{
			Objective:            "Child Task Exceeding Required Capabilities",
			RequiredCapabilities: []string{"host.root_exec"},
			AuthorityCeiling:     []string{"lostack.purchase"},
			AcceptanceCriteria:   []string{"test"},
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds parent authority ceiling") {
			t.Fatalf("expected capability escalation error, got %v", err)
		}
	})

	t.Run("AuthorityCeilingEscalation", func(t *testing.T) {
		_, err := execSvc.CreateChildTask(ctx, parentTask.ID, execution.TaskRequest{
			Objective:            "Child Task Exceeding Authority Ceiling",
			RequiredCapabilities: []string{"lostack.purchase"},
			AuthorityCeiling:     []string{"lostack.purchase", "admin.privilege"},
			AcceptanceCriteria:   []string{"test"},
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds parent authority ceiling") {
			t.Fatalf("expected authority ceiling escalation error, got %v", err)
		}
	})
}

// 4. Invariant: Sandbox Boundary Violations Trigger Complete Containment Fencing
func TestSecurityRegression_SandboxBoundaryProbingContained(t *testing.T) {
	store, _, contSvc, detector := setupSecurityTestEnvironment(t)
	ctx := context.Background()
	now := "2026-11-06T12:00:00Z"

	taskID := domain.ID("task_sandbox_sec")
	attemptID := domain.ID("att_sandbox_sec")

	db := store.DB()
	_, _ = db.Exec(`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES ('env_sb', 100, ?)`, now)
	_, _ = db.Exec(`INSERT INTO tasks(task_id, purpose_kind, purpose_id, state, current_fence, acceptance_criteria_json, required_capabilities_json, required_enforcement, authority_ceiling_json, resource_envelope_id, priority, created_at, updated_at) VALUES (?, 'MISSION', 'm1', 'EXECUTING', 0, '[]', '[]', 'ENFORCED', '[]', 'env_sb', 0, ?, ?)`, taskID, now, now)
	_, _ = db.Exec(`INSERT INTO attempts(attempt_id, task_id, state, fence_generation, lease_state, lease_expires_at, started_at, executor_kind) VALUES (?, ?, 'RUNNING', 1, 'ACTIVE', '2026-11-06T13:00:00Z', ?, 'TEST')`, attemptID, taskID, now)
	_, _ = db.Exec(`INSERT INTO capability_sessions(session_id, token_hash, collective_id, task_id, attempt_id, fence_generation, lease_expires_at, visible_capabilities_json, created_at) VALUES ('sess_sb', 'hash123', 'col1', ?, ?, 1, '2026-11-06T13:00:00Z', '{}', ?)`, taskID, attemptID, now)

	// Simulated probe on container daemon socket
	err := detector.ReportSandboxBoundaryViolation(ctx, attemptID, taskID, "/var/run/docker.sock", "exfiltration_attempt")
	if err != nil {
		t.Fatalf("report boundary violation: %v", err)
	}

	// 1. Attempt state must be SUSPECTED_ROGUE with investigation task
	rec, err := contSvc.GetRecord(ctx, attemptID)
	if err != nil {
		t.Fatalf("get record: %v", err)
	}
	if rec.LadderState != containment.StateSuspectedRogue {
		t.Fatalf("expected StateSuspectedRogue, got: %s", rec.LadderState)
	}
	if rec.InvestigationTaskID == "" {
		t.Fatal("expected InvestigationTaskID to be spawned")
	}

	// 2. Attempt operations must be blocked by CheckAttemptAllowed
	err = contSvc.CheckAttemptAllowed(ctx, attemptID, "external_operation")
	if !errors.Is(err, containment.ErrAttemptSuspectedRogue) {
		t.Fatalf("expected ErrAttemptSuspectedRogue, got %v", err)
	}

	// 3. Attempt lease must be REVOKED
	var leaseState string
	err = db.QueryRow(`SELECT lease_state FROM attempts WHERE attempt_id = ?`, attemptID).Scan(&leaseState)
	if err != nil || leaseState != "REVOKED" {
		t.Fatalf("expected lease_state REVOKED, got %s (err: %v)", leaseState, err)
	}

	// 4. Capability sessions must be REVOKED
	var revokedAt sql.NullString
	err = db.QueryRow(`SELECT revoked_at FROM capability_sessions WHERE session_id = 'sess_sb'`).Scan(&revokedAt)
	if err != nil || !revokedAt.Valid {
		t.Fatalf("expected capability session revoked_at to be populated, got: %v (err: %v)", revokedAt, err)
	}
}
