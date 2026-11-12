package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/resources"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

// newEnvelopeCommand holds the Owner's budget controls. A resource envelope
// is the budget Tasks spend from, counted in units (bounded requests, not a
// currency). Creating one and changing its limit is signed with the Owner
// key; like the emergency commands these write the local database directly.
func newEnvelopeCommand(jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "envelope", Short: "Owner budget controls: create, re-limit and list resource envelopes"}
	var home, ownerKey string
	command.PersistentFlags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	command.PersistentFlags().StringVar(&ownerKey, "owner-key", "", "path to the Owner Ed25519 private key (default: <home>/keys/owner.key)")

	withService := func(ctx context.Context, run func(*resources.Service, localconfig.Config, string) error) error {
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
		return run(resources.New(store, clock.System{}), cfg, resolvedHome)
	}
	printEnvelopes := func(cmd *cobra.Command, envelopes []resources.Envelope) error {
		out := cmd.OutOrStdout()
		if *jsonOutput {
			encoder := json.NewEncoder(out)
			encoder.SetIndent("", "  ")
			return encoder.Encode(envelopes)
		}
		if len(envelopes) == 0 {
			fmt.Fprintln(out, "No envelopes.")
			return nil
		}
		table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(table, "ENVELOPE\tLIMIT\tAVAILABLE\tCREATED")
		for _, envelope := range envelopes {
			fmt.Fprintf(table, "%s\t%d\t%d\t%s\n", envelope.ID, envelope.HardLimit, envelope.Available, envelope.CreatedAt.UTC().Format(time.RFC3339))
		}
		return table.Flush()
	}
	apply := func(cmd *cobra.Command, action resources.GrantAction, id string, limit int64, reason string) error {
		return withService(cmd.Context(), func(service *resources.Service, cfg localconfig.Config, resolvedHome string) error {
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
			grant, err := resources.NewOwnerGrant(signer, cfg.CollectiveID, action, domain.ID(strings.TrimSpace(id)), limit, reason, time.Now().UTC())
			if err != nil {
				return err
			}
			envelope, err := service.ApplyOwnerGrant(cmd.Context(), grant)
			if err != nil {
				return err
			}
			return printEnvelopes(cmd, []resources.Envelope{envelope})
		})
	}

	var createLimit, setLimit int64
	var createReason, setReason string
	create := &cobra.Command{
		Use: "create <envelope-id>", Short: "Create an envelope with a limit in units (Owner-signed)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("limit") {
				return fmt.Errorf("envelope create requires --limit")
			}
			return apply(cmd, resources.GrantCreate, args[0], createLimit, createReason)
		},
	}
	create.Flags().Int64Var(&createLimit, "limit", 0, "hard limit in units")
	create.Flags().StringVar(&createReason, "reason", "", "why the budget is granted (recorded with the signature)")
	setLimitCommand := &cobra.Command{
		Use: "set-limit <envelope-id>", Short: "Change an envelope's limit; not below what it has already committed (Owner-signed)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("limit") {
				return fmt.Errorf("envelope set-limit requires --limit")
			}
			return apply(cmd, resources.GrantSetLimit, args[0], setLimit, setReason)
		},
	}
	setLimitCommand.Flags().Int64Var(&setLimit, "limit", 0, "new hard limit in units")
	setLimitCommand.Flags().StringVar(&setReason, "reason", "", "why the limit changes (recorded with the signature)")
	list := &cobra.Command{
		Use: "list", Short: "List envelopes with their limit and what is still available", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withService(cmd.Context(), func(service *resources.Service, _ localconfig.Config, _ string) error {
				envelopes, err := service.Envelopes(cmd.Context())
				if err != nil {
					return err
				}
				return printEnvelopes(cmd, envelopes)
			})
		},
	}
	command.AddCommand(create, setLimitCommand, list)
	return command
}
