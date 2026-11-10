package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/emergency"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

type emergencyStatusDTO struct {
	BreakGlass      string `json:"break_glass"`
	FreezeReason    string `json:"freeze_reason,omitempty"`
	DeadMan         string `json:"dead_man"`
	LastHeartbeatAt string `json:"last_heartbeat_at"`
	HorizonSeconds  int    `json:"horizon_seconds"`
	SafeModeAt      string `json:"safe_mode_at"`
}

// newEmergencyCommand holds the Owner's emergency controls. Like policy
// upgrade, they write canonical state directly: a control that stops the
// Collective must not depend on the Box it stops, and the run-* workers run
// without the control daemon. Every change is signed with the Owner key.
func newEmergencyCommand(jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "emergency", Short: "Owner emergency controls: break glass and the dead man switch"}
	var home, ownerKey string
	command.PersistentFlags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	command.PersistentFlags().StringVar(&ownerKey, "owner-key", "", "path to the Owner Ed25519 private key (default: <home>/keys/owner.key)")

	withService := func(ctx context.Context, run func(*emergency.Service, localconfig.Config, string) error) error {
		resolvedHome, err := localconfig.ResolveHome(home)
		if err != nil {
			return err
		}
		cfg, err := localconfig.Load(resolvedHome)
		if err != nil {
			return fmt.Errorf("load local config: %w", err)
		}
		store, err := state.Open(ctx, cfg.DatabasePath)
		if err != nil {
			return err
		}
		defer store.DB().Close()
		service := emergency.NewService(store, clock.System{}, cfg.OwnerPrincipalID)
		if err := service.EnsureDefaults(ctx, 0); err != nil {
			return err
		}
		return run(service, cfg, resolvedHome)
	}
	apply := func(cmd *cobra.Command, action emergency.OwnerAction, reason string, timeout time.Duration) error {
		return withService(cmd.Context(), func(service *emergency.Service, cfg localconfig.Config, resolvedHome string) error {
			keyPath := strings.TrimSpace(ownerKey)
			if keyPath == "" {
				keyPath = filepath.Join(resolvedHome, "keys", "owner.key")
			}
			if _, err := os.Stat(keyPath); err != nil {
				return fmt.Errorf("Owner key must already exist: %w", err)
			}
			signer, err := identity.NewLocalEd25519(keyPath, "owner")
			if err != nil {
				return fmt.Errorf("load Owner key: %w", err)
			}
			signed, err := emergency.NewOwnerCommand(signer, cfg.CollectiveID, action, reason, int(timeout/time.Second), time.Now().UTC())
			if err != nil {
				return err
			}
			if err := service.ApplyOwnerCommand(cmd.Context(), signed); err != nil {
				return err
			}
			return printEmergencyStatus(cmd, service, *jsonOutput)
		})
	}

	status := &cobra.Command{
		Use: "status", Short: "Show break glass and dead man switch state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withService(cmd.Context(), func(service *emergency.Service, _ localconfig.Config, _ string) error {
				return printEmergencyStatus(cmd, service, *jsonOutput)
			})
		},
	}
	var freezeReason, resumeReason string
	freeze := &cobra.Command{
		Use: "freeze", Short: "Break glass: revoke every lease and session and refuse new work", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return apply(cmd, emergency.ActionFreeze, freezeReason, 0)
		},
	}
	freeze.Flags().StringVar(&freezeReason, "reason", "", "why the Collective is frozen (recorded in the audit trail)")
	resume := &cobra.Command{
		Use: "resume", Short: "Lift break glass", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return apply(cmd, emergency.ActionResume, resumeReason, 0)
		},
	}
	resume.Flags().StringVar(&resumeReason, "reason", "", "why work may resume (recorded in the audit trail)")
	var horizon time.Duration
	heartbeat := &cobra.Command{
		Use: "heartbeat", Short: "Record the Owner's presence; --timeout sets how long the Collective works without it", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if horizon < 0 || (horizon != 0 && horizon < emergency.MinHeartbeatTimeout) {
				return errors.New("--timeout must be at least " + emergency.MinHeartbeatTimeout.String())
			}
			return apply(cmd, emergency.ActionHeartbeat, "", horizon)
		},
	}
	heartbeat.Flags().DurationVar(&horizon, "timeout", 0, "new Owner horizon, for example 168h (default: keep the stored one)")
	command.AddCommand(status, freeze, resume, heartbeat)
	return command
}

func printEmergencyStatus(cmd *cobra.Command, service *emergency.Service, asJSON bool) error {
	breakGlass, deadMan, err := service.GetStatus(cmd.Context())
	if err != nil {
		return err
	}
	horizon := time.Duration(deadMan.HeartbeatTimeoutSec) * time.Second
	safeModeAt := deadMan.LastHeartbeatAt.Add(horizon)
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(emergencyStatusDTO{
			BreakGlass: string(breakGlass.State), FreezeReason: breakGlass.FreezeReason, DeadMan: string(deadMan.State),
			LastHeartbeatAt: deadMan.LastHeartbeatAt.UTC().Format(time.RFC3339), HorizonSeconds: deadMan.HeartbeatTimeoutSec,
			SafeModeAt: safeModeAt.UTC().Format(time.RFC3339),
		})
	}
	out := cmd.OutOrStdout()
	line := "Break glass: " + string(breakGlass.State)
	if breakGlass.State == emergency.BreakGlassFrozen {
		line += " (" + breakGlass.FreezeReason + ")"
	}
	if _, err := fmt.Fprintln(out, line); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Dead man switch: %s\nLast Owner heartbeat: %s\nOwner horizon: %s (safe mode at %s)\n",
		deadMan.State, deadMan.LastHeartbeatAt.UTC().Format(time.RFC3339), horizon, safeModeAt.UTC().Format(time.RFC3339))
	return err
}
