package resources

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func setupBootstrappedResources(t *testing.T) (*Service, context.Context, identity.Signer, domain.ID) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))
	keyDir := t.TempDir()
	owner, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "root.key"), "root")
	if err != nil {
		t.Fatal(err)
	}
	res, err := bootstrap.New(store, clk).Init(ctx, bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: rootKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(store, clk), ctx, owner, res.CollectiveID
}

func TestProjectCannotSpendBeyondItsEnvelope(t *testing.T) {
	svc, ctx, owner, collectiveID := setupBootstrappedResources(t)
	now := svc.clock.Now()

	// Fund collective root to 10 units.
	gRoot, err := NewOwnerGrant(owner, collectiveID, GrantSetLimit, "envelope_collective_root", 10, "fund collective root", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyOwnerGrant(ctx, gRoot); err != nil {
		t.Fatalf("fund root: %v", err)
	}

	// Create two sibling child envelopes under envelope_collective_root.
	for _, id := range []domain.ID{"envelope_project_alpha", "envelope_project_beta"} {
		if _, err := svc.store.DB().ExecContext(ctx,
			`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES (?, 'envelope_collective_root', 0, ?)`,
			id, domain.FormatTime(now),
		); err != nil {
			t.Fatal(err)
		}
	}

	// Allocate 6 to alpha and 4 to beta (10 == root limit).
	for _, spec := range []struct {
		id    domain.ID
		limit int64
	}{
		{"envelope_project_alpha", 6},
		{"envelope_project_beta", 4},
	} {
		g, err := NewOwnerGrant(owner, collectiveID, GrantSetLimit, spec.id, spec.limit, "fund child", now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.ApplyOwnerGrant(ctx, g); err != nil {
			t.Fatalf("fund %s: %v", spec.id, err)
		}
	}

	// Strict partitioning: raising beta from 4 to 5 would push children sum (11) > root (10).
	gOver, err := NewOwnerGrant(owner, collectiveID, GrantSetLimit, "envelope_project_beta", 5, "overallocate beta", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyOwnerGrant(ctx, gOver); err == nil {
		t.Fatal("expected over-allocating child envelope beyond parent hard_limit to fail")
	}

	// Lowering root below children sum (10) must also fail.
	gShrink, err := NewOwnerGrant(owner, collectiveID, GrantSetLimit, "envelope_collective_root", 9, "shrink root", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyOwnerGrant(ctx, gShrink); err == nil {
		t.Fatal("expected shrinking parent below allocated children sum to fail")
	}

	// Alpha reserves 6 (its full limit); trying 1 more must fail even though root has 4 left for beta.
	if _, err := svc.Reserve(ctx, "envelope_project_alpha", 6, hardCapped()); err != nil {
		t.Fatalf("alpha reserve 6: %v", err)
	}
	if _, err := svc.Reserve(ctx, "envelope_project_alpha", 1, hardCapped()); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("alpha reserve beyond child limit: got %v, want ErrBudgetExceeded", err)
	}

	// Beta still keeps its isolated 4 units.
	if _, err := svc.Reserve(ctx, "envelope_project_beta", 4, hardCapped()); err != nil {
		t.Fatalf("beta isolated reserve 4 failed: %v", err)
	}

	// If alpha over-settled on root's ancestors or direct spend would exceed root, AllowRenewalInTx also guards ancestor cap.
	tx, err := svc.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	allowed, err := svc.AllowRenewalInTx(ctx, tx, "envelope_project_alpha")
	if err != nil {
		t.Fatalf("AllowRenewalInTx: %v", err)
	}
	if !allowed {
		t.Fatal("expected renewal allowed while exposure is HELD within hard_limit")
	}
}

func TestEnvelopeHierarchyRejectsCycleAndDepthLimit(t *testing.T) {
	svc, ctx, _, _ := setupBootstrappedResources(t)
	now := domain.FormatTime(svc.clock.Now())

	// Build a cycle A -> B -> A.
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES ('env_cycle_a', NULL, 10, ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES ('env_cycle_b', 'env_cycle_a', 10, ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx,
		`UPDATE resource_envelopes SET parent_envelope_id = 'env_cycle_b' WHERE envelope_id = 'env_cycle_a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reserve(ctx, "env_cycle_a", 1, hardCapped()); err == nil {
		t.Fatal("expected cycle in envelope hierarchy to fail closed")
	}

	// Build a chain exceeding max depth (> 8 ancestors).
	prev := ""
	for i := 0; i < 10; i++ {
		id := domain.ID("env_depth_" + string(rune('a'+i)))
		var err error
		if prev == "" {
			_, err = svc.store.DB().ExecContext(ctx,
				`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES (?, NULL, 10, ?)`, id, now)
		} else {
			_, err = svc.store.DB().ExecContext(ctx,
				`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES (?, ?, 10, ?)`, id, prev, now)
		}
		if err != nil {
			t.Fatal(err)
		}
		prev = string(id)
	}
	if _, err := svc.Reserve(ctx, domain.ID(prev), 1, hardCapped()); err == nil {
		t.Fatal("expected envelope hierarchy exceeding max depth to fail closed")
	}
}
