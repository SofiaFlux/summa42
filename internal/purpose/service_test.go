package purpose

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

var purposeTestNow = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func newPurposeService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	store := testutil.OpenStore(t)
	return New(store, fixedClock{now: purposeTestNow}), context.Background()
}

func TestTaskPurposeRejectsUnknownReference(t *testing.T) {
	svc, ctx := newPurposeService(t)
	err := svc.ValidatePurpose(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: "mission_missing"})
	if !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("got %v, want ErrInvalidPurpose", err)
	}
}

func TestCreateMissionEnforcesSingleActiveMission(t *testing.T) {
	svc, ctx := newPurposeService(t)
	first, err := svc.CreateMission(ctx, "Build the MVC safely")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("mission id is empty")
	}
	if _, err := svc.CreateMission(ctx, "Competing active mission"); err == nil {
		t.Fatal("second active Mission must be rejected")
	}
}

func TestActiveMissionReturnsPersistedStatement(t *testing.T) {
	svc, ctx := newPurposeService(t)
	if _, _, err := svc.ActiveMission(ctx); err == nil {
		t.Fatal("missionless Collective reported an active Mission")
	}
	id, err := svc.CreateMission(ctx, "Maintain the MVC")
	if err != nil {
		t.Fatal(err)
	}
	gotID, statement, err := svc.ActiveMission(ctx)
	if err != nil || gotID != id || statement != "Maintain the MVC" {
		t.Fatalf("active Mission = %q %q, err=%v", gotID, statement, err)
	}
}

func TestGoalTracesToActiveMission(t *testing.T) {
	svc, ctx := newPurposeService(t)
	missionID, err := svc.CreateMission(ctx, "Build the MVC safely")
	if err != nil {
		t.Fatal(err)
	}
	goalID, err := svc.CreateGoal(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID}, "Implement durable purpose lineage")
	if err != nil {
		t.Fatal(err)
	}
	if goalID == "" {
		t.Fatal("goal id is empty")
	}
	if err := svc.DeactivateMission(ctx, missionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateGoal(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID}, "Work under retired Mission"); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("goal under inactive Mission: got %v, want ErrInvalidPurpose", err)
	}
}

func TestObligationSurvivesMissionDeactivation(t *testing.T) {
	svc, ctx := newPurposeService(t)
	missionID, err := svc.CreateMission(ctx, "Temporary Mission")
	if err != nil {
		t.Fatal(err)
	}
	obligationID, err := svc.CreateObligation(ctx, "contract", "Pay already-incurred supplier invoice")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeactivateMission(ctx, missionID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ValidatePurpose(ctx, domain.PurposeRef{Kind: domain.PurposeObligation, ID: obligationID}); err != nil {
		t.Fatalf("obligation became invalid after Mission deactivation: %v", err)
	}
}

func TestValidatePurposeAcceptsApprovedSystemClassesAndRejectsUnknownKind(t *testing.T) {
	svc, ctx := newPurposeService(t)
	for _, kind := range []domain.PurposeKind{
		domain.PurposeCollectiveMaintenance,
		domain.PurposeGovernance,
		domain.PurposeStrategicPulse,
		domain.PurposeRecovery,
		domain.PurposeOwnerDirective,
	} {
		if err := svc.ValidatePurpose(ctx, domain.PurposeRef{Kind: kind, ID: domain.ID("authority_ref")}); err != nil {
			t.Fatalf("approved purpose kind %q rejected: %v", kind, err)
		}
	}
	if err := svc.ValidatePurpose(ctx, domain.PurposeRef{Kind: domain.PurposeKind("INVENTED"), ID: "x"}); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("unknown purpose kind: got %v, want ErrInvalidPurpose", err)
	}
}

func TestSecondActiveMissionRefusedAndOwnerSwitchReplacesActiveMission(t *testing.T) {
	svc, ctx := newPurposeService(t)
	m1, err := svc.CreateMission(ctx, "Manage core engineering")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateMission(ctx, "Competing active mission"); err == nil {
		t.Fatal("second active Mission must be refused while first is active")
	}

	m2, err := svc.CreateStandbyMission(ctx, "Launch product ABC end-to-end")
	if err != nil {
		t.Fatalf("CreateStandbyMission failed: %v", err)
	}
	missions, err := svc.ListMissions(ctx)
	if err != nil || len(missions) != 2 {
		t.Fatalf("ListMissions = (%d, %v), want 2", len(missions), err)
	}

	if err := svc.SwitchMission(ctx, m2); err != nil {
		t.Fatalf("SwitchMission failed: %v", err)
	}
	activeID, stmt, err := svc.ActiveMission(ctx)
	if err != nil || activeID != m2 || stmt != "Launch product ABC end-to-end" {
		t.Fatalf("ActiveMission after switch = (%q, %q, %v), want %q", activeID, stmt, err, m2)
	}

	// Verify m1 is now deactivated and only m2 is active.
	if err := svc.ValidatePurpose(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: m1}); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("deactivated mission %q still valid: %v", m1, err)
	}
}
