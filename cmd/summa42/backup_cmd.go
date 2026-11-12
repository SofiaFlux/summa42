package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/backup"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/spf13/cobra"
)

// newBackupCommand holds backup create, verify and restore. They work on the
// local database and evidence directly, so a backup can be taken while the
// Box runs and restored where no Box runs yet.
func newBackupCommand(jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "backup", Short: "Back up, check and restore the Collective's state (database and evidence; never keys or tokens)"}
	print := func(cmd *cobra.Command, value any, text func()) error {
		if *jsonOutput {
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(value)
		}
		text()
		return nil
	}
	parseKey := func(text string) (ed25519.PublicKey, error) {
		key, err := hex.DecodeString(strings.TrimSpace(text))
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("--owner-public-key must be the Owner's Ed25519 public key as 64 hex characters (backup create prints it)")
		}
		return ed25519.PublicKey(key), nil
	}

	var createHome, createKey string
	create := &cobra.Command{
		Use: "create <directory>", Short: "Write a signed backup into an empty directory; the Box may keep running", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedHome, err := localconfig.ResolveHome(createHome)
			if err != nil {
				return err
			}
			cfg, err := localconfig.Load(resolvedHome)
			if err != nil {
				return fmt.Errorf("load local config: %w", err)
			}
			keyPath := strings.TrimSpace(createKey)
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
			defer store.DB().Close()
			directory, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			manifest, err := backup.Create(cmd.Context(), store, cfg.EvidencePath, directory, signer, clock.System{})
			if err != nil {
				return err
			}
			publicKey := hex.EncodeToString(signer.PublicKey())
			return print(cmd, map[string]any{"directory": directory, "files": len(manifest.Files), "created_at": manifest.CreatedAt, "owner_public_key": publicKey}, func() {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Backup written to %s (%d files, %s).\n", directory, len(manifest.Files), manifest.CreatedAt.Format(time.RFC3339))
				fmt.Fprintf(out, "Owner public key, needed to restore: %s\n", publicKey)
				fmt.Fprintln(out, "Not in the backup: keys, config.json, tokens and webhook files. Keep your own copy of the keys directory.")
			})
		},
	}
	create.Flags().StringVar(&createHome, "home", "", "Collective home directory (default ~/.summa42)")
	create.Flags().StringVar(&createKey, "owner-key", "", "path to the Owner Ed25519 private key (default: <home>/keys/owner.key)")

	var verifyKey string
	verify := &cobra.Command{
		Use: "verify <directory>", Short: "Check a backup: the Owner's signature, every file, the database and its evidence", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := parseKey(verifyKey)
			if err != nil {
				return err
			}
			manifest, err := backup.Verify(cmd.Context(), args[0], key)
			if err != nil {
				return err
			}
			return print(cmd, manifest, func() {
				fmt.Fprintf(cmd.OutOrStdout(), "Backup is intact: %d files, created %s, Collective %s.\n", len(manifest.Files), manifest.CreatedAt.Format(time.RFC3339), manifest.CollectiveID)
			})
		},
	}
	verify.Flags().StringVar(&verifyKey, "owner-public-key", "", "the Owner's public key as hex (backup create prints it)")

	var restoreKey, restoreInto string
	restore := &cobra.Command{
		Use: "restore <directory>", Short: "Restore a verified backup into an empty home; the restored Collective is left FROZEN", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := parseKey(restoreKey)
			if err != nil {
				return err
			}
			if strings.TrimSpace(restoreInto) == "" {
				return errors.New("backup restore requires --into <empty home directory>")
			}
			target, err := filepath.Abs(restoreInto)
			if err != nil {
				return err
			}
			result, err := backup.Restore(cmd.Context(), args[0], target, key, clock.System{})
			if err != nil {
				return err
			}
			return print(cmd, result, func() {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Restored the backup of %s into %s.\n", result.Manifest.CreatedAt.Format(time.RFC3339), target)
				fmt.Fprintf(out, "  database: %s\n  evidence: %s\n", result.DatabasePath, result.EvidencePath)
				fmt.Fprintln(out, "The Collective is FROZEN. Before `summa42 emergency resume`:")
				fmt.Fprintln(out, "  1. Put back keys/ and config.json from your own copy; set database_path and evidence_path in config.json to the paths above.")
				if len(result.OpenOperations) == 0 {
					fmt.Fprintln(out, "  2. No external operation was open at backup time.")
				} else {
					fmt.Fprintf(out, "  2. Check these %d external operations, whose outcome was open at backup time, against the outside world:\n", len(result.OpenOperations))
					for _, operation := range result.OpenOperations {
						fmt.Fprintf(out, "     %s  %s\n", operation.ID, operation.State)
					}
				}
			})
		},
	}
	restore.Flags().StringVar(&restoreKey, "owner-public-key", "", "the Owner's public key as hex (backup create prints it)")
	restore.Flags().StringVar(&restoreInto, "into", "", "empty directory that becomes the restored Collective home")

	command.AddCommand(create, verify, restore)
	return command
}
