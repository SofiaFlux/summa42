package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

// newPolicyCommand is a local Owner action like init: it writes canonical state
// directly, because the control API has no policy-change endpoint and a policy
// change must not depend on the Box it reconfigures.
func newPolicyCommand() *cobra.Command {
	command := &cobra.Command{Use: "policy", Short: "Manage the Collective policy set"}
	var home, ownerKey string
	upgrade := &cobra.Command{
		Use:   "upgrade",
		Short: "Activate the shipped default policy set with an Owner signature",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedHome, err := localconfig.ResolveHome(home)
			if err != nil {
				return err
			}
			cfg, err := localconfig.Load(resolvedHome)
			if err != nil {
				return fmt.Errorf("load local config: %w", err)
			}
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
			store, err := state.Open(cmd.Context(), cfg.DatabasePath)
			if err != nil {
				return err
			}
			policySetID, err := bootstrap.New(store, clock.System{}).UpgradePolicy(cmd.Context(), signer)
			_ = store.DB().Close()
			if err != nil {
				return err
			}
			// The database commit comes first: a crash before this write leaves a
			// Box that refuses to start, and rerunning the upgrade repairs it.
			cfg.ActivePolicySetID = policySetID
			if err := localconfig.Write(resolvedHome, cfg); err != nil {
				return fmt.Errorf("policy set %s is active but config.json was not updated (rerun the upgrade): %w", policySetID, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Active policy set %s; restart the Box to load it\n", policySetID)
			return err
		},
	}
	upgrade.Flags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	upgrade.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: <home>/keys/owner.key)")
	command.AddCommand(upgrade)
	return command
}
