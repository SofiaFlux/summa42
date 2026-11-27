package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/project"
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

	command.AddCommand(create, list, show)
	return command
}
