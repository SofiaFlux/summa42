package project

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func setupCollective(t *testing.T) (*Service, *purpose.Service, domain.ID, identity.Signer, clock.Clock) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := fixedClock{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	keyDir := t.TempDir()
	owner, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	root, err := identity.CreateLocalEd25519(filepath.Join(keyDir, "root.key"), "root")
	if err != nil {
		t.Fatal(err)
	}
	initRes, err := bootstrap.New(store, clk).Init(context.Background(), bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(store, clk), purpose.New(store, clk), initRes.CollectiveID, owner, clk
}

func TestProjectCreateRequiresOwnerSignature(t *testing.T) {
	svc, _, collectiveID, owner, clk := setupCollective(t)
	ctx := context.Background()

	stranger, err := identity.CreateLocalEd25519(filepath.Join(t.TempDir(), "stranger.key"), "stranger")
	if err != nil {
		t.Fatal(err)
	}

	// Non-owner key must be rejected.
	badCmd, err := NewCreateCommand(stranger, collectiveID, "daas", "DaaS Product", "Initial DaaS project", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSigned(ctx, badCmd); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stranger signature error = %v, want ErrUnauthorized", err)
	}

	// Tampered slug after signing must be rejected.
	validCmd, err := NewCreateCommand(owner, collectiveID, "daas", "DaaS Product", "Initial DaaS project", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validCmd
	tampered.Slug = "other"
	if _, err := svc.CreateSigned(ctx, tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered slug error = %v, want ErrUnauthorized", err)
	}

	// Stale issued_at outside replay window must be rejected.
	staleCmd, err := NewCreateCommand(owner, collectiveID, "daas", "DaaS Product", "Stale create", clk.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSigned(ctx, staleCmd); !errors.Is(err, ErrExpired) {
		t.Fatalf("stale command error = %v, want ErrExpired", err)
	}

	// Valid Owner-signed command succeeds, seeds zero-limit child envelope under root, and records event.
	created, err := svc.CreateSigned(ctx, validCmd)
	if err != nil {
		t.Fatalf("CreateSigned failed: %v", err)
	}
	if created.Slug != "daas" || created.State != StateActive || created.EnvelopeID == "" {
		t.Fatalf("created project = %+v", created)
	}

	var hardLimit int64
	var parent, envProject string
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT hard_limit, COALESCE(parent_envelope_id, ''), COALESCE(project_id, '') FROM resource_envelopes WHERE envelope_id = ?`,
		created.EnvelopeID,
	).Scan(&hardLimit, &parent, &envProject); err != nil {
		t.Fatal(err)
	}
	if hardLimit != 0 || parent != string(CollectiveRootEnvelopeID) || envProject != string(created.ID) {
		t.Fatalf("project envelope limit=%d parent=%q project_id=%q, want 0, %q, %q", hardLimit, parent, envProject, CollectiveRootEnvelopeID, created.ID)
	}

	// Replay of the exact same signed command must be refused.
	if _, err := svc.CreateSigned(ctx, validCmd); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replayed command error = %v, want ErrReplayed", err)
	}
}

func TestTwoProjectsShareCollectiveActiveMissionWithIsolatedCasesAndBoard(t *testing.T) {
	svc, purposes, collectiveID, owner, clk := setupCollective(t)
	ctx := context.Background()

	daasCmd, err := NewCreateCommand(owner, collectiveID, "daas", "DaaS", "Launch DaaS", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	daas, err := svc.CreateSigned(ctx, daasCmd)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := svc.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("List = (%d, %v), want 2 projects (default + daas)", len(listed), err)
	}

	missionID, err := purposes.CreateMission(ctx, "Build technical infrastructure and DaaS product")
	if err != nil {
		t.Fatalf("CreateMission failed: %v", err)
	}

	const ts = "2026-11-27T12:00:00Z"
	for _, tc := range []struct {
		caseID, cardID, projectID, obj string
	}{
		{"case-default", "card-default", string(DefaultProjectID), "SofiaFlux/summa42#1"},
		{"case-daas", "card-daas", string(daas.ID), "daas/api#1"},
	} {
		if _, err := svc.store.DB().ExecContext(ctx, `
			INSERT INTO workflow_cases(case_id, project_id, mission_id, source, object_id, revision_id, observation_evidence_id,
				initial_request_json, grant_json, state, current_work_id, next_work_json, completed_steps, max_steps,
				remaining_budget, progress_signature, created_at, updated_at)
			VALUES (?, ?, ?, 'github', ?, 'r1', 'ev1', '{}', '{"Capabilities":["github.issue.read"],"Actions":null}', 'ACTIVE', 'w1', '{}', 0, 3, 10, 'sig', ?, ?)`,
			tc.caseID, tc.projectID, missionID, tc.obj, ts, ts); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.store.DB().ExecContext(ctx, `
			INSERT INTO board_cards(card_id, project_id, mission_id, source, source_id, revision_id, column_name, title, created_at, updated_at)
			VALUES (?, ?, ?, 'github', ?, 'r1', 'INTAKE', 'Card', ?, ?)`,
			tc.cardID, tc.projectID, missionID, tc.obj, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	var defaultCases, daasCases int
	_ = svc.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM workflow_cases WHERE project_id = ?`, DefaultProjectID).Scan(&defaultCases)
	_ = svc.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM workflow_cases WHERE project_id = ?`, daas.ID).Scan(&daasCases)
	if defaultCases != 1 || daasCases != 1 {
		t.Fatalf("case counts default=%d daas=%d, want 1 and 1", defaultCases, daasCases)
	}
}
