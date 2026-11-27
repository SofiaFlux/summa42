package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/project"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

func newBootstrappedControlServer(t *testing.T) (*Server, *Client, identity.Signer, domain.ID) {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })

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
	initRes, err := bootstrap.New(store, clock.System{}).Init(ctx, bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}

	server, status, _, _, _, _, _ := newTestServer(t)
	status.value = StatusDTO{CollectiveID: initRes.CollectiveID, State: "DORMANT"}
	server.config.OwnerPrincipalID = owner.PrincipalID()
	server.config.OwnerPublicKey = owner.PublicKey()
	server.deps.Projects = project.New(store, clock.System{})
	server.deps.Missions = purpose.New(store, clock.System{})

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	client := NewClient(httpServer.URL, "control-secret", httpServer.Client())
	return server, client, owner, initRes.CollectiveID
}

func TestControlProjectsRequireOwnerSignature(t *testing.T) {
	_, client, owner, collectiveID := newBootstrappedControlServer(t)
	ctx := context.Background()

	// Unsigned request (valid fields, empty signature) with bearer token alone must fail 403.
	body, _ := json.Marshal(project.CreateCommand{
		EventID: domain.NewID("projevent"), Slug: "daas", Name: "DaaS",
		Reason: "unsigned", PrincipalID: owner.PrincipalID(), IssuedAt: time.Now().UTC(),
	})
	resp := doRequest(t, http.MethodPost, client.baseURL+"/projects", "control-secret", bytes.NewReader(body))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned POST /projects status = %d, want 403", resp.StatusCode)
	}

	// Non-owner key must also fail.
	stranger, err := identity.CreateLocalEd25519(filepath.Join(t.TempDir(), "stranger.key"), "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateProject(ctx, "daas", "DaaS", "stranger", stranger); err == nil {
		t.Fatal("stranger-signed CreateProject must fail")
	}
	_ = collectiveID

	// Owner-signed CreateProject succeeds and persists under List/Get.
	created, err := client.CreateProject(ctx, "daas", "DaaS Platform", "add daas", owner)
	if err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}
	if created.Slug != "daas" || created.Name != "DaaS Platform" {
		t.Fatalf("created = %+v", created)
	}

	got, err := client.Project(ctx, "daas")
	if err != nil || got.ID != created.ID {
		t.Fatalf("Get(daas) = (%+v, %v)", got, err)
	}
	list, err := client.Projects(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("Projects() len=%d err=%v, want 2 (default + daas)", len(list), err)
	}
}

func TestControlMissionSwitchRequiresOwnerSignature(t *testing.T) {
	_, client, owner, _ := newBootstrappedControlServer(t)
	ctx := context.Background()

	m1, err := client.CreateMission(ctx, "Manage core infrastructure", owner)
	if err != nil {
		t.Fatalf("CreateMission m1 failed: %v", err)
	}
	m2, err := client.CreateStandbyMission(ctx, "Launch DaaS product", owner)
	if err != nil {
		t.Fatalf("CreateStandbyMission m2 failed: %v", err)
	}

	// Unsigned switch with bearer token alone must be refused with 403.
	challengeBody, _ := json.Marshal(SwitchMissionRequest{MissionID: m2.ID})
	chResp := doRequest(t, http.MethodPost, client.baseURL+"/missions/switch/challenge", "control-secret", bytes.NewReader(challengeBody))
	if chResp.StatusCode != http.StatusOK {
		t.Fatalf("switch challenge status = %d", chResp.StatusCode)
	}
	var ch MissionChallengeDTO
	decodeJSON(t, chResp, &ch)

	unsignedBody, _ := json.Marshal(SwitchMissionRequest{MissionID: m2.ID, Challenge: ch.Challenge})
	swResp := doRequest(t, http.MethodPost, client.baseURL+"/missions/switch", "control-secret", bytes.NewReader(unsignedBody))
	swResp.Body.Close()
	if swResp.StatusCode != http.StatusForbidden {
		t.Fatalf("unsigned switch status = %d, want 403", swResp.StatusCode)
	}

	// Owner-signed SwitchMission switches active Mission atomically.
	switched, err := client.SwitchMission(ctx, m2.ID, owner)
	if err != nil {
		t.Fatalf("SwitchMission failed: %v", err)
	}
	if switched.ID != m2.ID {
		t.Fatalf("switched ID = %q, want %q", switched.ID, m2.ID)
	}
	active, err := client.ActiveMission(ctx)
	if err != nil || active.ID != m2.ID {
		t.Fatalf("ActiveMission = (%+v, %v), want %q", active, err, m2.ID)
	}
	all, err := client.Missions(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("Missions = (%d, %v), want 2", len(all), err)
	}
	_ = m1
}
