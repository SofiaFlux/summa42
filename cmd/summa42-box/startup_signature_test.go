package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/policy"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func TestStartupRejectsTamperedConstitution(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	if _, err := loadStartupMaterial(ctx, config); err != nil {
		t.Fatalf("untampered startup: %v", err)
	}
	store, err := state.Open(ctx, config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()

	// Content swapped, hash left alone.
	if _, err := store.DB().ExecContext(ctx, `UPDATE constitutions SET content = ? WHERE active = 1`, []byte("tampered constitution")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStartupMaterial(ctx, config); err == nil {
		t.Fatal("startup accepted a constitution whose content no longer matches its hash")
	}
}

func TestStartupRejectsForgedConstitutionSignature(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	store, err := state.Open(ctx, config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	if _, err := store.DB().ExecContext(ctx, `UPDATE constitutions SET signature = zeroblob(64) WHERE active = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStartupMaterial(ctx, config); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v, want constitution signature failure", err)
	}
}

func TestStartupVerifiesPolicySetSignatureWhenPresent(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	store, err := state.Open(ctx, config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	owner, err := identity.NewLocalEd25519(filepath.Join(filepath.Dir(config.DatabasePath), "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := policy.DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	good, err := owner.Sign(bootstrap.PolicySetSigningDigest(profile))
	if err != nil {
		t.Fatal(err)
	}
	set := func(signature []byte) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, `UPDATE policy_sets SET signer_principal_id = ?, signature = ? WHERE active = 1`, owner.PrincipalID(), signature); err != nil {
			t.Fatal(err)
		}
	}
	set(good)
	if _, err := loadStartupMaterial(ctx, config); err != nil {
		t.Fatalf("valid Owner-signed policy set rejected: %v", err)
	}
	set(make([]byte, 64))
	if _, err := loadStartupMaterial(ctx, config); err == nil {
		t.Fatal("startup accepted a policy set with an invalid signature")
	}
}
