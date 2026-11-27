package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/project"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

func loadOwnerSigner(ownerKey string) (identity.Signer, error) {
	keyPath := strings.TrimSpace(ownerKey)
	if keyPath == "" {
		home, err := localconfig.ResolveHome("")
		if err != nil {
			return nil, err
		}
		keyPath = filepath.Join(home, "keys", "owner.key")
	}
	if _, err := os.Stat(keyPath); err != nil {
		return nil, fmt.Errorf("Owner key must already exist: %w", err)
	}
	signer, err := identity.NewLocalEd25519(keyPath, "owner")
	if err != nil {
		return nil, fmt.Errorf("load Owner key: %w", err)
	}
	return signer, nil
}

func newProjectCommand(api control.API, jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "project", Short: "Manage Projects inside the Collective"}

	var name, reason, ownerKey string
	create := &cobra.Command{
		Use:   "create <slug>",
		Short: "Create a Project with an Owner signature (starts with a zero-limit envelope)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signer, err := loadOwnerSigner(ownerKey)
			if err != nil {
				return err
			}
			proj, err := api.CreateProject(cmd.Context(), args[0], name, reason, signer)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(proj)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created Project %s (%s)\n", proj.Slug, proj.ID)
			return err
		},
	}
	create.Flags().StringVar(&name, "name", "", "human-readable Project name (default: slug)")
	create.Flags().StringVar(&reason, "reason", "", "audit reason for creating the Project")
	create.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List Projects in the Collective",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projects, err := api.Projects(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(projects)
			}
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(table, "SLUG\tPROJECT_ID\tSTATE\tSTEP\tENVELOPE")
			for _, p := range projects {
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", p.Slug, p.ID, p.State, p.AutonomyStep, p.EnvelopeID)
			}
			return table.Flush()
		},
	}

	show := &cobra.Command{
		Use:   "show [slug-or-id]",
		Short: "Show a Project (defaults to 'default' when omitted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := project.DefaultProjectSlug
			if len(args) == 1 {
				ref = args[0]
			}
			proj, err := api.Project(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(proj)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Project %s (%s): state=%s step=%s envelope=%s\n",
				proj.Slug, proj.ID, proj.State, proj.AutonomyStep, proj.EnvelopeID)
			return err
		},
	}

	command.AddCommand(
		create, list, show,
		newProjectApplyCommand(api, jsonOutput),
		newProjectFundCommand(api, jsonOutput),
		newProjectTransitionCommand(api, jsonOutput, "pause", "Pause a Project (revokes active leases; requires Owner signature)", project.ActionPause),
		newProjectTransitionCommand(api, jsonOutput, "resume", "Resume a paused Project (requires Owner signature)", project.ActionResume),
		newProjectTransitionCommand(api, jsonOutput, "archive", "Archive a Project permanently (requires Owner signature)", project.ActionArchive),
	)
	return command
}

func newProjectFundCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var ownerKey, reason string
	var limit int64
	cmd := &cobra.Command{
		Use:   "fund <slug>",
		Short: "Set a Project child envelope's budget limit (Owner-signed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("limit") {
				return errors.New("project fund requires --limit")
			}
			signer, err := loadOwnerSigner(ownerKey)
			if err != nil {
				return err
			}
			env, err := api.FundProject(cmd.Context(), args[0], limit, reason, signer)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(env)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Project envelope %s: limit=%d available=%d\n", env.ID, env.HardLimit, env.Available)
			return err
		},
	}
	cmd.Flags().Int64Var(&limit, "limit", 0, "new hard limit in units under envelope_collective_root")
	cmd.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	cmd.Flags().StringVar(&reason, "reason", "", "audit reason for funding the Project envelope")
	return cmd
}

func newProjectTransitionCommand(api control.API, jsonOutput *bool, use, short string, action project.TransitionAction) *cobra.Command {
	var ownerKey, reason string
	cmd := &cobra.Command{
		Use:   use + " <slug>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signer, err := loadOwnerSigner(ownerKey)
			if err != nil {
				return err
			}
			proj, err := api.TransitionProject(cmd.Context(), args[0], action, reason, signer)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(proj)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Project %s (%s): state=%s step=%s envelope=%s\n",
				proj.Slug, proj.ID, proj.State, proj.AutonomyStep, proj.EnvelopeID)
			return err
		},
	}
	cmd.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	cmd.Flags().StringVar(&reason, "reason", "", "audit reason for the Project state change")
	return cmd
}

func newProjectApplyCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var home, ownerKey, reason string
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "apply <slug>",
		Short: "Validate, confirm, and Owner-sign $SUMMA42_HOME/projects/<slug>/project.json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimSpace(args[0])
			resolvedHome, err := localconfig.ResolveHome(home)
			if err != nil {
				return err
			}
			m, digest, err := project.LoadManifestFile(resolvedHome, slug)
			if err != nil {
				return err
			}
			var eng policy.PolicyEngine
			var ownerID domain.ID
			cfg, cfgErr := localconfig.Load(resolvedHome)
			switch {
			case cfgErr == nil:
				store, err := state.Open(cmd.Context(), cfg.DatabasePath)
				if err != nil {
					return fmt.Errorf("open Collective state database: %w", err)
				}
				eng, ownerID, err = project.LoadActivePolicyEngine(cmd.Context(), store)
				_ = store.DB().Close()
				if err != nil {
					return err
				}
			case errors.Is(cfgErr, os.ErrNotExist):
				prof, err := policy.DefaultProfile()
				if err != nil {
					return err
				}
				eng = policy.NewOPAEngine(policy.OPAConfig{
					ModuleName: prof.ModuleName, Module: prof.Module, PolicySetID: prof.PolicySetID, PolicySetHash: prof.PolicyHash,
				})
			default:
				return cfgErr
			}
			if err := project.ValidateManifestWithPolicy(cmd.Context(), m, eng, ownerID, time.Now().UTC()); err != nil {
				return err
			}
			tty := stdinIsTTY()
			if assumeYes && (tty || os.Getenv("SUMMA42_APPROVE_NONINTERACTIVE") != "1") {
				return errors.New("--yes is accepted only when stdin is not a TTY and SUMMA42_APPROVE_NONINTERACTIVE=1")
			}
			if !assumeYes && !tty {
				return errors.New("project apply needs confirmation on a TTY (non-interactive: --yes with SUMMA42_APPROVE_NONINTERACTIVE=1)")
			}
			fmt.Fprint(cmd.ErrOrStderr(), project.FormatManifestSummary(m, digest))
			if !assumeYes {
				fmt.Fprint(cmd.ErrOrStderr(), "Type 'yes' to apply: ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("project manifest apply not confirmed")
				}
			}
			signer, err := loadOwnerSigner(ownerKey)
			if err != nil {
				return err
			}
			status, err := api.Status(cmd.Context())
			if err != nil {
				return err
			}
			proj, err := api.Project(cmd.Context(), slug)
			if err != nil {
				return err
			}
			applyCmd, err := project.NewApplyCommand(signer, status.CollectiveID, proj.ID, proj.Slug, digest, reason, time.Now().UTC())
			if err != nil {
				return err
			}
			updated, err := api.ApplyProjectManifest(cmd.Context(), proj.Slug, applyCmd)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(updated)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Applied manifest %s to Project %s\n", updated.ManifestSHA256, updated.Slug)
			return err
		},
	}
	cmd.Flags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	cmd.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	cmd.Flags().StringVar(&reason, "reason", "", "audit reason for applying the manifest")
	cmd.Flags().BoolVar(&assumeYes, "yes", false, "skip confirmation (only when stdin is not a TTY and SUMMA42_APPROVE_NONINTERACTIVE=1)")
	return cmd
}
