package workflowcase

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflow"
)

func readyForVerificationFixture(t *testing.T) (*Service, context.Context, Case) {
	t.Helper()
	svc, ctx, initial := assessmentFixture(t)
	result, err := svc.Assess(ctx, AssessmentRequest{
		CaseID: initial.ID, WorkID: initial.CurrentWorkID, RemainingBudget: 4, ProgressSignature: "ready",
		Assessment: workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"ready-evidence"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, ctx, result.Case
}

func insertWorkflowEvidence(t *testing.T, svc *Service, ctx context.Context, ids ...domain.ID) {
	t.Helper()
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		if _, err := svc.store.DB().ExecContext(ctx, `
			INSERT INTO evidence_objects(evidence_id,content_hash,media_type,kind,size_bytes,created_at)
			VALUES (?,?,'application/json','TEST',1,?)`, id, "hash-"+string(id), now); err != nil {
			t.Fatal(err)
		}
	}
}

func closeVerificationRequest(c Case) VerificationRequest {
	return VerificationRequest{
		CaseID: c.ID, VerifierID: "verifier-1", VerifierType: "HUMAN",
		SnapshotHash: "snapshot-hash-1", SnapshotJSON: `{"publication":"verified"}`,
		EvidenceIDs: []domain.ID{"verify-b", "verify-a", "verify-b"},
	}
}

func workflowVerificationCount(t *testing.T, svc *Service, ctx context.Context, caseID domain.ID) int {
	t.Helper()
	var count int
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM workflow_verifications WHERE case_id = ?", caseID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func concurrentCloseFixture(t *testing.T) (*Service, *Service, context.Context, Case) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent-close.db")
	firstStore, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstStore.DB().Close() })
	secondStore, err := state.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondStore.DB().Close() })

	clk := testutil.NewClock(time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC))
	firstPurposes := purpose.New(firstStore, clk)
	missionID, err := firstPurposes.CreateMission(ctx, "Review incoming work")
	if err != nil {
		t.Fatal(err)
	}
	seedFixtureEvidence(t, ctx, firstStore, clk)
	first := New(firstStore, clk, firstPurposes)
	initial, err := first.Ensure(ctx, sampleObservation(missionID))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := first.Assess(ctx, AssessmentRequest{
		CaseID: initial.ID, WorkID: initial.CurrentWorkID, RemainingBudget: 4, ProgressSignature: "ready",
		Assessment: workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"ready-evidence"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second := New(secondStore, clk, purpose.New(secondStore, clk))
	return first, second, ctx, ready.Case
}

func TestCloseConcurrentReplay(t *testing.T) {
	t.Run("same request", func(t *testing.T) {
		first, second, ctx, ready := concurrentCloseFixture(t)
		insertWorkflowEvidence(t, first, ctx, "verify-a", "verify-b")
		request := closeVerificationRequest(ready)
		start := make(chan struct{})
		results := make(chan struct {
			record VerificationRecord
			err    error
		}, 2)
		var wg sync.WaitGroup
		for _, svc := range []*Service{first, second} {
			wg.Add(1)
			go func(svc *Service) {
				defer wg.Done()
				<-start
				record, err := svc.Close(ctx, request)
				results <- struct {
					record VerificationRecord
					err    error
				}{record: record, err: err}
			}(svc)
		}
		close(start)
		wg.Wait()
		close(results)

		var records []VerificationRecord
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			records = append(records, result.record)
		}
		if len(records) != 2 || !reflect.DeepEqual(records[0], records[1]) {
			t.Fatalf("concurrent replay records = %+v, want one identical record", records)
		}
		if workflowVerificationCount(t, first, ctx, ready.ID) != 1 {
			t.Fatal("concurrent replay inserted another verification")
		}
	})

	t.Run("snapshot drift", func(t *testing.T) {
		first, second, ctx, ready := concurrentCloseFixture(t)
		insertWorkflowEvidence(t, first, ctx, "verify-a", "verify-b")
		firstRequest := closeVerificationRequest(ready)
		secondRequest := firstRequest
		secondRequest.SnapshotHash = "snapshot-hash-2"
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for index, request := range []VerificationRequest{firstRequest, secondRequest} {
			wg.Add(1)
			go func(index int, request VerificationRequest) {
				defer wg.Done()
				<-start
				_, err := []*Service{first, second}[index].Close(ctx, request)
				results <- err
			}(index, request)
		}
		close(start)
		wg.Wait()
		close(results)

		var successes, conflicts int
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, domain.ErrIntentConflict):
				conflicts++
			default:
				t.Fatalf("concurrent drift error = %v", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("concurrent drift results: successes=%d conflicts=%d, want 1 each", successes, conflicts)
		}
		if workflowVerificationCount(t, first, ctx, ready.ID) != 1 {
			t.Fatal("concurrent drift inserted another verification")
		}
	})
}

func TestCloseRecordsVerificationAndClosesCase(t *testing.T) {
	svc, ctx, ready := readyForVerificationFixture(t)
	insertWorkflowEvidence(t, svc, ctx, "verify-a", "verify-b")
	request := closeVerificationRequest(ready)

	record, err := svc.Close(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.CaseID != ready.ID || record.VerifierID != request.VerifierID || record.VerifierType != request.VerifierType || record.SnapshotHash != request.SnapshotHash || record.SnapshotJSON != request.SnapshotJSON {
		t.Fatalf("verification record = %+v, want request %+v", record, request)
	}
	if !reflect.DeepEqual(record.EvidenceIDs, []domain.ID{"verify-a", "verify-b"}) {
		t.Fatalf("evidence IDs = %v, want normalized order", record.EvidenceIDs)
	}

	replayRequest := request
	replayRequest.EvidenceIDs = []domain.ID{"verify-a", "verify-b"}
	replay, err := svc.Close(ctx, replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay, record) {
		t.Fatalf("replay = %+v, want %+v", replay, record)
	}
	if workflowVerificationCount(t, svc, ctx, ready.ID) != 1 {
		t.Fatal("replay inserted another verification")
	}

	closed, err := svc.Get(ctx, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != Closed || closed.CurrentWorkID != "" || closed.NextWork.Kind != "" {
		t.Fatalf("closed case retained work: %+v", closed)
	}
	var workJSON string
	if err := svc.store.DB().QueryRowContext(ctx, "SELECT next_work_json FROM workflow_cases WHERE case_id = ?", ready.ID).Scan(&workJSON); err != nil {
		t.Fatal(err)
	}
	if workJSON != "{}" {
		t.Fatalf("next_work_json = %q, want {}", workJSON)
	}
}

func TestCloseRejectsVerificationSnapshotDrift(t *testing.T) {
	svc, ctx, ready := readyForVerificationFixture(t)
	insertWorkflowEvidence(t, svc, ctx, "verify-a", "verify-b", "verify-c")
	request := closeVerificationRequest(ready)
	if _, err := svc.Close(ctx, request); err != nil {
		t.Fatal(err)
	}

	hashDrift := request
	hashDrift.SnapshotHash = "snapshot-hash-2"
	if _, err := svc.Close(ctx, hashDrift); !errors.Is(err, domain.ErrIntentConflict) {
		t.Fatalf("hash drift error = %v, want ErrIntentConflict", err)
	}
	evidenceDrift := request
	evidenceDrift.EvidenceIDs = []domain.ID{"verify-c"}
	if _, err := svc.Close(ctx, evidenceDrift); !errors.Is(err, domain.ErrIntentConflict) {
		t.Fatalf("evidence drift error = %v, want ErrIntentConflict", err)
	}
	if workflowVerificationCount(t, svc, ctx, ready.ID) != 1 {
		t.Fatal("drift inserted another verification")
	}
}

func TestCloseRejectsNonReadyCases(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		svc, ctx, active := assessmentFixture(t)
		insertWorkflowEvidence(t, svc, ctx, "verify-a", "verify-b")
		if _, err := svc.Close(ctx, closeVerificationRequest(active)); err == nil {
			t.Fatal("active case was closed")
		}
		if workflowVerificationCount(t, svc, ctx, active.ID) != 0 {
			t.Fatal("active case rejection recorded a verification")
		}
	})

	t.Run("blocked", func(t *testing.T) {
		svc, ctx, active := assessmentFixture(t)
		blocked, err := svc.Assess(ctx, AssessmentRequest{
			CaseID: active.ID, WorkID: active.CurrentWorkID, RemainingBudget: 5, ProgressSignature: "blocked",
			Assessment: workflow.Assessment{Verdict: workflow.Unknown, Reason: "blocked", EvidenceIDs: []string{"blocked-evidence"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		insertWorkflowEvidence(t, svc, ctx, "verify-a", "verify-b")
		if _, err := svc.Close(ctx, closeVerificationRequest(blocked.Case)); err == nil {
			t.Fatal("blocked case was closed")
		}
		if workflowVerificationCount(t, svc, ctx, active.ID) != 0 {
			t.Fatal("blocked case rejection recorded a verification")
		}
	})
}

func TestCloseRequiresExistingEvidence(t *testing.T) {
	svc, ctx, ready := readyForVerificationFixture(t)
	request := closeVerificationRequest(ready)
	request.EvidenceIDs = []domain.ID{"missing-verification-evidence"}
	if _, err := svc.Close(ctx, request); err == nil {
		t.Fatal("missing evidence was accepted")
	}
	current, err := svc.Get(ctx, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != ReadyForVerification || workflowVerificationCount(t, svc, ctx, ready.ID) != 0 {
		t.Fatalf("failed close changed ready case: %+v", current)
	}
}

func TestCloseRejectsInactiveMission(t *testing.T) {
	svc, ctx, ready := readyForVerificationFixture(t)
	insertWorkflowEvidence(t, svc, ctx, "verify-a", "verify-b")
	if err := svc.purposes.DeactivateMission(ctx, ready.MissionID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Close(ctx, closeVerificationRequest(ready)); !errors.Is(err, domain.ErrInvalidPurpose) {
		t.Fatalf("inactive Mission error = %v, want ErrInvalidPurpose", err)
	}
}

func TestRejectRecordsReasonAndBlocksCase(t *testing.T) {
	svc, ctx, ready := readyForVerificationFixture(t)
	insertWorkflowEvidence(t, svc, ctx, "reject-a", "reject-b")

	record, err := svc.Reject(ctx, ready.ID, "publication mismatch", []domain.ID{"reject-b", "reject-a", "reject-b"})
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.CaseID != ready.ID || record.VerifierType != "REJECT" {
		t.Fatalf("rejection record = %+v", record)
	}
	if !reflect.DeepEqual(record.EvidenceIDs, []domain.ID{"reject-a", "reject-b"}) {
		t.Fatalf("rejection evidence IDs = %v, want normalized order", record.EvidenceIDs)
	}
	var snapshot map[string]string
	if err := json.Unmarshal([]byte(record.SnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["reason"] != "publication mismatch" {
		t.Fatalf("rejection snapshot = %+v", snapshot)
	}
	blocked, err := svc.Get(ctx, ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.State != Blocked {
		t.Fatalf("rejected case state = %s, want BLOCKED", blocked.State)
	}
}

func TestRejectOnlyAllowsReadyCases(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		svc, ctx, active := assessmentFixture(t)
		insertWorkflowEvidence(t, svc, ctx, "reject-evidence")
		if _, err := svc.Reject(ctx, active.ID, "not ready", []domain.ID{"reject-evidence"}); err == nil {
			t.Fatal("active case was rejected")
		}
	})

	t.Run("already rejected", func(t *testing.T) {
		svc, ctx, ready := readyForVerificationFixture(t)
		insertWorkflowEvidence(t, svc, ctx, "reject-evidence")
		if _, err := svc.Reject(ctx, ready.ID, "first rejection", []domain.ID{"reject-evidence"}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Reject(ctx, ready.ID, "second rejection", []domain.ID{"reject-evidence"}); err == nil {
			t.Fatal("blocked case was rejected again")
		}
		if workflowVerificationCount(t, svc, ctx, ready.ID) != 1 {
			t.Fatal("second rejection inserted another verification")
		}
	})
}

func TestListReadyForVerificationFiltersAndOrders(t *testing.T) {
	svc, _, missionID, ctx := setupEnsure(t)
	ensureReady := func(object string) Case {
		t.Helper()
		observation := sampleObservation(missionID)
		observation.ObjectID = object
		created, err := svc.Ensure(ctx, observation)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := svc.Assess(ctx, AssessmentRequest{
			CaseID: created.ID, WorkID: created.CurrentWorkID, RemainingBudget: 4, ProgressSignature: "ready",
			Assessment: workflow.Assessment{Verdict: workflow.Ready, EvidenceIDs: []string{"ready-evidence"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return ready.Case
	}

	first := ensureReady("ready-1")
	second := ensureReady("ready-2")
	activeObservation := sampleObservation(missionID)
	activeObservation.ObjectID = "active"
	active, err := svc.Ensure(ctx, activeObservation)
	if err != nil {
		t.Fatal(err)
	}
	blockedObservation := sampleObservation(missionID)
	blockedObservation.ObjectID = "blocked"
	blocked, err := svc.Ensure(ctx, blockedObservation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Assess(ctx, AssessmentRequest{
		CaseID: blocked.ID, WorkID: blocked.CurrentWorkID, RemainingBudget: 5, ProgressSignature: "blocked",
		Assessment: workflow.Assessment{Verdict: workflow.Unknown, Reason: "blocked", EvidenceIDs: []string{"blocked-evidence"}},
	}); err != nil {
		t.Fatal(err)
	}

	otherMission := domain.NewID("mission")
	now := svc.clock.Now().UTC().Format(time.RFC3339Nano)
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO missions(mission_id,statement,active,created_at,deactivated_at)
		VALUES (?,'other mission',0,?,?)`, otherMission, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.DB().ExecContext(ctx, `
		INSERT INTO workflow_cases(case_id,mission_id,source,object_id,revision_id,observation_evidence_id,
			initial_request_json,grant_json,state,current_work_id,next_work_json,completed_steps,max_steps,
			remaining_budget,progress_signature,created_at,updated_at)
		VALUES (?,?,'ado','other-ready','r1','observation','{}','{"Capabilities":["read"],"Actions":null}',
			'READY_FOR_VERIFICATION','','{}',1,3,2,'ready',?,?)`, domain.NewID("case"), otherMission, now, now); err != nil {
		t.Fatal(err)
	}

	got, err := svc.ListReadyForVerification(ctx, missionID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{string(first.ID), string(second.ID)}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ListReadyForVerification = %+v, want %v", got, want)
	}
	for index := range want {
		if string(got[index].ID) != want[index] || got[index].State != ReadyForVerification {
			t.Fatalf("ready case %d = %+v, want ID %s", index, got[index], want[index])
		}
	}
	if got[0].ID == active.ID {
		t.Fatal("active case appeared in ready list")
	}
}
