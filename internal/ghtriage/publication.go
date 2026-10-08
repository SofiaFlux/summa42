package ghtriage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"time"
)

const KindPublication = "github.issue.publication"
const PublicationVersion = "github.issue.publication.v1"
const PublicationLeaseDuration = 10 * time.Minute

var publicationCapabilities = []string{"github.repo.publish", "github.pr.create"}

type PublicationRecord struct {
	Schema                   string    `json:"schema"`
	CaseID                   domain.ID `json:"case_id"`
	WorkID                   domain.ID `json:"work_id"`
	TaskID                   domain.ID `json:"task_id"`
	AttemptID                domain.ID `json:"attempt_id"`
	Fence                    int64     `json:"fence"`
	ImplementationEvidenceID domain.ID `json:"implementation_evidence_id"`
	ReviewEvidenceID         domain.ID `json:"review_evidence_id"`
	Repository               string    `json:"repository"`
	BaseBranch               string    `json:"base_branch"`
	HeadBranch               string    `json:"head_branch"`
	repoworkspace.Candidate
	BranchOperationID domain.ID `json:"branch_operation_id"`
	PROperationID     domain.ID `json:"pr_operation_id"`
	BranchReference   string    `json:"branch_reference"`
	PRReference       string    `json:"pr_reference"`
}
type PublicationResult struct {
	Held        bool               `json:"held"`
	ApprovalID  domain.ID          `json:"approval_id,omitempty"`
	OperationID domain.ID          `json:"operation_id,omitempty"`
	Busy        bool               `json:"busy"`
	Pending     bool               `json:"pending"`
	Reused      bool               `json:"reused"`
	EvidenceID  domain.ID          `json:"evidence_id,omitempty"`
	Record      *PublicationRecord `json:"record,omitempty"`
}
type Publisher struct {
	cases      *workflowcase.Service
	exec       *execution.Service
	verify     *verification.Service
	evidence   *evidence.Store
	operations *operations.Service
}

func NewPublisher(c *workflowcase.Service, x *execution.Service, v *verification.Service, e *evidence.Store, o *operations.Service) *Publisher {
	return &Publisher{c, x, v, e, o}
}
func publicationAuthorized(c workflowcase.Case) bool {
	for _, cap := range publicationCapabilities {
		if !contains(c.Grant.Capabilities, cap) || !contains(c.Grant.Actions, cap) {
			return false
		}
	}
	return implementationAuthorized(c)
}
func (p *Publisher) Publish(ctx context.Context, id domain.ID, source, baseBranch string) (PublicationResult, error) {
	if ctx == nil || p == nil || p.cases == nil || p.exec == nil || p.verify == nil || p.evidence == nil || p.operations == nil {
		return PublicationResult{}, errors.New("publisher not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, PublicationLeaseDuration-time.Minute)
	defer cancel()
	c, err := p.cases.Get(ctx, id)
	if err != nil {
		return PublicationResult{}, err
	}
	if !publicationAuthorized(c) {
		return PublicationResult{}, errors.New("current Case lacks explicit publication capabilities/actions")
	}
	loader := NewImplementer(p.cases, p.exec, p.verify, p.evidence, nil)
	input, planID, planReviewID, planReviewTask, err := loader.loadInput(ctx, c)
	if err != nil {
		return PublicationResult{}, err
	}
	raw, found, err := findAcceptedIndex(ctx, p.evidence, c)
	if err != nil {
		return PublicationResult{}, err
	}
	if !found {
		return PublicationResult{}, errors.New("accepted triage missing")
	}
	var index acceptedIndex
	if err = json.Unmarshal(raw, &index); err != nil {
		return PublicationResult{}, err
	}
	triage, found, err := p.exec.FindByIdempotencyKey(ctx, string(index.TaskID))
	if err != nil {
		return PublicationResult{}, err
	}
	if !found || triage.State != domain.TaskSucceeded {
		return PublicationResult{}, errors.New("accepted triage Task missing")
	}
	// Accepted upstream evidence must actually have been completed by that attempt.
	triageIDs, err := p.verify.CompletionEvidence(ctx, triage.ID, triage.CurrentAttemptID)
	if err != nil {
		return PublicationResult{}, err
	}
	matched := false
	for _, evidenceID := range triageIDs {
		if evidenceID == index.DecisionEvidenceID {
			matched = true
		}
	}
	if !matched {
		return PublicationResult{}, errors.New("triage completion does not contain accepted decision")
	}
	planReviewIDs, err := p.verify.CompletionEvidence(ctx, planReviewTask.ID, planReviewTask.CurrentAttemptID)
	if err != nil || len(planReviewIDs) != 1 || planReviewIDs[0] != planReviewID {
		return PublicationResult{}, errors.New("plan-review completion differs from accepted verdict")
	}
	decisionObject, decisionRaw, err := p.evidence.Get(ctx, index.DecisionEvidenceID)
	if err != nil {
		return PublicationResult{}, err
	}
	var decision Decision
	if err = decodeExactlyOne(decisionRaw, &decision); err != nil {
		return PublicationResult{}, err
	}
	if decisionObject.Kind != KindDecision || decision.Validate() != nil || decision.Repository != input.Plan.Repository || decision.Issue != input.Snapshot.Issue || decision.Revision != c.RevisionID || decision.SnapshotEvidenceID != c.ObservationEvidenceID || decision.FinalDisposition() != DispositionReadyToPlan {
		return PublicationResult{}, errors.New("accepted triage decision identity/disposition differs")
	}
	implTask, found, err := p.exec.FindByIdempotencyKey(ctx, string(c.CurrentWorkID))
	if err != nil {
		return PublicationResult{}, err
	}
	if !found || implTask.State != domain.TaskSucceeded || implTask.TaskClass != ImplementationWorkKind || implTask.Purpose != (domain.PurposeRef{Kind: domain.PurposeMission, ID: c.MissionID}) {
		return PublicationResult{}, errors.New("independently accepted implementation required")
	}
	impl, err := loader.replay(ctx, implTask, ImplementationRecord{Schema: ImplementationVersion, CaseID: c.ID, WorkID: c.CurrentWorkID, PlanEvidenceID: planID, ContextEvidenceID: input.Plan.Source.ContextEvidenceID, ReviewEvidenceID: planReviewID}, input.Plan)
	if err != nil {
		return PublicationResult{}, err
	}
	a, found, err := p.verify.FindAcceptance(ctx, implTask.ID)
	if err != nil {
		return PublicationResult{}, err
	}
	if !found || a.AttemptID != implTask.CurrentAttemptID || a.VerifierID != implementationVerifierID || a.VerifierType != implementationVerifierType || !a.CriteriaMet || len(a.EvidenceIDs) != 2 || (a.EvidenceIDs[0] != impl.EvidenceID && a.EvidenceIDs[1] != impl.EvidenceID) {
		return PublicationResult{}, errors.New("implementation lacks exact independent acceptance")
	}
	reviewTask, found, err := p.exec.FindByIdempotencyKey(ctx, "code-review:"+string(c.CurrentWorkID))
	if err != nil {
		return PublicationResult{}, err
	}
	if !found || reviewTask.State != domain.TaskSucceeded || reviewTask.TaskClass != KindCodeReview || reviewTask.Purpose != implTask.Purpose {
		return PublicationResult{}, errors.New("accepted code-review Task missing")
	}
	reviewID := a.EvidenceIDs[0]
	if reviewID == impl.EvidenceID {
		reviewID = a.EvidenceIDs[1]
	}
	ra, found, err := p.verify.FindAcceptance(ctx, reviewTask.ID)
	if err != nil {
		return PublicationResult{}, err
	}
	if !found || ra.AttemptID != reviewTask.CurrentAttemptID || ra.VerifierID != codeReviewVerifierID || ra.VerifierType != codeReviewVerifierType || !ra.CriteriaMet || len(ra.EvidenceIDs) != 1 || ra.EvidenceIDs[0] != reviewID {
		return PublicationResult{}, errors.New("code-review acceptance differs")
	}
	ids, err := p.verify.CompletionEvidence(ctx, reviewTask.ID, reviewTask.CurrentAttemptID)
	if err != nil || len(ids) != 1 || ids[0] != reviewID {
		return PublicationResult{}, errors.New("code-review completion differs")
	}
	object, _, err := p.evidence.Get(ctx, impl.EvidenceID)
	if err != nil {
		return PublicationResult{}, err
	}
	reviewObject, reviewRaw, err := p.evidence.Get(ctx, reviewID)
	if err != nil {
		return PublicationResult{}, err
	}
	review, err := validateCodeReview(reviewObject, reviewRaw, reviewTask, CodeReviewRecord{Schema: CodeReviewVersion, CaseID: c.ID, WorkID: c.CurrentWorkID, ImplementationEvidenceID: impl.EvidenceID, ImplementationHash: object.ContentHash, ImplementationTaskID: implTask.ID, ImplementationAttemptID: implTask.CurrentAttemptID, ImplementationFence: implTask.CurrentFence, Candidate: impl.Record.Candidate})
	if err != nil {
		return PublicationResult{}, err
	}
	if review.Verdict != "ACCEPT" || !impl.Record.AllPassed {
		return PublicationResult{}, errors.New("passing accepted candidate required")
	}
	observe := func() error { return observeReview(ctx, source, input, *impl.Record) }
	if err = observe(); err != nil {
		return PublicationResult{}, err
	}
	exported, err := repoworkspace.ExportCandidate(ctx, impl.Record.Workspace, impl.Record.Candidate)
	if err != nil {
		return PublicationResult{}, err
	}
	workHash := sha256.Sum256([]byte(c.CurrentWorkID))
	head := fmt.Sprintf("summa42/issue-%d-%s", input.Snapshot.Issue, hex.EncodeToString(workHash[:8]))
	markerHash := sha256.Sum256([]byte(string(c.CurrentWorkID) + ":" + impl.Record.CandidateSHA))
	marker := "summa42:publication:" + hex.EncodeToString(markerHash[:])
	branch := ghpublish.BranchIntent{Repository: input.Plan.Repository, BaseBranch: baseBranch, HeadBranch: head, Export: exported}
	pr := ghpublish.PRIntent{Repository: input.Plan.Repository, BaseBranch: baseBranch, HeadBranch: head, BaseSHA: impl.Record.BaseSHA, HeadSHA: impl.Record.CandidateSHA, Title: fmt.Sprintf("Summa42: issue #%d", input.Snapshot.Issue), Body: fmt.Sprintf("Reviewed candidate for issue #%d.\n\nImplementation evidence: %s\nIndependent review evidence: %s\nCandidate: %s\n\n<!-- %s -->", input.Snapshot.Issue, impl.EvidenceID, reviewID, impl.Record.CandidateSHA, marker), Marker: marker}
	chainGuard := (&CodeReviewer{cases: p.cases}).guard(c, implTask, triage, planReviewTask, reviewTask, implTask)
	guard := func(ctx context.Context, tx *sql.Tx) error {
		if err := chainGuard(ctx, tx); err != nil {
			return err
		}
		current, err := p.cases.GuardCurrentWork(ctx, tx, c.ID, c.CurrentWorkID)
		if err != nil {
			return err
		}
		if !publicationAuthorized(current) {
			return errors.New("publication authority revoked")
		}
		return nil
	}
	expected := PublicationRecord{Schema: PublicationVersion, CaseID: c.ID, WorkID: c.CurrentWorkID, ImplementationEvidenceID: impl.EvidenceID, ReviewEvidenceID: reviewID, Repository: input.Plan.Repository, BaseBranch: baseBranch, HeadBranch: head, Candidate: impl.Record.Candidate}
	payload, _ := json.Marshal(struct {
		Record PublicationRecord
		Branch ghpublish.BranchIntent
		PR     ghpublish.PRIntent
	}{expected, branch, pr})
	request := execution.TaskRequest{Purpose: implTask.Purpose, TaskClass: KindPublication, Objective: "Publish reviewed candidate " + impl.Record.CandidateSHA + " as a draft PR", PayloadJSON: payload, IdempotencyKey: "publication:" + string(c.CurrentWorkID), AcceptanceCriteria: []string{"Exact candidate branch and draft PR require independent CI verification"}, RequiredCapabilities: publicationCapabilities, AuthorityCeiling: publicationCapabilities, RequiredEnforcement: domain.EnforcementEnforced, ResourceEnvelopeID: implTask.ResourceEnvelopeID}
	task, err := p.exec.CreateTaskWithGuard(ctx, request, guard)
	if err != nil {
		return PublicationResult{}, err
	}
	var receipt PublicationRecord
	var receiptObject evidence.EvidenceObject
	reused := task.State != domain.TaskEligible
	validate := func(o evidence.EvidenceObject, b []byte) error {
		if err := decodeExactlyOne(b, &receipt); err != nil {
			return err
		}
		want := expected
		want.TaskID = task.ID
		want.AttemptID = task.CurrentAttemptID
		want.Fence = task.CurrentFence
		want.BranchOperationID = receipt.BranchOperationID
		want.PROperationID = receipt.PROperationID
		want.BranchReference = receipt.BranchReference
		want.PRReference = receipt.PRReference
		x, _ := json.Marshal(want)
		y, _ := json.Marshal(receipt)
		if o.Kind != KindPublication || !bytes.Equal(x, y) || receipt.BranchOperationID == "" || receipt.PROperationID == "" || receipt.BranchReference == "" || receipt.PRReference == "" {
			return errors.New("publication receipt binding differs")
		}
		return nil
	}
	receiptGuard := func(ctx context.Context, tx *sql.Tx) error {
		if err := guard(ctx, tx); err != nil {
			return err
		}
		for _, v := range []struct {
			id                        domain.ID
			provider, slot, reference string
			intent                    any
		}{{receipt.BranchOperationID, ghpublish.BranchProviderName, "candidate-branch", receipt.BranchReference, branch}, {receipt.PROperationID, ghpublish.PRProviderName, "candidate-pr", receipt.PRReference, pr}} {
			var state domain.OperationState
			var taskID domain.ID
			var provider, slot, reference, canonical string
			if err := tx.QueryRowContext(ctx, `SELECT o.state,o.task_id,o.provider,s.trusted_slot_key,COALESCE(o.provider_reference,''),s.canonical_intent_json FROM external_operations o JOIN effect_slots s ON s.effect_slot_id=o.effect_slot_id WHERE o.operation_id=?`, v.id).Scan(&state, &taskID, &provider, &slot, &reference, &canonical); err != nil {
				return err
			}
			want, _ := json.Marshal(v.intent)
			decoder := json.NewDecoder(bytes.NewReader(want))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			want, _ = json.Marshal(value)
			if state != domain.OperationConfirmedEffect || taskID != task.ID || provider != v.provider || slot != v.slot || reference != v.reference || canonical != string(want) {
				return errors.New("receipt protected operations differ")
			}
		}
		return nil
	}
	if task.State == domain.TaskAwaitingVerification || task.State == domain.TaskSucceeded {
		ids, err := p.verify.CompletionEvidence(ctx, task.ID, task.CurrentAttemptID)
		if err != nil || len(ids) != 1 {
			return PublicationResult{}, errors.New("publication requires one completion receipt")
		}
		o, b, err := p.evidence.Get(ctx, ids[0])
		if err != nil {
			return PublicationResult{}, err
		}
		if err = validate(o, b); err != nil {
			return PublicationResult{}, err
		} // CreateTaskWithGuard above checked current authority. Validate operation bindings transactionally on replay.
		_, err = p.exec.CreateTaskWithGuard(ctx, execution.TaskRequest{Purpose: implTask.Purpose, TaskClass: KindPublication, Objective: task.Objective, PayloadJSON: payload, IdempotencyKey: task.IdempotencyKey, AcceptanceCriteria: task.AcceptanceCriteria, RequiredCapabilities: publicationCapabilities, AuthorityCeiling: publicationCapabilities, RequiredEnforcement: domain.EnforcementEnforced, ResourceEnvelopeID: implTask.ResourceEnvelopeID}, receiptGuard)
		if err != nil {
			return PublicationResult{}, err
		}
		return PublicationResult{Reused: true, EvidenceID: o.ID, Record: &receipt}, nil
	}
	staged := false
	var stagedRaw []byte
	if task.CurrentAttemptID != "" {
		receiptObject, stagedRaw, staged, err = p.evidence.FindBySubject(ctx, KindPublication, string(task.CurrentAttemptID))
		if err != nil {
			return PublicationResult{}, err
		}
	}
	if staged {
		if err = validate(receiptObject, stagedRaw); err != nil {
			return PublicationResult{}, err
		}
		if task.State != domain.TaskExecuting {
			return PublicationResult{}, errors.New("staged publication requires operator recovery for nonexecuting attempt")
		}
	} else {
		attempt := domain.Attempt{ID: task.CurrentAttemptID, FenceGeneration: task.CurrentFence}
		if task.State == domain.TaskExecuting {
			resumable := false
			resumeGuard := func(ctx context.Context, tx *sql.Tx) error {
				if err := guard(ctx, tx); err != nil {
					return err
				}
				if _, err := p.exec.GuardAttempt(ctx, tx, task.CurrentAttemptID, domain.TaskExecuting); err != nil {
					return err
				}
				return tx.QueryRowContext(ctx, `SELECT EXISTS (
                  SELECT 1 FROM external_operations o JOIN effect_slots s ON s.effect_slot_id=o.effect_slot_id JOIN approval_requests a ON a.approval_id=o.approval_id
                  WHERE o.task_id=? AND o.attempt_id=? AND o.state='PREPARED' AND a.state IN ('PENDING','APPROVED')
                    AND ((s.trusted_slot_key='candidate-branch' AND o.provider=?) OR (s.trusted_slot_key='candidate-pr' AND o.provider=?))
                    AND NOT EXISTS (SELECT 1 FROM external_operations x WHERE x.task_id=o.task_id AND x.state IN ('DISPATCHED','OUTCOME_UNKNOWN'))
                )`, task.ID, task.CurrentAttemptID, ghpublish.BranchProviderName, ghpublish.PRProviderName).Scan(&resumable)
			}
			if _, err = p.exec.CreateTaskWithGuard(ctx, request, resumeGuard); err != nil {
				return PublicationResult{}, err
			}
			if !resumable {
				return PublicationResult{Busy: true}, nil
			}
		} else {
			if task.State != domain.TaskEligible {
				return PublicationResult{}, fmt.Errorf("publication task %s requires operator recovery", task.State)
			}
			attempt, err = p.exec.StartAttempt(ctx, task.ID, "github-candidate-publisher", PublicationLeaseDuration)
			if err != nil {
				current, e := p.exec.Task(ctx, task.ID)
				if e == nil && current.State != domain.TaskEligible {
					return PublicationResult{Busy: true}, nil
				}
				return PublicationResult{}, err
			}
			task.CurrentAttemptID = attempt.ID
			task.CurrentFence = attempt.FenceGeneration
		}

		receipt = expected
		receipt.TaskID = task.ID
		receipt.AttemptID = attempt.ID
		receipt.Fence = attempt.FenceGeneration
		for n, v := range []struct {
			provider, slot string
			intent         operations.IntentDescriptor
		}{{ghpublish.BranchProviderName, "candidate-branch", branch}, {ghpublish.PRProviderName, "candidate-pr", pr}} {
			if err = observe(); err != nil {
				return PublicationResult{}, err
			}
			op, err := p.operations.PrepareWithGuard(ctx, operations.PrepareRequest{AttemptID: attempt.ID, Provider: v.provider, TrustedSlotKey: v.slot, Intent: v.intent, Risk: "LOW", Attributes: map[string]any{"repository": input.Plan.Repository, "issue": input.Snapshot.Issue, "candidate": impl.Record.CandidateSHA}}, guard)
			if err != nil {
				return PublicationResult{}, err
			}
			if err = observe(); err != nil {
				return PublicationResult{}, err
			}
			op, err = p.operations.DispatchWithGuard(ctx, op.ID, attempt.ID, guard)
			if err != nil && op.State == domain.OperationPrepared && op.ApprovalID != "" {
				return PublicationResult{Held: true, ApprovalID: op.ApprovalID, OperationID: op.ID}, nil
			}
			if op.State == domain.OperationOutcomeUnknown || op.State == domain.OperationDispatched {
				return PublicationResult{Pending: true}, nil
			}
			if err != nil {
				return PublicationResult{}, err
			}
			if op.State != domain.OperationConfirmedEffect {
				return PublicationResult{}, errors.New("publication effect not confirmed")
			}
			if n == 0 {
				receipt.BranchOperationID = op.ID
				receipt.BranchReference = op.ProviderReference
			} else {
				receipt.PROperationID = op.ID
				receipt.PRReference = op.ProviderReference
			}
		}
		raw, _ := json.Marshal(receipt)
		receiptObject, err = p.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{Kind: KindPublication, Subject: string(attempt.ID), MediaType: "application/json"})
		if err != nil {
			return PublicationResult{}, err
		}
	}
	if err = observe(); err != nil {
		return PublicationResult{}, err
	}
	if _, err = p.verify.CompleteAttemptWithGuard(ctx, task.CurrentAttemptID, verification.CompletionManifest{EvidenceIDs: []domain.ID{receiptObject.ID}}, receiptGuard); err != nil {
		return PublicationResult{}, fmt.Errorf("staged receipt retained; live fence or operator recovery required: %w", err)
	}
	return PublicationResult{Reused: reused, EvidenceID: receiptObject.ID, Record: &receipt}, nil
}
