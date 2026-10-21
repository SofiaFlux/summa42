package workflowcase

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func setupEnsure(t *testing.T) (*Service, *purpose.Service, domain.ID, context.Context) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	purposes := purpose.New(store, clk)
	ctx := context.Background()
	missionID, err := purposes.CreateMission(ctx, "Review incoming work")
	if err != nil {
		t.Fatal(err)
	}
	return New(store, clk, purposes), purposes, missionID, ctx
}

func sampleObservation(missionID domain.ID) Observation {
	return Observation{
		MissionID: missionID, Source: "source-a", ObjectID: "item-1", RevisionID: "r1", EvidenceID: "e1",
		FirstWork: workflow.WorkProposal{Kind: "inspect", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		Grant:     workflow.Grant{Capabilities: []string{"read"}}, MaxSteps: 3, RemainingBudget: 5,
	}
}

func TestEnsureCaseDeduplicatesObservationRevision(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	request := sampleObservation(missionID)
	first, err := svc.Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.CurrentWorkID == "" || first.State != Active {
		t.Fatalf("invalid initial case: %+v", first)
	}
	if first.MissionID != missionID || first.NextWork.Kind != "inspect" || first.RemainingBudget != 5 {
		t.Fatalf("case did not retain request: %+v", first)
	}
	replay, err := svc.Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID || replay.CurrentWorkID != first.CurrentWorkID {
		t.Fatalf("replay changed case or work ID: first=%+v replay=%+v", first, replay)
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_cases").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("same revision created %d cases, want 1", count)
	}
	request.RevisionID = "r2"
	next, err := svc.Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID || next.CurrentWorkID == first.CurrentWorkID {
		t.Fatalf("new revision reused case or work ID: first=%+v next=%+v", first, next)
	}
}

func TestEnsureCaseRejectsInactiveMissionAndGrantChange(t *testing.T) {
	svc, purposes, missionID, ctx := setupEnsure(t)
	request := sampleObservation(missionID)
	first, err := svc.Ensure(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Grant.Capabilities = []string{"read", "write"}
	if _, err := svc.Ensure(ctx, request); err == nil {
		t.Fatal("changed grant on same observation key was accepted")
	}
	var grantJSON string
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT grant_json FROM workflow_cases WHERE case_id = ?", first.ID).Scan(&grantJSON); err != nil {
		t.Fatal(err)
	}
	if grantJSON != `{"Capabilities":["read"],"Actions":null}` {
		t.Fatalf("stored grant changed: %s", grantJSON)
	}
	if err := purposes.DeactivateMission(ctx, missionID); err != nil {
		t.Fatal(err)
	}
	request = sampleObservation(missionID)
	request.RevisionID = "r2"
	if _, err := svc.Ensure(ctx, request); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("inactive Mission: got %v, want ErrInvalidPurpose", err)
	}
}

func TestEnsureCaseRejectsInvalidInputAndProposal(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	base := sampleObservation(missionID)
	tests := []struct {
		name string
		edit func(*Observation)
	}{
		{"blank mission", func(o *Observation) { o.MissionID = " " }},
		{"blank source", func(o *Observation) { o.Source = " " }},
		{"blank object", func(o *Observation) { o.ObjectID = " " }},
		{"blank revision", func(o *Observation) { o.RevisionID = " " }},
		{"blank evidence", func(o *Observation) { o.EvidenceID = " " }},
		{"blank first work", func(o *Observation) { o.FirstWork.Kind = " " }},
		{"zero steps", func(o *Observation) { o.MaxSteps = 0 }},
		{"zero budget", func(o *Observation) { o.RemainingBudget = 0 }},
		{"unauthorized capability", func(o *Observation) { o.FirstWork.AuthorityCeiling = []string{"write"} }},
		{"unauthorized action", func(o *Observation) { o.FirstWork.ProposedActions = []string{"comment"} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := base
			tt.edit(&request)
			if _, err := svc.Ensure(ctx, request); err == nil {
				t.Fatal("invalid observation was accepted")
			}
		})
	}
}

func TestListActiveFiltersMissionAndOrdersByID(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	ensure := func(object string) Case {
		t.Helper()
		observation := sampleObservation(missionID)
		observation.ObjectID = object
		c, err := svc.Ensure(ctx, observation)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := ensure("item-1")
	second := ensure("item-2")
	blocked := ensure("item-4")
	if _, err := svc.Assess(ctx, AssessmentRequest{
		CaseID: blocked.ID, WorkID: blocked.CurrentWorkID,
		Assessment:      workflow.Assessment{Verdict: workflow.Unknown, Reason: "blocked", EvidenceIDs: []string{"evidence"}},
		RemainingBudget: blocked.RemainingBudget, ProgressSignature: "blocked",
	}); err != nil {
		t.Fatal(err)
	}
	otherMission := domain.NewID("mission")
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO missions(mission_id, statement, active, created_at, deactivated_at) VALUES (?, 'other', 0, ?, ?)`,
		otherMission, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx,
		`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id, observation_evidence_id,
		 initial_request_json, grant_json, state, current_work_id, next_work_json, completed_steps, max_steps,
		 remaining_budget, progress_signature, created_at, updated_at)
		 VALUES (?, ?, 'ado', 'item-3', 'r3', 'e3', '{}', '{"Capabilities":["read"],"Actions":null}',
		 'ACTIVE', ?, '{"Kind":"publish-decision","RequiredCapabilities":null,"AuthorityCeiling":null,"ProposedActions":null}',
		 0, 3, 5, '', ?, ?)`,
		domain.NewID("case"), otherMission, domain.NewID("work"), now, now,
	); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ListActive(ctx, missionID)

	if err != nil {
		t.Fatal(err)
	}
	want := []string{string(first.ID), string(second.ID)}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ListActive = %+v, want %v", got, want)
	}
	for index := range want {
		if string(got[index].ID) != want[index] {
			t.Fatalf("ListActive IDs = %q, want ordered %v", got[index].ID, want)
		}
	}
}

func TestListAssessmentsRoundTripsStoredJSON(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	initial, err := svc.Ensure(ctx, sampleObservation(missionID))
	if err != nil {
		t.Fatal(err)
	}
	request := AssessmentRequest{
		CaseID: initial.ID, WorkID: initial.CurrentWorkID, RemainingBudget: 4, ProgressSignature: "reviewed",
		Assessment: workflow.Assessment{
			Verdict: workflow.Continue, EvidenceIDs: []string{"review-evidence"},
			Next: &workflow.WorkProposal{Kind: "publish", RequiredCapabilities: []string{"read"}, AuthorityCeiling: []string{"read"}},
		},
	}
	if _, err := svc.Assess(ctx, request); err != nil {
		t.Fatal(err)
	}
	var storedID, storedRequest, storedResult, storedCreated string
	if err := svc.store.DB().QueryRowContext(ctx,
		`SELECT assessment_id, request_json, result_json, created_at FROM workflow_assessments WHERE case_id = ?`, initial.ID,
	).Scan(&storedID, &storedRequest, &storedResult, &storedCreated); err != nil {
		t.Fatal(err)
	}
	records, err := svc.ListAssessments(ctx, initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one", records)
	}
	record := records[0]
	if record.ID != storedID || record.WorkID != string(initial.CurrentWorkID) || record.RequestJSON != storedRequest || record.ResultJSON != storedResult || domain.FormatTime(record.CreatedAt) != storedCreated {
		t.Fatalf("record = %+v, want stored %s %s %s %s", record, storedID, storedRequest, storedResult, storedCreated)
	}
	var decodedRequest AssessmentRequest
	var decodedResult AssessmentResult
	if err := json.Unmarshal([]byte(record.RequestJSON), &decodedRequest); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(record.ResultJSON), &decodedResult); err != nil {
		t.Fatal(err)
	}
	if decodedRequest.WorkID != initial.CurrentWorkID || decodedResult.Case.ID != initial.ID {
		t.Fatalf("round trip = request %+v result %+v", decodedRequest, decodedResult)
	}
}

func TestEnsureRefusesObjectOwnedByOtherMission(t *testing.T) {
	svc, purposes, missionA, ctx := setupEnsure(t)
	if _, err := svc.Ensure(ctx, sampleObservation(missionA)); err != nil {
		t.Fatal(err)
	}
	if err := purposes.DeactivateMission(ctx, missionA); err != nil { // only one Mission is active at a time
		t.Fatal(err)
	}
	missionB, err := purposes.CreateMission(ctx, "Second mission")
	if err != nil {
		t.Fatal(err)
	}
	request := sampleObservation(missionB)
	request.RevisionID = "r2"
	if _, err := svc.Ensure(ctx, request); !errors.Is(err, ErrObjectOwnedByOtherMission) {
		t.Fatalf("got %v, want ErrObjectOwnedByOtherMission", err)
	}
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM workflow_cases").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("refused Ensure left %d cases, want 1", count)
	}
}

func TestEnsureAllowsObjectAfterOtherMissionCaseClosed(t *testing.T) {
	svc, purposes, missionA, ctx := setupEnsure(t)
	first, err := svc.Ensure(ctx, sampleObservation(missionA))
	if err != nil {
		t.Fatal(err)
	}
	if err := purposes.DeactivateMission(ctx, missionA); err != nil { // only one Mission is active at a time
		t.Fatal(err)
	}
	missionB, err := purposes.CreateMission(ctx, "Second mission")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx, "UPDATE workflow_cases SET state = ? WHERE case_id = ?", Closed, first.ID); err != nil {
		t.Fatal(err)
	}
	request := sampleObservation(missionB)
	request.RevisionID = "r2"
	if _, err := svc.Ensure(ctx, request); err != nil {
		t.Fatalf("closed owner still blocked: %v", err)
	}
}
