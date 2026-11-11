package emergency

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type testSigner struct {
	id  domain.ID
	key ed25519.PrivateKey
}

func (s testSigner) PrincipalID() domain.ID { return s.id }
func (s testSigner) Sign(message []byte) ([]byte, error) {
	return ed25519.Sign(s.key, message), nil
}

func newTestSigner(t *testing.T, id domain.ID) (testSigner, ed25519.PublicKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{id: id, key: private}, public
}

// seedSignedOwner makes owner the Collective's Owner with a real public key.
func seedSignedOwner(t *testing.T, store *state.Store, owner domain.ID, public ed25519.PublicKey) {
	t.Helper()
	now := domain.FormatTime(time.Now().UTC())
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO principals(principal_id, principal_kind, public_key, custody_profile, created_at) VALUES (?, 'HUMAN', ?, 'LOCAL', ?)`, []any{owner, []byte(public), now}},
		{`INSERT INTO constitutions(version, content, content_hash, signature, signer_principal_id, active, created_at) VALUES (1, X'00', 'hash-1', X'00', ?, 1, ?)`, []any{owner, now}},
		{`INSERT INTO collective_metadata(singleton, collective_id, owner_principal_id, cube_principal_id, constitutional_root_principal_id, active_constitution_version, created_at)
		  VALUES (1, 'collective-1', ?, ?, ?, 1, ?)`, []any{owner, owner, owner, now}},
	} {
		if _, err := store.DB().Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func newOwnerCommandFixture(t *testing.T) (*Service, *fixedClock, testSigner, *state.Store) {
	t.Helper()
	store, _ := setupTestStore(t)
	t.Cleanup(func() { _ = store.DB().Close() })
	clk := &fixedClock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	signer, public := newTestSigner(t, "owner_signed")
	seedSignedOwner(t, store, signer.id, public)
	svc := NewService(store, clk, signer.id)
	if err := svc.EnsureDefaults(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	return svc, clk, signer, store
}

func TestOwnerCommandFreezeResumeAndHeartbeatAreSignedAndRecorded(t *testing.T) {
	ctx := context.Background()
	svc, clk, signer, store := newOwnerCommandFixture(t)

	freeze, err := NewOwnerCommand(signer, "collective-1", ActionFreeze, "stop everything", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, freeze); err != nil {
		t.Fatalf("apply freeze: %v", err)
	}
	bg, _, err := svc.GetStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bg.State != BreakGlassFrozen || bg.FreezeReason != "stop everything" {
		t.Fatalf("break glass = %s (%q), want FROZEN with the Owner's reason", bg.State, bg.FreezeReason)
	}
	var signature []byte
	if err := store.DB().QueryRow(`SELECT signature FROM emergency_audit_events WHERE event_id = ?`, freeze.EventID).Scan(&signature); err != nil {
		t.Fatalf("freeze audit event under the command's event ID: %v", err)
	}
	if string(signature) != string(freeze.Signature) {
		t.Fatal("audit event does not carry the Owner's signature")
	}

	resume, err := NewOwnerCommand(signer, "collective-1", ActionResume, "verified", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, resume); err != nil {
		t.Fatalf("apply resume: %v", err)
	}

	clk.Advance(8 * 24 * time.Hour)
	if err := svc.CheckExecutionAllowed(ctx, "external_operation"); !errors.Is(err, ErrSafeModeRestricted) {
		t.Fatalf("past the horizon = %v, want ErrSafeModeRestricted", err)
	}
	heartbeat, err := NewOwnerCommand(signer, "collective-1", ActionHeartbeat, "", int((30 * 24 * time.Hour).Seconds()), clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, heartbeat); err != nil {
		t.Fatalf("apply heartbeat: %v", err)
	}
	if err := svc.CheckExecutionAllowed(ctx, "external_operation"); err != nil {
		t.Fatalf("after the signed heartbeat: %v", err)
	}
	if got, want := storedTimeout(t, store.DB()), int((30 * 24 * time.Hour).Seconds()); got != want {
		t.Fatalf("horizon = %ds, want the %ds the Owner signed", got, want)
	}
}

func TestOwnerCommandRejectsForgedStaleReplayedAndUnknownCommands(t *testing.T) {
	ctx := context.Background()
	svc, clk, signer, _ := newOwnerCommandFixture(t)
	stranger, _ := newTestSigner(t, signer.id) // right principal ID, wrong key

	forged, err := NewOwnerCommand(stranger, "collective-1", ActionHeartbeat, "", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, forged); !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("command signed with another key = %v, want ErrUnauthorizedEmergencyCaller", err)
	}

	other, _ := newTestSigner(t, "not_the_owner")
	notOwner, err := NewOwnerCommand(other, "collective-1", ActionFreeze, "x", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, notOwner); !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("command from a non-Owner principal = %v, want ErrUnauthorizedEmergencyCaller", err)
	}

	tampered, err := NewOwnerCommand(signer, "collective-1", ActionHeartbeat, "", 3600, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	tampered.TimeoutSeconds = 999999999
	if err := svc.ApplyOwnerCommand(ctx, tampered); !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("command changed after signing = %v, want ErrUnauthorizedEmergencyCaller", err)
	}

	elsewhere, err := NewOwnerCommand(signer, "another-collective", ActionHeartbeat, "", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, elsewhere); !errors.Is(err, ErrUnauthorizedEmergencyCaller) {
		t.Fatalf("command signed for another Collective = %v, want ErrUnauthorizedEmergencyCaller", err)
	}

	stale, err := NewOwnerCommand(signer, "collective-1", ActionHeartbeat, "", 0, clk.Now().Add(-10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, stale); !errors.Is(err, ErrOwnerCommandExpired) {
		t.Fatalf("ten-minute-old command = %v, want ErrOwnerCommandExpired", err)
	}

	fresh, err := NewOwnerCommand(signer, "collective-1", ActionHeartbeat, "", 0, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if err := svc.ApplyOwnerCommand(ctx, fresh); !errors.Is(err, ErrOwnerCommandReplayed) {
		t.Fatalf("replayed command = %v, want ErrOwnerCommandReplayed", err)
	}

	if _, err := NewOwnerCommand(signer, "collective-1", OwnerAction("SELF_DESTRUCT"), "", 0, clk.Now()); err == nil {
		t.Fatal("an unknown action must not be signable")
	}
	if _, err := NewOwnerCommand(signer, "collective-1", ActionFreeze, "  ", 0, clk.Now()); err == nil {
		t.Fatal("a freeze without a reason must not be signable")
	}
	if _, err := NewOwnerCommand(signer, "collective-1", ActionHeartbeat, "", 30, clk.Now()); err == nil {
		t.Fatal("a horizon under one minute must not be signable")
	}
}
