package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/resources"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func TestEnvelopeCreateListAndSetLimit(t *testing.T) {
	home, cfg := emergencyCollective(t)

	output, err := runCLIOutput(t, "envelope", "create", "summa42-work", "--limit", "500", "--reason", "first budget", "--home", home)
	if err != nil {
		t.Fatalf("envelope create: %v\n%s", err, output)
	}
	if !strings.Contains(output, "summa42-work") || !strings.Contains(output, "500") {
		t.Fatalf("create output:\n%s", output)
	}

	// The envelope is a real one: the ledger reserves against it.
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	ledger := resources.New(store, clock.System{})
	if available, err := ledger.Available(ctx, "summa42-work"); err != nil || available != 500 {
		t.Fatalf("Available = %d, %v; want 500", available, err)
	}
	var action, reason string
	var signature []byte
	if err := store.DB().QueryRow(`SELECT action, reason, signature FROM resource_envelope_grants WHERE envelope_id = 'summa42-work'`).Scan(&action, &reason, &signature); err != nil {
		t.Fatal(err)
	}
	if action != "CREATE" || reason != "first budget" || len(signature) != 64 {
		t.Fatalf("recorded grant: action %q reason %q signature of %d bytes", action, reason, len(signature))
	}

	if output, err := runCLIOutput(t, "envelope", "create", "summa42-work", "--limit", "9", "--reason", "again", "--home", home); err == nil {
		t.Fatalf("creating an existing envelope succeeded:\n%s", output)
	}
	if output, err := runCLIOutput(t, "envelope", "set-limit", "summa42-work", "--limit", "800", "--reason", "more work", "--home", home); err != nil {
		t.Fatalf("set-limit: %v\n%s", err, output)
	}
	if output, err := runCLIOutput(t, "envelope", "set-limit", "no-such-envelope", "--limit", "1", "--reason", "x", "--home", home); err == nil {
		t.Fatalf("set-limit on a missing envelope succeeded:\n%s", output)
	}

	output, err = runCLIOutput(t, "--json", "envelope", "list", "--home", home)
	if err != nil {
		t.Fatalf("envelope list: %v\n%s", err, output)
	}
	var envelopes []resources.Envelope
	if err := json.Unmarshal([]byte(output), &envelopes); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, output)
	}
	found := false
	for _, envelope := range envelopes {
		if envelope.ID == "summa42-work" {
			found = envelope.HardLimit == 800 && envelope.Available == 800
		}
	}
	if !found {
		t.Fatalf("list does not show summa42-work with limit 800: %+v", envelopes)
	}
}

func TestEnvelopeCommandsRefuseBadInput(t *testing.T) {
	home, _ := emergencyCollective(t)
	for name, args := range map[string][]string{
		"no limit":       {"envelope", "create", "budget-a", "--reason", "r"},
		"no reason":      {"envelope", "create", "budget-a", "--limit", "5"},
		"negative limit": {"envelope", "create", "budget-a", "--limit", "-1", "--reason", "r"},
		"bad id":         {"envelope", "create", "Budget A", "--limit", "5", "--reason", "r"},
		"missing key":    {"envelope", "create", "budget-a", "--limit", "5", "--reason", "r", "--owner-key", filepath.Join(home, "no.key")},
	} {
		if output, err := runCLIOutput(t, append(args, "--home", home)...); err == nil {
			t.Errorf("%s: accepted:\n%s", name, output)
		}
	}
}

// Only the Collective's Owner can give it a budget: a grant signed with
// another key, an old grant and a replayed grant are refused, and none of
// them leaves an envelope behind.
func TestEnvelopeGrantNeedsTheOwnersFreshSignature(t *testing.T) {
	home, cfg := emergencyCollective(t)
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	ledger := resources.New(store, clock.System{})
	owner, err := identity.NewLocalEd25519(filepath.Join(home, "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := identity.CreateLocalEd25519(filepath.Join(t.TempDir(), "stranger.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	forged, err := resources.NewOwnerGrant(stranger, cfg.CollectiveID, resources.GrantCreate, "forged", 100, "mine now", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ApplyOwnerGrant(ctx, forged); !errors.Is(err, resources.ErrGrantUnauthorized) {
		t.Fatalf("grant signed by another key: %v, want ErrGrantUnauthorized", err)
	}
	// The stranger's key under the Owner's principal ID does not verify either.
	forged.PrincipalID = owner.PrincipalID()
	if _, err := ledger.ApplyOwnerGrant(ctx, forged); !errors.Is(err, resources.ErrGrantUnauthorized) {
		t.Fatalf("grant with a borrowed principal: %v, want ErrGrantUnauthorized", err)
	}

	good, err := resources.NewOwnerGrant(owner, cfg.CollectiveID, resources.GrantCreate, "tampered", 100, "ok", now)
	if err != nil {
		t.Fatal(err)
	}
	raised := good
	raised.HardLimit = 1_000_000
	if _, err := ledger.ApplyOwnerGrant(ctx, raised); !errors.Is(err, resources.ErrGrantUnauthorized) {
		t.Fatalf("grant with a changed limit: %v, want ErrGrantUnauthorized", err)
	}

	old, err := resources.NewOwnerGrant(owner, cfg.CollectiveID, resources.GrantCreate, "old", 100, "ok", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ApplyOwnerGrant(ctx, old); !errors.Is(err, resources.ErrGrantExpired) {
		t.Fatalf("hour-old grant: %v, want ErrGrantExpired", err)
	}

	for _, id := range []string{"forged", "tampered", "old"} {
		var count int
		if err := store.DB().QueryRow(`SELECT COUNT(*) FROM resource_envelopes WHERE envelope_id = ?`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("refused grant left envelope %s behind (%d, %v)", id, count, err)
		}
	}

	if _, err := ledger.ApplyOwnerGrant(ctx, good); err != nil {
		t.Fatalf("the Owner's grant: %v", err)
	}
	if _, err := ledger.ApplyOwnerGrant(ctx, good); !errors.Is(err, resources.ErrGrantReplayed) {
		t.Fatalf("replayed grant: %v, want ErrGrantReplayed", err)
	}
}

func TestEnvelopeLimitCannotDropBelowWhatIsCommitted(t *testing.T) {
	home, cfg := emergencyCollective(t)
	if output, err := runCLIOutput(t, "envelope", "create", "budget-a", "--limit", "100", "--reason", "r", "--home", home); err != nil {
		t.Fatalf("create: %v\n%s", err, output)
	}
	ctx := context.Background()
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := resources.New(store, clock.System{})
	if err := store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := ledger.ReserveInTx(ctx, tx, "budget-a", 60, resources.Enforceability{CostControl: resources.CostTechnicallyCapped, Source: "test"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	store.DB().Close()

	if output, err := runCLIOutput(t, "envelope", "set-limit", "budget-a", "--limit", "59", "--reason", "cut", "--home", home); err == nil {
		t.Fatalf("limit set below the 60 units already held:\n%s", output)
	}
	if output, err := runCLIOutput(t, "envelope", "set-limit", "budget-a", "--limit", "60", "--reason", "cut", "--home", home); err != nil {
		t.Fatalf("limit set to exactly what is held: %v\n%s", err, output)
	}
}
