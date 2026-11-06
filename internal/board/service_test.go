package board_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/board"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

type boardFixture struct {
	svc      *board.Service
	purposes *purpose.Service
	evidence *evidence.Store
	clock    *testutil.Clock
	mission  domain.ID
	ctx      context.Context
}

func newBoardFixture(t *testing.T) *boardFixture {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC))
	purposesSvc := purpose.New(store, clk)
	missionID, err := purposesSvc.CreateMission(ctx, "Board work queue mission")
	if err != nil {
		t.Fatal(err)
	}

	evStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}

	boardSvc := board.NewService(store, clk, purposesSvc, evStore)
	return &boardFixture{
		svc:      boardSvc,
		purposes: purposesSvc,
		evidence: evStore,
		clock:    clk,
		mission:  missionID,
		ctx:      ctx,
	}
}

func (f *boardFixture) makeEvidence(t *testing.T, payload string) domain.ID {
	t.Helper()
	obj, err := f.evidence.Put(f.ctx, strings.NewReader(payload), evidence.Metadata{
		MediaType: "text/plain",
		Kind:      "test.evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	return obj.ID
}

// Acceptance criterion: A GitHub issue becomes exactly one card; a second read
// of the same revision creates no card; a new revision updates the card.
func TestCardIntakeIdempotencyAndRevisionUpdate(t *testing.T) {
	f := newBoardFixture(t)

	item := board.Item{
		MissionID:            f.mission,
		Source:               "github",
		SourceID:             "org/repo#42",
		RevisionID:           "sha-v1",
		Title:                "Bug in calculation",
		Description:          "Fix calculation overflow",
		RequiredCapabilities: []string{"read"},
		Grant:                workflow.Grant{Capabilities: []string{"read"}},
	}

	// 1. Initial intake creates card
	card1, created, err := f.svc.SyncItem(f.ctx, item)
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}
	if !created {
		t.Fatalf("expected card to be newly created")
	}
	if card1.Column != board.ColumnIntake {
		t.Fatalf("expected initial column INTAKE, got %s", card1.Column)
	}

	// 2. Re-reading identical revision is a no-op (no new card, created = false)
	card2, created2, err := f.svc.SyncItem(f.ctx, item)
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if created2 {
		t.Fatalf("expected no new card created on re-read of same revision")
	}
	if card2.ID != card1.ID {
		t.Fatalf("expected same card ID %s, got %s", card1.ID, card2.ID)
	}

	// Move card to TRIAGE
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   card1.ID,
		ToColumn: board.ColumnTriage,
		Actor:    "planner",
		Reason:   "Triaging issue",
	})
	if err != nil {
		t.Fatalf("move to triage: %v", err)
	}

	// 3. New revision updates title/description and keeps column
	updatedItem := item
	updatedItem.RevisionID = "sha-v2"
	updatedItem.Title = "Bug in calculation (updated)"

	card3, created3, err := f.svc.SyncItem(f.ctx, updatedItem)
	if err != nil {
		t.Fatalf("third sync with new revision: %v", err)
	}
	if created3 {
		t.Fatalf("new revision should update existing card, not create duplicate")
	}
	if card3.ID != card1.ID {
		t.Fatalf("expected same card ID %s", card1.ID)
	}
	if card3.Title != "Bug in calculation (updated)" {
		t.Fatalf("expected updated title, got %s", card3.Title)
	}
	if card3.RevisionID != "sha-v2" {
		t.Fatalf("expected revision sha-v2, got %s", card3.RevisionID)
	}
	if card3.Column != board.ColumnTriage {
		t.Fatalf("expected card to remain in TRIAGE column across revision update, got %s", card3.Column)
	}
}

// Acceptance criterion: Every card move records the actor, the previous and next
// column, the reason, and the evidence IDs.
func TestCardMoveAuditLog(t *testing.T) {
	f := newBoardFixture(t)
	evID := f.makeEvidence(t, "triage review result")

	card, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:  f.mission,
		Source:     "github",
		SourceID:   "org/repo#100",
		RevisionID: "rev-1",
		Title:      "Feature request",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Move: INTAKE -> TRIAGE
	f.clock.Advance(time.Second)
	movedCard, move1, err := f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:      card.ID,
		ToColumn:    board.ColumnTriage,
		Actor:       "architect-1",
		Reason:      "Beginning decomposition",
		EvidenceIDs: []domain.ID{evID},
	})
	if err != nil {
		t.Fatalf("move card: %v", err)
	}
	if movedCard.Column != board.ColumnTriage {
		t.Fatalf("expected column TRIAGE, got %s", movedCard.Column)
	}
	if move1.FromColumn != board.ColumnIntake || move1.ToColumn != board.ColumnTriage {
		t.Fatalf("expected move from INTAKE to TRIAGE, got %s -> %s", move1.FromColumn, move1.ToColumn)
	}
	if move1.Actor != "architect-1" || move1.Reason != "Beginning decomposition" {
		t.Fatalf("move metadata mismatch: %+v", move1)
	}
	if len(move1.EvidenceIDs) != 1 || move1.EvidenceIDs[0] != evID {
		t.Fatalf("evidence IDs mismatch: %v", move1.EvidenceIDs)
	}

	// Verify move history
	moves, err := f.svc.ListMoves(f.ctx, card.ID)
	if err != nil {
		t.Fatalf("list moves: %v", err)
	}
	if len(moves) != 2 { // Initial creation move + move to triage
		t.Fatalf("expected 2 moves recorded, got %d", len(moves))
	}
	lastMove := moves[len(moves)-1]
	if lastMove.ToColumn != board.ColumnTriage || lastMove.Actor != "architect-1" {
		t.Fatalf("unexpected last move: %+v", lastMove)
	}
}

// Acceptance criterion: A card cannot enter In progress without a mission reference
// and the grant its work needs; a test covers each missing piece.
func TestCardCannotEnterInProgressWithoutMissionAndGrant(t *testing.T) {
	f := newBoardFixture(t)

	// Case 1: Card missing required grant capability
	cardWithInsufficientGrant, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:            f.mission,
		Source:               "github",
		SourceID:             "org/repo#201",
		RevisionID:           "rev-1",
		Title:                "Needs write capability",
		RequiredCapabilities: []string{"workspace.repo.write"},
		Grant:                workflow.Grant{Capabilities: []string{"read"}}, // Missing write!
	})
	if err != nil {
		t.Fatal(err)
	}

	// Move to TRIAGE then READY first
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   cardWithInsufficientGrant.ID,
		ToColumn: board.ColumnTriage,
		Actor:    "planner",
		Reason:   "Triaging",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   cardWithInsufficientGrant.ID,
		ToColumn: board.ColumnReady,
		Actor:    "planner",
		Reason:   "Ready for execution",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Attempt to move to IN_PROGRESS without required capability must fail
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   cardWithInsufficientGrant.ID,
		ToColumn: board.ColumnInProgress,
		Actor:    "worker-1",
		Reason:   "Claiming execution slot",
	})
	if err == nil {
		t.Fatalf("expected error moving to IN_PROGRESS with insufficient grant, got nil")
	}

	// Case 2: Card with inactive mission
	if err := f.purposes.DeactivateMission(f.ctx, f.mission); err != nil {
		t.Fatal(err)
	}

	_, _, err = f.svc.SyncItem(f.ctx, board.Item{
		MissionID:            f.mission,
		Source:               "github",
		SourceID:             "org/repo#202",
		RevisionID:           "rev-1",
		Title:                "Inactive mission item",
		RequiredCapabilities: []string{"read"},
		Grant:                workflow.Grant{Capabilities: []string{"read"}},
	})
	if err == nil {
		t.Fatalf("expected error syncing card with inactive mission, got nil")
	}

	// Recreate active mission for subsequent test cases
	f.mission, err = f.purposes.CreateMission(f.ctx, "Reactivated board mission")
	if err != nil {
		t.Fatal(err)
	}

	// Case 3: Valid grant allowing move to IN_PROGRESS
	validCard, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:            f.mission,
		Source:               "github",
		SourceID:             "org/repo#203",
		RevisionID:           "rev-1",
		Title:                "Valid item",
		RequiredCapabilities: []string{"workspace.repo.write"},
		Grant:                workflow.Grant{Capabilities: []string{"workspace.repo.write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   validCard.ID,
		ToColumn: board.ColumnTriage,
		Actor:    "planner",
		Reason:   "Triaging",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   validCard.ID,
		ToColumn: board.ColumnReady,
		Actor:    "planner",
		Reason:   "Plan accepted",
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgressCard, _, err := f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   validCard.ID,
		ToColumn: board.ColumnInProgress,
		Actor:    "worker-1",
		Reason:   "Starting execution",
	})
	if err != nil {
		t.Fatalf("expected successful move to IN_PROGRESS, got: %v", err)
	}
	if inProgressCard.Column != board.ColumnInProgress {
		t.Fatalf("expected IN_PROGRESS, got %s", inProgressCard.Column)
	}
}

// Acceptance criterion: Moving a card never produces an external effect; a test
// shows that a move with an approved card still needs a separate commitment.
func TestMoveCardNeverProducesExternalSideEffect(t *testing.T) {
	f := newBoardFixture(t)

	card, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:  f.mission,
		Source:     "github",
		SourceID:   "org/repo#301",
		RevisionID: "rev-1",
		Title:      "Deploy feature",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Move through valid sequence to DONE
	for _, col := range []board.Column{board.ColumnTriage, board.ColumnReady, board.ColumnInProgress, board.ColumnReview, board.ColumnDone} {
		_, _, err := f.svc.MoveCard(f.ctx, board.MoveRequest{
			CardID:   card.ID,
			ToColumn: col,
			Actor:    "operator",
			Reason:   "advancing workflow",
		})
		if err != nil {
			t.Fatalf("move to %s: %v", col, err)
		}
	}

	// Constitutional boundary (Art. 22): moving card never produces an external effect.
	// Verify that no external operations were created.
	var opCount int
	if err := f.svc.DB().QueryRowContext(f.ctx, "SELECT count(*) FROM external_operations").Scan(&opCount); err != nil {
		t.Fatal(err)
	}
	if opCount != 0 {
		t.Fatalf("expected 0 external operations, got %d", opCount)
	}

	finalCard, err := f.svc.GetCard(f.ctx, card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalCard.Column != board.ColumnDone {
		t.Fatalf("expected card in DONE, got %s", finalCard.Column)
	}
}

// Acceptance criterion: The Owner summary for a mission lists Held and Blocked
// cards with their reasons and matches the board state (a test compares them).
func TestOwnerSummaryMatchesBoardState(t *testing.T) {
	f := newBoardFixture(t)

	// Create 1 Held card, 1 InProgress card, 1 Intake card
	c1, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:  f.mission,
		Source:     "github",
		SourceID:   "org/repo#401",
		RevisionID: "rev-1",
		Title:      "Item to hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{
		CardID:   c1.ID,
		ToColumn: board.ColumnHeld,
		Actor:    "owner",
		Reason:   "Waiting for architectural directive",
	})
	if err != nil {
		t.Fatal(err)
	}

	c2, _, err := f.svc.SyncItem(f.ctx, board.Item{
		MissionID:            f.mission,
		Source:               "github",
		SourceID:             "org/repo#402",
		RevisionID:           "rev-1",
		Title:                "Item in flight",
		RequiredCapabilities: []string{"read"},
		Grant:                workflow.Grant{Capabilities: []string{"read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{CardID: c2.ID, ToColumn: board.ColumnTriage, Actor: "p", Reason: "triage"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{CardID: c2.ID, ToColumn: board.ColumnReady, Actor: "p", Reason: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.svc.MoveCard(f.ctx, board.MoveRequest{CardID: c2.ID, ToColumn: board.ColumnInProgress, Actor: "w", Reason: "executing"})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := f.svc.OwnerSummary(f.ctx, f.mission)
	if err != nil {
		t.Fatalf("owner summary: %v", err)
	}

	if summary.TotalCards != 2 {
		t.Fatalf("expected total 2 cards, got %d", summary.TotalCards)
	}
	if len(summary.HeldCards) != 1 {
		t.Fatalf("expected 1 held card, got %d", len(summary.HeldCards))
	}
	held := summary.HeldCards[0]
	if held.CardID != c1.ID || held.Reason != "Waiting for architectural directive" {
		t.Fatalf("held card mismatch: %+v", held)
	}
	if summary.ColumnCounts[board.ColumnHeld] != 1 || summary.ColumnCounts[board.ColumnInProgress] != 1 {
		t.Fatalf("column counts mismatch: %+v", summary.ColumnCounts)
	}
}

// Acceptance criterion: Adding a second source (ADO or Jira) needs only a new
// connector, not a change to the board model; the test uses a fake source.
func TestMultiSourceConnectorsWithoutModelChanges(t *testing.T) {
	f := newBoardFixture(t)

	// Source 1: GitHub connector
	ghConnector := board.NewGitHubConnector()
	ghItem, err := ghConnector.ToItem(f.mission, map[string]any{
		"repo":        "SofiaFlux/summa42",
		"issue_id":    124,
		"revision_id": "rev-gh-1",
		"title":       "Board work queue",
	})
	if err != nil {
		t.Fatalf("github connector: %v", err)
	}
	ghCard, _, err := f.svc.SyncItem(f.ctx, ghItem)
	if err != nil {
		t.Fatalf("sync github card: %v", err)
	}

	// Source 2: ADO connector (fake external item)
	adoConnector := board.NewFakeADOConnector("https://dev.azure.com/org/proj")
	adoItem, err := adoConnector.ToItem(f.mission, map[string]any{
		"work_item_id": 999,
		"rev":          3,
		"title":        "ADO user story",
	})
	if err != nil {
		t.Fatalf("ado connector: %v", err)
	}
	adoCard, _, err := f.svc.SyncItem(f.ctx, adoItem)
	if err != nil {
		t.Fatalf("sync ado card: %v", err)
	}

	// Both cards exist on the same board
	cards, err := f.svc.ListCards(f.ctx, f.mission, board.ColumnIntake)
	if err != nil {
		t.Fatalf("list cards: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("expected 2 intake cards from different sources, got %d", len(cards))
	}
	if ghCard.Source != "github" || adoCard.Source != "azure_devops" {
		t.Fatalf("sources mismatch: gh=%s, ado=%s", ghCard.Source, adoCard.Source)
	}
}
