package ghtriage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/wake"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"strings"
	"testing"
	"time"
)

type publicationPolicy struct{ required []domain.ID }

func (p publicationPolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	outcome := domain.PolicyAllow
	if len(p.required) > 0 {
		outcome = domain.PolicyRequireApproval
	}
	return domain.PolicyDecision{RequiredApprovals: p.required, ID: domain.NewID("decision"), Outcome: outcome, PolicySetID: "publication_policy", PolicySetHash: "publication-v1", PolicyCapabilitiesHash: "publication-caps", InputDigest: "fixture", EvaluatedAt: in.Now}, nil
}

type publicationProvider struct {
	name, cap       string
	writes, lookups int
	unknown         bool
	noEffect        bool
	onDispatch      func()
}

func (p *publicationProvider) Name() string       { return p.name }
func (p *publicationProvider) Capability() string { return p.cap }
func (*publicationProvider) EnforcementLevel() domain.EnforcementLevel {
	return domain.EnforcementEnforced
}
func (*publicationProvider) AdapterVersion() string                   { return "1" }
func (*publicationProvider) AdapterVersionSemanticallyRelevant() bool { return true }
func (*publicationProvider) SettleWindow() time.Duration              { return 0 }
func (*publicationProvider) MinimumRisk() string                      { return "LOW" }
func (p *publicationProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	return json.Marshal(d)
}
func (p *publicationProvider) CostProfile(operations.IntentDescriptor) (operations.CostProfile, error) {
	return operations.CostProfile{MaxExposure: 1, Enforceability: resources.Enforceability{CostControl: resources.CostTechnicallyCapped, Source: "fixture bounded writes"}}, nil
}
func (p *publicationProvider) Dispatch(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.writes++
	if p.onDispatch != nil {
		p.onDispatch()
	}
	if p.noEffect {
		return operations.ProviderOutcome{State: domain.OperationConfirmedNoEffect}, nil
	}
	if p.unknown {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
	}
	return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "https://github.com/o/r/pull/7", ActualCost: 1}, nil
}
func (p *publicationProvider) LookupOutcome(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	p.lookups++
	if p.unknown {
		return operations.ProviderOutcome{State: domain.OperationOutcomeUnknown}, nil
	}
	return operations.ProviderOutcome{State: domain.OperationConfirmedEffect, ProviderReference: "https://github.com/o/r/pull/7", ActualCost: 1}, nil
}
func publicationOps(t *testing.T, f *driverFixture) (*operations.Service, *publicationProvider, *publicationProvider) {
	t.Helper()
	_, err := f.store.DB().ExecContext(f.ctx, `INSERT INTO policy_sets(policy_set_id,version,module_name,module,policy_hash,capabilities_hash,active,created_at) VALUES ('publication_policy',1,'fixture','package fixture','publication-v1','publication-caps',1,?)`, f.clock.Now().Format("2006-01-02T15:04:05Z07:00"))
	if err != nil {
		t.Fatal(err)
	}
	b := &publicationProvider{name: ghpublish.BranchProviderName, cap: "github.repo.publish"}
	p := &publicationProvider{name: ghpublish.PRProviderName, cap: "github.pr.create"}
	return operations.New(f.store, f.clock, f.execSvc, publicationPolicy{}, resources.New(f.store, f.clock), approvals.New(f.store, f.clock), "publication_collective", b, p), b, p
}
func grantPublication(t *testing.T, f *driverFixture, id domain.ID) {
	t.Helper()
	c, err := f.cases.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	c.Grant.Capabilities = append(c.Grant.Capabilities, "github.repo.publish", "github.pr.create")
	c.Grant.Actions = append(c.Grant.Actions, "github.repo.publish", "github.pr.create")
	raw, _ := json.Marshal(c.Grant)
	if _, err = f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json=? WHERE case_id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
}
func TestPublicationAcceptedChainAndReceiptReplay(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	r, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || !r.Accepted {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || got.Pending || got.Record == nil || b.writes != 1 || p.writes != 1 {
		t.Fatalf("%+v %v writes=%d/%d", got, err, b.writes, p.writes)
	}
	if _, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil || b.writes != 1 || p.writes != 1 {
		t.Fatalf("a published case must not publish again: %v writes=%d/%d", err, b.writes, p.writes)
	}
	task, err := f.execSvc.Task(f.ctx, got.Record.TaskID)
	if err != nil || task.State != domain.TaskAwaitingVerification {
		t.Fatal("publication must await independent verification", task.State, err)
	}
}
func TestConfirmedPublicationMovesCaseToReadyForVerification(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	if r, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath); err != nil || !r.Accepted {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, _, _ := publicationOps(t, f)
	got, err := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops).Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || got.Record == nil {
		t.Fatalf("%+v %v", got, err)
	}
	assertAssessed(t, f, id, workflowcase.ReadyForVerification, "published:"+got.Record.PRReference)
	c, _ := f.cases.Get(f.ctx, id)
	if c.NextWork.Kind != "" || c.CurrentWorkID != "" {
		t.Fatalf("ready case keeps work: %+v", c)
	}
}
func TestPublicationRejectsMissingAcceptanceAndGrant(t *testing.T) {
	for _, mode := range []string{"unreviewed", "no-grant"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, id, _ := implementedFixture(t)
			if mode == "no-grant" {
				_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				grantPublication(t, f, id)
			}
			ops, b, p := publicationOps(t, f)
			if _, err := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops).Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil || b.writes != 0 || p.writes != 0 {
				t.Fatal("unauthorized publication")
			}
		})
	}
}
func TestPublicationUnknownBranchStopsPR(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	b.unknown = true
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !got.Pending || b.writes != 1 || p.writes != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	got, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !got.Busy || b.writes != 1 || p.writes != 0 {
		t.Fatalf("repeat %+v %v", got, err)
	}
}

func TestPublicationResumesStagedReceiptAndRejectsRevocation(t *testing.T) {
	for _, mode := range []string{"resume", "revoked", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg, id, _ := implementedFixture(t)
			_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
			if err != nil {
				t.Fatal(err)
			}
			grantPublication(t, f, id)
			ops, b, p := publicationOps(t, f)
			pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
			if _, err = f.store.DB().ExecContext(f.ctx, `CREATE TRIGGER interrupt_publication BEFORE INSERT ON attempt_completion_records BEGIN SELECT RAISE(ABORT,'interrupted'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil {
				t.Fatal("completion interruption missed")
			}
			f.store.DB().ExecContext(f.ctx, `DROP TRIGGER interrupt_publication`)
			switch mode {
			case "revoked":
				f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":[],"Actions":[]}' WHERE case_id=?`, id)
			case "expired":
				f.clock.Advance(ghtriage.PublicationLeaseDuration + time.Second)
			}
			got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
			if mode == "resume" {
				if err != nil || !got.Reused || got.Record == nil {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid staged completion accepted")
			}
			if b.writes != 1 || p.writes != 1 {
				t.Fatal("effects repeated")
			}
		})
	}
}
func TestPublicationRevocationAfterBranchStopsPR(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	b.onDispatch = func() {
		f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":[],"Actions":[]}' WHERE case_id=?`, id)
	}
	if _, err = ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops).Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil || b.writes != 1 || p.writes != 0 {
		t.Fatal("revoked authority published PR")
	}
}
func TestPublicationClaimsBeforeConcurrentEffects(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	b.onDispatch = func() { close(entered); <-release }
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	first := make(chan error, 1)
	go func() { _, e := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); first <- e }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("not dispatched")
	}
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	close(release)
	e := <-first
	if err != nil || e != nil || !got.Busy || b.writes != 1 || p.writes != 1 {
		t.Fatalf("%+v %v / %v", got, err, e)
	}
}

func TestPublicationReconcilesUnknownPRWithoutRepeatingBranch(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	p.unknown = true
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !got.Pending {
		t.Fatalf("%+v %v", got, err)
	}
	f.clock.Advance(ghtriage.PublicationLeaseDuration + time.Second)
	if _, err = wake.New(f.store, f.clock, nil).RecoverExpiredLeases(f.ctx); err != nil {
		t.Fatal(err)
	}
	p.unknown = false
	got, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || got.Record == nil || got.Pending || b.writes != 1 || p.writes != 1 || p.lookups != 1 {
		t.Fatalf("%+v %v writes=%d/%d lookup=%d", got, err, b.writes, p.writes, p.lookups)
	}
}

func TestPublicationResumesExactApprovedOperations(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	_, b, p := publicationOps(t, f)
	a := approvals.New(f.store, f.clock)
	ops := operations.New(f.store, f.clock, f.execSvc, publicationPolicy{required: []domain.ID{"owner"}}, resources.New(f.store, f.clock), a, "publication_collective", b, p)
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	first, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !first.Held || first.ApprovalID == "" || first.OperationID == "" || b.writes != 0 || p.writes != 0 {
		t.Fatalf("first %+v %v", first, err)
	}
	record, err := a.Get(f.ctx, first.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Approve(f.ctx, record.ID, "owner", record.RequestDigest); err != nil {
		t.Fatal(err)
	}
	second, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !second.Held || second.ApprovalID == first.ApprovalID || b.writes != 1 || p.writes != 0 {
		t.Fatalf("second %+v %v writes=%d/%d", second, err, b.writes, p.writes)
	}
	record, err = a.Get(f.ctx, second.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Approve(f.ctx, record.ID, "owner", record.RequestDigest); err != nil {
		t.Fatal(err)
	}
	third, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || third.Record == nil || third.Held || b.writes != 1 || p.writes != 1 {
		t.Fatalf("third %+v %v", third, err)
	}
	var requests int
	if err = f.store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM approval_requests`).Scan(&requests); err != nil || requests != 2 {
		t.Fatal("approval rebound", requests, err)
	}
}
func TestPublicationRejectsUpstreamCompletionMismatch(t *testing.T) {
	for _, class := range []string{ghtriage.TaskClass, ghtriage.PlanReviewWorkKind} {
		t.Run(class, func(t *testing.T) {
			f, cfg, id, _ := implementedFixture(t)
			_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
			if err != nil {
				t.Fatal(err)
			}
			grantPublication(t, f, id)
			ops, b, p := publicationOps(t, f)
			c, err := f.cases.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var attempt domain.ID
			if err = f.store.DB().QueryRowContext(f.ctx, `SELECT current_attempt_id FROM tasks WHERE task_class=? AND purpose_id=?`, class, c.MissionID).Scan(&attempt); err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"version":1,"evidence_ids":["` + c.ObservationEvidenceID + `"]}`)
			hash := sha256.Sum256(raw)
			if _, err = f.store.DB().ExecContext(f.ctx, `UPDATE attempt_completion_records SET manifest_json=?,manifest_hash=? WHERE attempt_id=?`, string(raw), hex.EncodeToString(hash[:]), attempt); err != nil {
				t.Fatal(err)
			}
			if _, err = ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops).Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil || b.writes != 0 || p.writes != 0 {
				t.Fatal("mismatched upstream completion published")
			}
		})
	}
}

// assertAssessed checks the case state and that the latest assessment reason starts with reason.
func assertAssessed(t *testing.T, f *driverFixture, id domain.ID, state workflowcase.State, reason string) {
	t.Helper()
	c, err := f.cases.Get(f.ctx, id)
	if err != nil || c.State != state {
		t.Fatalf("state %s want %s (%v)", c.State, state, err)
	}
	records, err := f.cases.ListAssessments(f.ctx, id)
	if err != nil || len(records) == 0 {
		t.Fatalf("assessments %v", err)
	}
	var result workflowcase.AssessmentResult
	if err := json.Unmarshal([]byte(records[len(records)-1].ResultJSON), &result); err != nil || !strings.HasPrefix(result.Decision.Reason, reason) {
		t.Fatalf("reason %q want prefix %q (%v)", result.Decision.Reason, reason, err)
	}
}

// restoreActive puts a moved case back to ACTIVE with its pre-move work, simulating a crash after
// the stage's record was saved but before the case move was committed. Assess commits the state
// and its assessment row together, so the move's assessment row is removed as well.
func restoreActive(t *testing.T, f *driverFixture, id domain.ID, before workflowcase.Case) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(f.ctx, `DELETE FROM workflow_assessments WHERE case_id=? AND work_id=?`, id, before.CurrentWorkID); err != nil {
		t.Fatal(err)
	}
	nextWork, _ := json.Marshal(before.NextWork)
	if _, err := f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET state='ACTIVE', current_work_id=?, next_work_json=? WHERE case_id=?`, before.CurrentWorkID, string(nextWork), id); err != nil {
		t.Fatal(err)
	}
}
func TestPublicationReplayAfterCrashPerformsTheMove(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	if r, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath); err != nil || !r.Accepted {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	before, _ := f.cases.Get(f.ctx, id)
	ops, b, p := publicationOps(t, f)
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || got.Record == nil {
		t.Fatalf("%+v %v", got, err)
	}
	// A repeat on the READY case reports that it is past the stage.
	if _, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); !errors.Is(err, ghtriage.ErrCaseNotAtStage) {
		t.Fatalf("want ErrCaseNotAtStage, got %v", err)
	}
	restoreActive(t, f, id, before)
	replay, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !replay.Reused || replay.EvidenceID != got.EvidenceID || b.writes != 1 || p.writes != 1 {
		t.Fatalf("replay %+v %v writes=%d/%d", replay, err, b.writes, p.writes)
	}
	assertAssessed(t, f, id, workflowcase.ReadyForVerification, "published:"+got.Record.PRReference)
	// Revoked authority is still checked on the replay path.
	restoreActive(t, f, id, before)
	f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json='{"Capabilities":[],"Actions":[]}' WHERE case_id=?`, id)
	if _, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); err == nil {
		t.Fatal("revoked receipt replay")
	}
}

func TestPublicationNoEffectIsTerminalForCandidate(t *testing.T) {
	f, cfg, id, _ := implementedFixture(t)
	_, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil {
		t.Fatal(err)
	}
	grantPublication(t, f, id)
	ops, b, p := publicationOps(t, f)
	b.noEffect = true // the base branch moved: the provider confirms nothing was written
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	got, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || !got.Blocked || !strings.Contains(got.Reason, "rebuilt") {
		t.Fatalf("%+v %v", got, err)
	}
	c, err := f.cases.Get(f.ctx, id)
	if err != nil || c.State != workflowcase.Blocked {
		t.Fatalf("case must be BLOCKED: %+v %v", c.State, err)
	}
	// Lease expiry must not start a new attempt or a new approval request.
	f.clock.Advance(ghtriage.PublicationLeaseDuration + time.Second)
	if _, err = pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main"); !errors.Is(err, ghtriage.ErrCaseNotAtStage) || b.writes != 1 || p.writes != 0 {
		t.Fatalf("blocked case retried: %v writes=%d/%d", err, b.writes, p.writes)
	}
}
