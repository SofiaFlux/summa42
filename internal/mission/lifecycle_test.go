package mission_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/mission"
	_ "modernc.org/sqlite"
)

func sampleDraftFields() mission.CreateMissionFields {
	return mission.CreateMissionFields{
		Purpose: "Establish an autonomous sovereign collective platform",
		DesiredOutcomes: []string{
			"Deterministic execution boundary with zero ambient leakage",
			"Safe self-evolution under anti-Goodhart governance",
		},
		Obligations: []string{
			"Preserve audit log immutability",
			"Maintain human sovereignty over mission mutations",
		},
		Constraints: []string{
			"Zero egress outside verified TEB proxies",
			"Maximum single-run compute budget 500k tokens",
		},
		NonGoals: []string{
			"Building general artificial intelligence without human governance",
			"Unfenced financial settlement",
		},
		TradeOffRules: []mission.TradeOffRule{
			{
				Priority:    "Safety & Containment",
				Subordinate: "Task Completion Speed",
				Rationale:   "Containment breaches cause irreversible external consequence",
			},
		},
		HumanTrustBoundaries: []string{
			"Mission statement mutations require root human signature",
			"Financial authority above $100 requires manual gate",
		},
		Assumptions: []string{
			"Underlying host kernel enforces OCI cgroups/namespaces",
		},
		Unknowns: []string{
			"Long-term drift in LLM reasoning across model provider revisions",
		},
		RevisionTriggers: []string{
			"Repeated containment escape attempts (>3 in 24h)",
			"Statutory regulatory policy change",
		},
		SourceInputs: []string{
			"corp_charter_2026.md",
			"incident_report_77.json",
		},
		EvidenceRefs: []string{
			"ev_hash_99a8b1",
		},
	}
}

func TestMissionLifecycle_CanonicalHappyPath(t *testing.T) {
	ctx := context.Background()
	store := mission.NewInMemoryMissionStore()
	service := mission.NewService(store)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}

	workerPrincipal := mission.Principal{
		ID:   "worker-agent-researcher",
		Kind: mission.PrincipalKindWorker,
	}

	humanGovernor := mission.Principal{
		ID:        "sovereign-human-root",
		Kind:      mission.PrincipalKindHuman,
		PublicKey: pub,
	}

	// 1. Worker creates DRAFT mission
	mID := domain.NewID("mission")
	m, err := service.CreateDraft(ctx, workerPrincipal, mID, sampleDraftFields())
	if err != nil {
		t.Fatalf("create draft failed: %v", err)
	}
	if m.State != mission.MissionStateDraft {
		t.Errorf("expected state DRAFT, got %s", m.State)
	}
	if m.Version != 1 {
		t.Errorf("expected version 1, got %d", m.Version)
	}
	if len(m.DesiredOutcomes) != 2 || len(m.TradeOffRules) != 1 {
		t.Errorf("incomplete fields on draft: %+v", m)
	}

	// 2. Worker submits DRAFT for PROPOSED
	m, err = service.Propose(ctx, workerPrincipal, mID, 1)
	if err != nil {
		t.Fatalf("propose failed: %v", err)
	}
	if m.State != mission.MissionStateProposed {
		t.Errorf("expected state PROPOSED, got %s", m.State)
	}
	if m.Provenance.ProposedBy != workerPrincipal.ID {
		t.Errorf("expected proposed_by recorded, got %s", m.Provenance.ProposedBy)
	}

	// 3. Worker attempts to self-approve -> MUST BE REJECTED!
	_, err = service.Approve(ctx, workerPrincipal, mID, 1)
	if !errors.Is(err, mission.ErrUnauthorizedApproval) {
		t.Fatalf("expected ErrUnauthorizedApproval when worker attempts to approve mission, got: %v", err)
	}

	// 4. Governor signs and approves mission
	payload := []byte(fmt.Sprintf("%s:%d:%s", mID, 1, m.Purpose))
	humanGovernor.Signature = ed25519.Sign(priv, payload)

	m, err = service.Approve(ctx, humanGovernor, mID, 1)
	if err != nil {
		t.Fatalf("governor approve failed: %v", err)
	}
	if m.State != mission.MissionStateApproved {
		t.Errorf("expected state APPROVED, got %s", m.State)
	}
	if m.Provenance.ApprovingPrincipal != "sovereign-human-root" {
		t.Errorf("expected approving principal recorded in provenance, got %s", m.Provenance.ApprovingPrincipal)
	}
	if m.Provenance.ApprovedAt == nil {
		t.Errorf("expected ApprovedAt timestamp recorded")
	}

	// 5. Activate mission
	m, err = service.Activate(ctx, humanGovernor, mID, 1)
	if err != nil {
		t.Fatalf("activate failed: %v", err)
	}
	if m.State != mission.MissionStateActive {
		t.Errorf("expected state ACTIVE, got %s", m.State)
	}

	// Active mission can be retrieved
	activeM, err := service.GetActive(ctx, mID)
	if err != nil {
		t.Fatalf("get active failed: %v", err)
	}
	if activeM.Version != 1 || activeM.State != mission.MissionStateActive {
		t.Errorf("unexpected active mission: %+v", activeM)
	}
}

func TestMissionLifecycle_VersionSupersessionAndImmutability(t *testing.T) {
	ctx := context.Background()
	store := mission.NewInMemoryMissionStore()
	service := mission.NewService(store)

	humanGovernor := mission.Principal{
		ID:   "sovereign-human-root",
		Kind: mission.PrincipalKindHuman,
	}

	mID := domain.NewID("mission_v")

	// Create and activate Version 1
	_, _ = service.CreateDraft(ctx, humanGovernor, mID, sampleDraftFields())
	_, _ = service.Propose(ctx, humanGovernor, mID, 1)
	_, _ = service.Approve(ctx, humanGovernor, mID, 1)
	m1Active, err := service.Activate(ctx, humanGovernor, mID, 1)
	if err != nil {
		t.Fatalf("activate v1 failed: %v", err)
	}
	if m1Active.State != mission.MissionStateActive {
		t.Fatalf("v1 not active")
	}

	// Invariant: Active / Approved mission is immutable
	err = service.UpdateDraft(ctx, humanGovernor, mID, 1, sampleDraftFields())
	if !errors.Is(err, mission.ErrMissionImmutable) {
		t.Fatalf("expected ErrMissionImmutable when attempting to update active mission, got: %v", err)
	}

	// Create Version 2 referencing v1
	v2Fields := sampleDraftFields()
	v2Fields.Purpose = "Expanded purpose for version 2"
	v2Fields.ChangeSummary = "Expanded scope after collective review"

	m2, err := service.CreateNextVersion(ctx, humanGovernor, mID, v2Fields)
	if err != nil {
		t.Fatalf("create next version failed: %v", err)
	}
	if m2.Version != 2 {
		t.Fatalf("expected version 2, got %d", m2.Version)
	}
	if m2.State != mission.MissionStateDraft {
		t.Errorf("expected version 2 to start in DRAFT, got %s", m2.State)
	}
	if m2.Provenance.BaseVersion != 1 {
		t.Errorf("expected base version 1 recorded, got %d", m2.Provenance.BaseVersion)
	}

	// Progress Version 2 to ACTIVE
	_, _ = service.Propose(ctx, humanGovernor, mID, 2)
	_, _ = service.Approve(ctx, humanGovernor, mID, 2)
	m2Active, err := service.Activate(ctx, humanGovernor, mID, 2)
	if err != nil {
		t.Fatalf("activate v2 failed: %v", err)
	}
	if m2Active.State != mission.MissionStateActive {
		t.Errorf("expected v2 to be ACTIVE")
	}

	// Invariant: Activating v2 automatically marks v1 as SUPERSEDED!
	v1Reloaded, err := service.GetVersion(ctx, mID, 1)
	if err != nil {
		t.Fatalf("get v1 failed: %v", err)
	}
	if v1Reloaded.State != mission.MissionStateSuperseded {
		t.Fatalf("expected v1 to be SUPERSEDED after v2 activation, got %s", v1Reloaded.State)
	}

	// Active mission is now Version 2
	active, err := service.GetActive(ctx, mID)
	if err != nil {
		t.Fatalf("get active failed: %v", err)
	}
	if active.Version != 2 {
		t.Errorf("expected active version 2, got %d", active.Version)
	}
}

func TestMissionLifecycle_UnauthorizedMutationsAndGuards(t *testing.T) {
	ctx := context.Background()
	store := mission.NewInMemoryMissionStore()
	service := mission.NewService(store)

	workerPrincipal := mission.Principal{
		ID:   "autonomous-bot",
		Kind: mission.PrincipalKindWorker,
	}

	mID := domain.NewID("unauth")
	service.CreateDraft(ctx, workerPrincipal, mID, sampleDraftFields())
	service.Propose(ctx, workerPrincipal, mID, 1)

	// Invariant: UpdateDraft rejected when mission is in PROPOSED state
	err := service.UpdateDraft(ctx, workerPrincipal, mID, 1, sampleDraftFields())
	if !errors.Is(err, mission.ErrMissionImmutable) {
		t.Errorf("expected ErrMissionImmutable on updating PROPOSED mission, got: %v", err)
	}

	// Worker cannot activate
	_, err = service.Activate(ctx, workerPrincipal, mID, 1)
	if !errors.Is(err, mission.ErrUnauthorizedGovernance) {
		t.Errorf("expected ErrUnauthorizedGovernance when worker attempts to activate, got %v", err)
	}

	// Invalid state transition: cannot activate from PROPOSED without APPROVAL
	human := mission.Principal{ID: "root", Kind: mission.PrincipalKindHuman}
	_, err = service.Activate(ctx, human, mID, 1)
	if !errors.Is(err, mission.ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition activating unapproved mission, got %v", err)
	}

	// CreateNextVersion with empty purpose rejected
	emptyFields := sampleDraftFields()
	emptyFields.Purpose = "   "
	_, err = service.CreateNextVersion(ctx, human, mID, emptyFields)
	if err == nil {
		t.Errorf("expected error creating next version with empty purpose")
	}

	// Incomplete schema proposal rejected
	incompleteID := domain.NewID("incomp")
	service.CreateDraft(ctx, human, incompleteID, mission.CreateMissionFields{Purpose: "Some purpose"})
	_, err = service.Propose(ctx, human, incompleteID, 1)
	if !errors.Is(err, mission.ErrIncompleteMissionSchema) {
		t.Errorf("expected ErrIncompleteMissionSchema on proposing incomplete mission, got %v", err)
	}
}

func TestMission_ConcurrentActivation(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	store, err := mission.NewSQLiteMissionStore(db)
	if err != nil {
		t.Fatalf("new sqlite mission store failed: %v", err)
	}
	service := mission.NewService(store)
	human := mission.Principal{ID: "root", Kind: mission.PrincipalKindHuman}
	mID := domain.NewID("conc-mission")

	// Create and approve version 1 and version 2
	service.CreateDraft(ctx, human, mID, sampleDraftFields())
	service.Propose(ctx, human, mID, 1)
	service.Approve(ctx, human, mID, 1)

	service.CreateNextVersion(ctx, human, mID, sampleDraftFields())
	service.Propose(ctx, human, mID, 2)
	service.Approve(ctx, human, mID, 2)

	// Concurrently attempt to activate version 1 and version 2
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = service.Activate(ctx, human, mID, 1)
	}()
	go func() {
		defer wg.Done()
		_, _ = service.Activate(ctx, human, mID, 2)
	}()
	wg.Wait()

	// Verify invariant: Exactly 1 version is ACTIVE!
	versions, err := service.ListVersions(ctx, mID)
	if err != nil {
		t.Fatalf("list versions error: %v", err)
	}

	activeCount := 0
	for _, v := range versions {
		if v.State == mission.MissionStateActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("INVARIANT VIOLATION: expected exactly 1 ACTIVE version, found %d", activeCount)
	}
}

func TestMission_SQLiteStoreRoundtripAndCoexistence(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Simulate existing legacy purpose missions table
	legacySchema := `
	CREATE TABLE IF NOT EXISTS missions (
		mission_id TEXT PRIMARY KEY,
		statement TEXT NOT NULL,
		active INTEGER NOT NULL DEFAULT 1
	);
	INSERT INTO missions (mission_id, statement, active) VALUES ('legacy-1', 'Legacy statement', 1);
	`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy missions table failed: %v", err)
	}

	// New SQLiteMissionStore must initialize and co-exist cleanly without schema collision!
	store, err := mission.NewSQLiteMissionStore(db)
	if err != nil {
		t.Fatalf("new sqlite mission store failed: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	m := mission.Mission{
		ID:      domain.NewID("m-sql"),
		Version: 1,
		State:   mission.MissionStateDraft,
		Purpose: "SQLite persistence test",
		DesiredOutcomes: []string{
			"Full fidelity roundtrip",
		},
		TradeOffRules: []mission.TradeOffRule{
			{Priority: "Correctness", Subordinate: "Speed", Rationale: "Data integrity"},
		},
		Provenance: mission.MissionProvenance{
			AuthorPrincipal: "tester",
			SourceInputs:    []string{"input.txt"},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := store.Save(ctx, m); err != nil {
		t.Fatalf("store save failed: %v", err)
	}

	// Duplicate save of identical version fails
	if err := store.Save(ctx, m); !errors.Is(err, mission.ErrDuplicateMissionVersion) {
		t.Errorf("expected ErrDuplicateMissionVersion, got %v", err)
	}

	// Fetch back
	fetched, err := store.Get(ctx, m.ID, 1)
	if err != nil {
		t.Fatalf("store get failed: %v", err)
	}
	if fetched.Purpose != m.Purpose || len(fetched.TradeOffRules) != 1 {
		t.Errorf("roundtrip data mismatch: %+v", fetched)
	}

	// Update state
	m.State = mission.MissionStateApproved
	m.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	if err := store.Update(ctx, m); err != nil {
		t.Fatalf("store update failed: %v", err)
	}

	// Activate target version atomically
	activeM, err := store.ActivateVersion(ctx, m.ID, 1)
	if err != nil {
		t.Fatalf("activate version failed: %v", err)
	}
	if activeM.State != mission.MissionStateActive {
		t.Errorf("expected state ACTIVE, got %s", activeM.State)
	}

	active, err := store.GetActive(ctx, m.ID)
	if err != nil {
		t.Fatalf("get active failed: %v", err)
	}
	if active.State != mission.MissionStateActive {
		t.Errorf("expected state ACTIVE, got %s", active.State)
	}
}
