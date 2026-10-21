package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

func NewInitCommand() *cobra.Command {
	var home string
	command := &cobra.Command{
		Use:   "init",
		Short: "Initialize a local Summa42",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			resolvedHome, err := localconfig.ResolveHome(home)
			if err != nil {
				return err
			}
			return initCollective(command, resolvedHome)
		},
	}
	command.Flags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	return command
}

func initCollective(command *cobra.Command, home string) error {
	home, err := localconfig.ResolveHome(home)
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, "config.json")
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("%w: %s", bootstrap.ErrAlreadyInitialized, home)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	stateDir := filepath.Join(home, "state")
	evidenceDir := filepath.Join(home, "evidence")
	keysDir := filepath.Join(home, "keys")
	ceremonyDir := filepath.Join(home, "ceremony")
	for _, dir := range []string{home, stateDir, evidenceDir, keysDir, ceremonyDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	dbPath := filepath.Join(stateDir, "summa42.db")
	store, err := state.Open(command.Context(), dbPath)
	if err != nil {
		return err
	}
	defer store.DB().Close()

	owner, err := identity.CreateLocalEd25519(filepath.Join(keysDir, "owner.key"), "owner")
	if err != nil {
		return err
	}
	cube, err := identity.CreateLocalEd25519(filepath.Join(keysDir, "cube.key"), "cube")
	if err != nil {
		return err
	}
	root, err := identity.NewConstitutionalRootForCeremony(filepath.Join(ceremonyDir, "constitution-root.key"))
	if err != nil {
		return err
	}

	service := bootstrap.New(store, clock.System{})
	result, err := service.Init(command.Context(), bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: root,
	})
	if err != nil {
		return err
	}
	controlToken, err := localconfig.NewControlToken()
	if err != nil {
		return err
	}
	config := localconfig.Config{
		Version:                       localconfig.CurrentVersion,
		CollectiveID:                  result.CollectiveID,
		OwnerPrincipalID:              result.OwnerPrincipalID,
		CubePrincipalID:               result.CubePrincipalID,
		ConstitutionalRootPrincipalID: result.ConstitutionalRootPrincipalID,
		ConstitutionHash:              result.ConstitutionHash,
		ActivePolicySetID:             result.PolicySetID,
		DatabasePath:                  dbPath,
		EvidencePath:                  evidenceDir,
		ControlToken:                  controlToken,
	}
	if err := localconfig.Write(home, config); err != nil {
		return err
	}

	out := command.OutOrStdout()
	fmt.Fprintf(out, "collective_id=%s\n", result.CollectiveID)
	fmt.Fprintf(out, "owner_principal_id=%s\n", result.OwnerPrincipalID)
	fmt.Fprintf(out, "cube_principal_id=%s\n", result.CubePrincipalID)
	fmt.Fprintf(out, "constitutional_root_principal_id=%s\n", result.ConstitutionalRootPrincipalID)
	fmt.Fprintf(out, "constitution_hash=%s\n", result.ConstitutionHash)
	fmt.Fprintf(out, "policy_set_id=%s\n", result.PolicySetID)
	return nil
}
