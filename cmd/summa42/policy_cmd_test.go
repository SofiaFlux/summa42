package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/policy"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// v1Collective initializes a Collective with `summa42 init` and rewinds it to
// the policy_mvc_default_v1 state an existing install is in.
func v1Collective(t *testing.T) (string, localconfig.Config) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "collective")
	t.Setenv("SUMMA42_HOME", home)
	runCLI(t, "init", "--home", home)
	cfg, err := localconfig.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	testutil.UsePolicyV1(t, store.DB())
	_ = store.DB().Close()
	cfg.ActivePolicySetID = testutil.PolicyV1SetID
	if err := localconfig.Write(home, cfg); err != nil {
		t.Fatal(err)
	}
	return home, cfg
}

func runCLI(t *testing.T, args ...string) error {
	t.Helper()
	cmd := NewRootCommand()
	cmd.SetArgs(args)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err := cmd.ExecuteContext(t.Context())
	if err != nil && args[0] == "init" {
		t.Fatalf("init: %v\n%s", err, output.String())
	}
	return err
}

func activePolicySet(t *testing.T, path string) (domain.ID, string, []byte) {
	t.Helper()
	store, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	var id domain.ID
	var signer *string
	var signature []byte
	if err := store.DB().QueryRow(`SELECT policy_set_id, signer_principal_id, signature FROM policy_sets WHERE active = 1`).Scan(&id, &signer, &signature); err != nil {
		t.Fatal(err)
	}
	if signer == nil {
		return id, "", signature
	}
	return id, *signer, signature
}

func TestPolicyUpgradeActivatesOwnerSignedV2(t *testing.T) {
	home, cfg := v1Collective(t)
	if err := runCLI(t, "policy", "upgrade", "--home", home); err != nil {
		t.Fatal(err)
	}
	id, signer, signature := activePolicySet(t, cfg.DatabasePath)
	if id != policy.DefaultPolicySetID || domain.ID(signer) != cfg.OwnerPrincipalID {
		t.Fatalf("active policy set %s signed by %q, want %s signed by %s", id, signer, policy.DefaultPolicySetID, cfg.OwnerPrincipalID)
	}
	store, err := state.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	var publicKey []byte
	var active int
	err = store.DB().QueryRow(`SELECT public_key FROM principals WHERE principal_id = ?`, cfg.OwnerPrincipalID).Scan(&publicKey)
	if err == nil {
		err = store.DB().QueryRow(`SELECT count(*) FROM policy_sets WHERE policy_set_id = ? AND active = 0`, testutil.PolicyV1SetID).Scan(&active)
	}
	_ = store.DB().Close()
	if err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatal("v1 policy set is not kept inactive")
	}
	profile, err := policy.DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(publicKey, bootstrap.PolicySetSigningDigest(profile), signature) {
		t.Fatal("v2 policy set signature does not verify with the Owner key")
	}
	updated, err := localconfig.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ActivePolicySetID != policy.DefaultPolicySetID {
		t.Fatalf("config active policy set = %s", updated.ActivePolicySetID)
	}
	info, err := os.Stat(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestUpgradeRefusesWithInFlightOperations(t *testing.T) {
	for _, inFlight := range []domain.OperationState{domain.OperationPrepared, domain.OperationDispatched} {
		t.Run(string(inFlight), func(t *testing.T) {
			home, cfg := v1Collective(t)
			store, err := state.Open(context.Background(), cfg.DatabasePath)
			if err != nil {
				t.Fatal(err)
			}
			// The operation's task/attempt/slot rows are irrelevant to the refusal.
			_, err = store.DB().Exec(`PRAGMA foreign_keys = OFF`)
			if err == nil {
				_, err = store.DB().Exec(`
					INSERT INTO external_operations(
						operation_id, task_id, attempt_id, effect_slot_id, operation_sequence, state,
						intent_fingerprint, intent_revision, provider, adapter_version, reservation_id, risk,
						prepare_policy_decision_id, prepare_policy_set_id, prepare_policy_set_hash,
						prepare_policy_capabilities_hash, created_at)
					SELECT 'operation_inflight', 'task', 'attempt', 'slot', 1, ?, 'fp', 1, 'p', '1', 'r', 'LOW',
					       'decision', policy_set_id, policy_hash, capabilities_hash, '2026-10-21T00:00:00Z'
					FROM policy_sets WHERE active = 1`, inFlight)
			}
			_ = store.DB().Close()
			if err != nil {
				t.Fatal(err)
			}

			if err := runCLI(t, "policy", "upgrade", "--home", home); !errors.Is(err, bootstrap.ErrOperationsInFlight) {
				t.Fatalf("upgrade error = %v, want ErrOperationsInFlight", err)
			}
			if id, _, _ := activePolicySet(t, cfg.DatabasePath); id != testutil.PolicyV1SetID {
				t.Fatalf("active policy set = %s after refused upgrade", id)
			}
			unchanged, err := localconfig.Load(home)
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.ActivePolicySetID != testutil.PolicyV1SetID {
				t.Fatalf("config active policy set = %s after refused upgrade", unchanged.ActivePolicySetID)
			}
		})
	}
}
