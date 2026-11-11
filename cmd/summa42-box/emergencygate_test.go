package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/modelrun"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// The gate the Box registers reads the real emergency state: an Owner freeze
// through the emergency service is a definite refusal, a resume lifts it, and
// a Collective that cannot be read is an error (which the launcher treats as
// no).
func TestEmergencyGateFollowsBreakGlass(t *testing.T) {
	cfg := initializedCollective(t)
	ctx := context.Background()
	gate := emergencyGate(func() (string, localconfig.Config, error) { return "", cfg, nil })

	if err := gate(ctx); err != nil {
		t.Fatalf("gate on a running Collective: %v", err)
	}

	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	service := emergency.NewService(store, clock.System{}, cfg.OwnerPrincipalID)
	if err := service.EnsureDefaults(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := service.TriggerBreakGlass(ctx, cfg.OwnerPrincipalID, "test freeze"); err != nil {
		t.Fatal(err)
	}
	if err := gate(ctx); !errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("gate while frozen = %v, want modelrun.ErrRefused", err)
	}

	if err := service.ResumeSystem(ctx, cfg.OwnerPrincipalID, "test resume"); err != nil {
		t.Fatal(err)
	}
	if err := gate(ctx); err != nil {
		t.Fatalf("gate after resume: %v", err)
	}
}

func TestEmergencyGateCannotAnswerWithoutACollective(t *testing.T) {
	gate := emergencyGate(func() (string, localconfig.Config, error) {
		return "", localconfig.Config{}, errors.New("no config.json")
	})
	err := gate(context.Background())
	if err == nil {
		t.Fatal("the gate allowed a process although the Collective could not be loaded")
	}
	// Not a definite refusal: the launcher refuses at start and counts it
	// toward the limit for a running process.
	if errors.Is(err, modelrun.ErrRefused) {
		t.Fatalf("a gate that cannot answer reported a definite refusal: %v", err)
	}

	// A later check tries again and succeeds once the Collective is there.
	cfg := initializedCollective(t)
	calls := 0
	recovering := emergencyGate(func() (string, localconfig.Config, error) {
		if calls++; calls == 1 {
			return "", localconfig.Config{DatabasePath: filepath.Join(t.TempDir(), "missing", "dir", "state.db")}, errors.New("not yet")
		}
		return "", cfg, nil
	})
	if err := recovering(context.Background()); err == nil {
		t.Fatal("first check should fail")
	}
	if err := recovering(context.Background()); err != nil {
		t.Fatalf("second check: %v", err)
	}
}
