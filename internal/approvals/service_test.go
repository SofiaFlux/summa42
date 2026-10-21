package approvals

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func newHarness(t *testing.T) (*Service, *testutil.Clock, domain.ID) {
	t.Helper()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC))
	return New(store, clk), clk, domain.ID("owner_1")
}

func request(owner domain.ID, digest string, expires time.Time) CreateRequest {
	return CreateRequest{
		SubjectKind:       "EXTERNAL_OPERATION",
		SubjectID:         "operation_1",
		RequestDigest:     digest,
		PolicyDecisionID:  "decision_1",
		RequiredApprovers: []domain.ID{owner},
		RequestedBy:       "cube_1",
		ExpiresAt:         expires,
	}
}

func TestCreateIsIdempotentForExactSubjectDigest(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()
	first, err := svc.Create(ctx, request(owner, "digest-a", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Create(ctx, request(owner, "digest-a", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("same subject+digest created %s then %s", first.ID, second.ID)
	}

	changed, err := svc.Create(ctx, request(owner, "digest-b", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID == first.ID {
		t.Fatal("changed digest reused prior approval")
	}
}

func TestApprovalIsBoundToExactDigestAndRequiredApprover(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()
	record, err := svc.Create(ctx, request(owner, "digest-a", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, record.ID, "other_principal", "digest-a"); err == nil {
		t.Fatal("non-required approver was accepted")
	}
	if err := svc.Approve(ctx, record.ID, owner, "digest-b"); err == nil {
		t.Fatal("mismatched digest was approved")
	}
	if err := svc.Approve(ctx, record.ID, owner, "digest-a"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, record.ID)
	if err != nil || got.State != domain.ApprovalApproved {
		t.Fatalf("exact approved digest state=%s err=%v", got.State, err)
	}
}

func TestExpiredAndRejectedApprovalsFailClosed(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()

	expired, err := svc.Create(ctx, request(owner, "expiring", clk.Now().Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)
	if err := svc.Approve(ctx, expired.ID, owner, "expiring"); err == nil {
		t.Fatal("expired request was approved")
	}

	rejected, err := svc.Create(ctx, CreateRequest{
		SubjectKind: "ADAPTATION_GRANT", SubjectID: "grant_request_1", RequestDigest: "grant-digest",
		PolicyDecisionID: "decision_2", RequiredApprovers: []domain.ID{owner},
		RequestedBy: "cube_1", ExpiresAt: clk.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(ctx, rejected.ID, owner, "grant-digest"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, rejected.ID, owner, "grant-digest"); err == nil {
		t.Fatal("rejected request became approved")
	}
}

func TestPendingReturnsOnlyLivePendingRequests(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()
	live, err := svc.Create(ctx, request(owner, "live", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Create(ctx, request(owner, "approved", clk.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, approved.ID, owner, "approved"); err != nil {
		t.Fatal(err)
	}
	items, err := svc.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != live.ID {
		t.Fatalf("pending=%v, want only %s", items, live.ID)
	}
}


func TestAllRequiredApproversMustApprove(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()
	security := domain.ID("security_1")
	record, err := svc.Create(ctx, CreateRequest{
		SubjectKind: "EXTERNAL_OPERATION", SubjectID: "operation_multi", RequestDigest: "multi-digest",
		PolicyDecisionID: "decision_multi", RequiredApprovers: []domain.ID{owner, security},
		RequestedBy: "cube_1", ExpiresAt: clk.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.Approve(ctx, record.ID, owner, "multi-digest"); err != nil {
		t.Fatal(err)
	}
	partial, err := svc.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partial.State != domain.ApprovalPending || len(partial.Decisions) != 1 {
		t.Fatalf("partial approval = state:%s decisions:%v", partial.State, partial.Decisions)
	}

	if err := svc.Approve(ctx, record.ID, security, "multi-digest"); err != nil {
		t.Fatal(err)
	}
	complete, err := svc.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if complete.State != domain.ApprovalApproved || len(complete.Decisions) != 2 {
		t.Fatalf("complete approval = state:%s decisions:%v", complete.State, complete.Decisions)
	}
}

func TestAnyRequiredApproverCanRejectMultiPartyRequest(t *testing.T) {
	svc, clk, owner := newHarness(t)
	ctx := context.Background()
	security := domain.ID("security_1")
	record, err := svc.Create(ctx, CreateRequest{
		SubjectKind: "EXTERNAL_OPERATION", SubjectID: "operation_reject_multi", RequestDigest: "reject-multi",
		PolicyDecisionID: "decision_reject_multi", RequiredApprovers: []domain.ID{owner, security},
		RequestedBy: "cube_1", ExpiresAt: clk.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Approve(ctx, record.ID, owner, "reject-multi"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(ctx, record.ID, security, "reject-multi"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.ApprovalRejected || len(got.Decisions) != 2 {
		t.Fatalf("rejected multi-party approval = state:%s decisions:%v", got.State, got.Decisions)
	}
}
