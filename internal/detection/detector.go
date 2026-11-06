package detection

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// RogueSignalType represents deterministic security signal categories.
type RogueSignalType string

const (
	SignalUngrantedCapability    RogueSignalType = "UNGRANTED_CAPABILITY"
	SignalAuthorityCeilingBreach RogueSignalType = "AUTHORITY_CEILING_BREACH"
	SignalSandboxBoundaryBreach  RogueSignalType = "SANDBOX_BOUNDARY_BREACH"
	SignalCriticalControlEgress  RogueSignalType = "CRITICAL_CONTROL_EGRESS"
	SignalRevokedSessionReuse    RogueSignalType = "REVOKED_SESSION_REUSE"
	SignalStaleLeaseAccess       RogueSignalType = "STALE_LEASE_ACCESS"
	SignalAuditTampering         RogueSignalType = "AUDIT_TAMPERING_ATTEMPT"
	SignalRunawayDeniedLoop      RogueSignalType = "RUNAWAY_DENIED_LOOP"
)

type Severity string

const (
	SeverityLow      Severity = "LOW"
	SeverityMedium   Severity = "MEDIUM"
	SeverityHigh     Severity = "HIGH"
	SeverityCritical Severity = "CRITICAL"
)

// RogueSignal contains deterministic metadata about a detected security violation.
type RogueSignal struct {
	SignalType RogueSignalType `json:"signal_type"`
	Severity   Severity        `json:"severity"`
	AttemptID  domain.ID       `json:"attempt_id"`
	TaskID     domain.ID       `json:"task_id"`
	Target     string          `json:"target"`
	Details    map[string]any  `json:"details,omitempty"`
	DetectedAt time.Time       `json:"detected_at"`
}

type ContainmentApplier interface {
	ApplySignal(ctx context.Context, attemptID, taskID domain.ID, signalName, enforcedBy string, step containment.LadderStep, details map[string]any) error
}

type Detector struct {
	store       *state.Store
	clock       clock.Clock
	containment ContainmentApplier
	mu          sync.Mutex
	violations  map[domain.ID]int
}

func NewDetector(store *state.Store, clk clock.Clock, contApplier ContainmentApplier) *Detector {
	return &Detector{
		store:       store,
		clock:       clk,
		containment: contApplier,
		violations:  make(map[domain.ID]int),
	}
}

// IngestSignal processes a deterministic RogueSignal and applies the appropriate ladder escalation.
func (d *Detector) IngestSignal(ctx context.Context, sig RogueSignal) error {
	if sig.AttemptID == "" || sig.TaskID == "" || sig.SignalType == "" {
		return errors.New("attempt id, task id, and signal type are required")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Derive violation count directly from persisted containment audit events to survive process restarts
	count := 1
	if d.store != nil && d.store.DB() != nil {
		var persistentEvents int
		err := d.store.DB().QueryRowContext(ctx, `
			SELECT count(*) FROM attempt_containment_events WHERE attempt_id = ?`,
			sig.AttemptID,
		).Scan(&persistentEvents)
		if err == nil {
			count = persistentEvents + 1
		}
	}

	var step containment.LadderStep
	switch {
	case sig.Severity == SeverityCritical:
		step = containment.StepTerminate
	case sig.Severity == SeverityHigh:
		if count >= 2 {
			step = containment.StepTerminate
		} else {
			step = containment.StepRestrict
		}
	case sig.Severity == SeverityMedium:
		if count >= 3 {
			step = containment.StepTerminate
		} else if count >= 2 {
			step = containment.StepRestrict
		} else {
			step = containment.StepWarn
		}
	default:
		if count >= 4 {
			step = containment.StepTerminate
		} else if count >= 2 {
			step = containment.StepWarn
		} else {
			step = containment.StepObserve
		}
	}

	details := make(map[string]any)
	for k, v := range sig.Details {
		details[k] = v
	}
	details["severity"] = string(sig.Severity)
	details["target"] = sig.Target
	details["violation_count"] = count

	if d.containment != nil {
		return d.containment.ApplySignal(ctx, sig.AttemptID, sig.TaskID, string(sig.SignalType), "deterministic_detector", step, details)
	}
	return nil
}

// ReportUngrantedCapability is called when an attempt requests or executes an unassigned capability.
func (d *Detector) ReportUngrantedCapability(ctx context.Context, attemptID, taskID domain.ID, capability string) error {
	return d.IngestSignal(ctx, RogueSignal{
		SignalType: SignalUngrantedCapability,
		Severity:   SeverityMedium,
		AttemptID:  attemptID,
		TaskID:     taskID,
		Target:     capability,
		DetectedAt: d.clock.Now().UTC(),
	})
}

// ReportSandboxBoundaryViolation is called when an attempt targets unauthorized sockets or files.
func (d *Detector) ReportSandboxBoundaryViolation(ctx context.Context, attemptID, taskID domain.ID, target, reason string) error {
	sev := SeverityHigh
	sigType := SignalSandboxBoundaryBreach
	targetLower := strings.ToLower(target)
	// ponytail: substring match for well-known critical control targets; avoids complex regex on hot path
	if strings.Contains(targetLower, "docker.sock") ||
		strings.Contains(targetLower, "owner_key") ||
		strings.Contains(targetLower, "id_ed25519") ||
		strings.Contains(targetLower, "id_rsa") ||
		strings.Contains(targetLower, ".summa42/state") {
		sev = SeverityCritical
		sigType = SignalCriticalControlEgress
	}

	return d.IngestSignal(ctx, RogueSignal{
		SignalType: sigType,
		Severity:   sev,
		AttemptID:  attemptID,
		TaskID:     taskID,
		Target:     target,
		Details:    map[string]any{"reason": reason},
		DetectedAt: d.clock.Now().UTC(),
	})
}

// ReportRevokedSessionAccess is called when a session or attempt that has been revoked is invoked.
func (d *Detector) ReportRevokedSessionAccess(ctx context.Context, attemptID, taskID domain.ID, sessionID string) error {
	return d.IngestSignal(ctx, RogueSignal{
		SignalType: SignalRevokedSessionReuse,
		Severity:   SeverityHigh,
		AttemptID:  attemptID,
		TaskID:     taskID,
		Target:     sessionID,
		DetectedAt: d.clock.Now().UTC(),
	})
}

// ReportDeniedActionLoop is called when an attempt repeats denied actions or hits hard limits in loops.
func (d *Detector) ReportDeniedActionLoop(ctx context.Context, attemptID, taskID domain.ID, action string) error {
	return d.IngestSignal(ctx, RogueSignal{
		SignalType: SignalRunawayDeniedLoop,
		Severity:   SeverityMedium,
		AttemptID:  attemptID,
		TaskID:     taskID,
		Target:     action,
		DetectedAt: d.clock.Now().UTC(),
	})
}

// ReportAuditTampering is called when an operation attempts to bypass or tamper with evidence.
func (d *Detector) ReportAuditTampering(ctx context.Context, attemptID, taskID domain.ID, target string) error {
	return d.IngestSignal(ctx, RogueSignal{
		SignalType: SignalAuditTampering,
		Severity:   SeverityCritical,
		AttemptID:  attemptID,
		TaskID:     taskID,
		Target:     target,
		DetectedAt: d.clock.Now().UTC(),
	})
}
