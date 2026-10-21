package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/policy"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

// TestUpgradeFromV1CollectiveStarts: an existing v1 Collective keeps starting,
// and after the Owner upgrade the Box starts on v2.
func TestUpgradeFromV1CollectiveStarts(t *testing.T) {
	ctx := context.Background()
	config := initializedCollective(t)
	home := mustHome(t)
	store, err := state.Open(ctx, config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	testutil.UsePolicyV1(t, store.DB())
	config.ActivePolicySetID = testutil.PolicyV1SetID
	if err := localconfig.Write(home, config); err != nil {
		t.Fatal(err)
	}
	if _, err := loadStartupMaterial(ctx, config); err != nil {
		t.Fatalf("v1 Collective does not start: %v", err)
	}

	owner, err := identity.NewLocalEd25519(filepath.Join(filepath.Dir(config.DatabasePath), "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := bootstrap.New(store, clock.System{}).UpgradePolicy(ctx, owner)
	_ = store.DB().Close()
	if err != nil {
		t.Fatal(err)
	}
	if upgraded != policy.DefaultPolicySetID {
		t.Fatalf("upgraded to %s", upgraded)
	}
	if _, err := loadStartupMaterial(ctx, config); err == nil {
		t.Fatal("startup accepted a config naming the deactivated v1 policy set")
	}
	config.ActivePolicySetID = upgraded
	material, err := loadStartupMaterial(ctx, config)
	if err != nil {
		t.Fatalf("upgraded Collective does not start: %v", err)
	}
	metadata, err := material.policyEngine.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PolicySetID != policy.DefaultPolicySetID {
		t.Fatalf("startup policy set = %s", metadata.PolicySetID)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := localconfig.ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	return home
}
