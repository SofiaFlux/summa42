package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/modelrun"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// registerEmergencyGate makes every process the launcher starts (model calls
// and validation runs, in every summa42-box command) obey the Owner's break
// glass: it is refused while the Collective is FROZEN, and one that is already
// running is killed within the launcher's check interval. Without a readable
// emergency state the answer is no.
func registerEmergencyGate() {
	modelrun.SetGate(emergencyGate(func() (string, localconfig.Config, error) {
		home, err := localconfig.ResolveHome("")
		if err != nil {
			return "", localconfig.Config{}, err
		}
		cfg, err := localconfig.Load(home)
		return home, cfg, err
	}))
}

// emergencyGate opens the state on first use and keeps it; a failure to open
// is not remembered, so the next check tries again.
func emergencyGate(load func() (string, localconfig.Config, error)) modelrun.Gate {
	var mu sync.Mutex
	var service *emergency.Service
	return func(ctx context.Context) error {
		mu.Lock()
		if service == nil {
			_, cfg, err := load()
			if err != nil {
				mu.Unlock()
				return fmt.Errorf("emergency gate: load the Collective: %w", err)
			}
			store, err := state.Open(ctx, cfg.DatabasePath)
			if err != nil {
				mu.Unlock()
				return fmt.Errorf("emergency gate: open the state: %w", err)
			}
			service = emergency.NewService(store, clock.System{}, cfg.OwnerPrincipalID)
		}
		current := service
		mu.Unlock()
		err := current.CheckExecutionAllowed(ctx, "model_process")
		if errors.Is(err, emergency.ErrBreakGlassFrozen) {
			return fmt.Errorf("%w: %v", modelrun.ErrRefused, err)
		}
		return err
	}
}
