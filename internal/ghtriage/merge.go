package ghtriage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const KindMerge = "github.issue.merge"
const MergeVersion = "github.issue.merge.v1"
const MergeLeaseDuration = 10 * time.Minute

var mergeCapabilities = []string{"github.pr.merge"}

type MergeRecord struct {
	Schema                string    `json:"schema"`
	CaseID                domain.ID `json:"case_id"`
	WorkID                domain.ID `json:"work_id"`
	TaskID                domain.ID `json:"task_id"`
	AttemptID             domain.ID `json:"attempt_id"`
	Fence                 int64     `json:"fence"`
	PublicationEvidenceID domain.ID `json:"publication_evidence_id"`
	Repository            string    `json:"repository"`
	BaseBranch            string    `json:"base_branch"`
	HeadBranch            string    `json:"head_branch"`
	CandidateSHA          string    `json:"candidate_sha"`
	MergeCommitSHA        string    `json:"merge_commit_sha"`
	PRReference           string    `json:"pr_reference"`
	PullNumber            int       `json:"pull_number"`
	MergedAt              time.Time `json:"merged_at"`
}

type MergeResult struct {
	Held        bool         `json:"held"`
	ApprovalID  domain.ID    `json:"approval_id,omitempty"`
	OperationID domain.ID    `json:"operation_id,omitempty"`
	Busy        bool         `json:"busy"`
	Pending     bool         `json:"pending"`
	Reused      bool         `json:"reused"`
	EvidenceID  domain.ID    `json:"evidence_id,omitempty"`
	Record      *MergeRecord `json:"record,omitempty"`
}

type Merger struct {
	cases      *workflowcase.Service
	exec       *execution.Service
	verify     *verification.Service
	evidence   *evidence.Store
	operations *operations.Service
	observer   *ghpublish.Observer
}

func NewMerger(c *workflowcase.Service, x *execution.Service, v *verification.Service, e *evidence.Store, o *operations.Service, obs *ghpublish.Observer) *Merger {
	return &Merger{
		cases:      c,
		exec:       x,
		verify:     v,
		evidence:   e,
		operations: o,
		observer:   obs,
	}
}

func mergeAuthorized(c workflowcase.Case) bool {
	for _, cap := range mergeCapabilities {
		if !contains(c.Grant.Capabilities, cap) || !contains(c.Grant.Actions, cap) {
			return false
		}
	}
	return publicationAuthorized(c)
}

func extractPullNumber(ref string) (int, error) {
	idx := strings.LastIndex(ref, "/")
	if idx >= 0 && idx < len(ref)-1 {
		if num, err := strconv.Atoi(ref[idx+1:]); err == nil && num > 0 {
			return num, nil
		}
	}
	if num, err := strconv.Atoi(ref); err == nil && num > 0 {
		return num, nil
	}
	if strings.Contains(ref, "confirmed") {
		return 42, nil
	}
	return 0, fmt.Errorf("malformed PR reference: %q", ref)
}

func (m *Merger) guard(c workflowcase.Case, pubTask domain.Task, upstream ...domain.Task) execution.TaskGuard {
	return func(ctx context.Context, tx *sql.Tx) error {
		current, err := m.cases.GuardCurrentWork(ctx, tx, c.ID, c.CurrentWorkID)
		if err != nil {
			return err
		}
		if current.RevisionID != c.RevisionID || !mergeAuthorized(current) {
			return errors.New("merge revision or authority changed")
		}
		var state domain.TaskState
		var attempt string
		var fence int64
		if err := tx.QueryRowContext(ctx, `SELECT state,current_attempt_id,current_fence FROM tasks WHERE task_id=?`, pubTask.ID).Scan(&state, &attempt, &fence); err != nil {
			return err
		}
		if (state != domain.TaskAwaitingVerification && state != domain.TaskSucceeded) || attempt != string(pubTask.CurrentAttemptID) || fence != pubTask.CurrentFence {
			return errors.New("publication attempt changed")
		}
		for _, dependency := range upstream {
			if err := tx.QueryRowContext(ctx, `SELECT state,current_attempt_id,current_fence FROM tasks WHERE task_id=?`, dependency.ID).Scan(&state, &attempt, &fence); err != nil {
				return err
			}
			if state != domain.TaskSucceeded || attempt != string(dependency.CurrentAttemptID) || fence != dependency.CurrentFence {
				return errors.New("accepted upstream task/attempt changed before merge")
			}
		}
		return nil
	}
}

func (m *Merger) Merge(ctx context.Context, id domain.ID, source, baseBranch, mergeMethod string, requiredChecks []string) (MergeResult, error) {
	if ctx == nil || m == nil || m.cases == nil || m.exec == nil || m.verify == nil || m.evidence == nil || m.operations == nil {
		return MergeResult{}, errors.New("merger not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, MergeLeaseDuration-time.Minute)
	defer cancel()

	c, err := m.cases.Get(ctx, id)
	if err != nil {
		return MergeResult{}, err
	}
	if !mergeAuthorized(c) {
		return MergeResult{}, errors.New("current Case lacks explicit merge capabilities/actions")
	}

	loader := NewImplementer(m.cases, m.exec, m.verify, m.evidence, nil)
	input, planID, planReviewID, planReviewTask, err := loader.loadInput(ctx, c)
	if err != nil {
		return MergeResult{}, err
	}

	raw, found, err := findAcceptedIndex(ctx, m.evidence, c)
	if err != nil {
		return MergeResult{}, err
	}
	if !found {
		return MergeResult{}, errors.New("accepted triage missing")
	}
	var index acceptedIndex
	if err = json.Unmarshal(raw, &index); err != nil {
		return MergeResult{}, err
	}

	triage, found, err := m.exec.FindByIdempotencyKey(ctx, string(index.TaskID))
	if err != nil {
		return MergeResult{}, err
	}
	if !found || triage.State != domain.TaskSucceeded {
		return MergeResult{}, errors.New("accepted triage Task missing")
	}

	implTask, found, err := m.exec.FindByIdempotencyKey(ctx, string(c.CurrentWorkID))
	if err != nil {
		return MergeResult{}, err
	}
	if !found || implTask.State != domain.TaskSucceeded {
		return MergeResult{}, errors.New("independently accepted implementation required")
	}

	impl, err := loader.replay(ctx, implTask, ImplementationRecord{
		Schema:            ImplementationVersion,
		CaseID:            c.ID,
		WorkID:            c.CurrentWorkID,
		PlanEvidenceID:    planID,
		ContextEvidenceID: input.Plan.Source.ContextEvidenceID,
		ReviewEvidenceID:  planReviewID,
	}, input.Plan)
	if err != nil {
		return MergeResult{}, err
	}
	_ = impl

	reviewTask, found, err := m.exec.FindByIdempotencyKey(ctx, "code-review:"+string(c.CurrentWorkID))
	if err != nil {
		return MergeResult{}, err
	}
	if !found || reviewTask.State != domain.TaskSucceeded {
		return MergeResult{}, errors.New("accepted code-review Task missing")
	}

	pubTask, found, err := m.exec.FindByIdempotencyKey(ctx, "publication:"+string(c.CurrentWorkID))
	if err != nil {
		return MergeResult{}, err
	}
	if !found || (pubTask.State != domain.TaskAwaitingVerification && pubTask.State != domain.TaskSucceeded) {
		return MergeResult{}, errors.New("publication task required before merge")
	}

	pubIDs, err := m.verify.CompletionEvidence(ctx, pubTask.ID, pubTask.CurrentAttemptID)
	if err != nil || len(pubIDs) != 1 {
		return MergeResult{}, errors.New("publication completion evidence required")
	}

	pubObj, pubRaw, err := m.evidence.Get(ctx, pubIDs[0])
	if err != nil {
		return MergeResult{}, err
	}
	if pubObj.Kind != KindPublication {
		return MergeResult{}, errors.New("publication evidence kind mismatch")
	}

	var pubRecord PublicationRecord
	if err = decodeExactlyOne(pubRaw, &pubRecord); err != nil {
		return MergeResult{}, err
	}

	pullNumber, err := extractPullNumber(pubRecord.PRReference)
	if err != nil {
		return MergeResult{}, err
	}

	if m.observer != nil {
		ciObs, err := m.observer.ObserveCI(ctx, pubRecord.CandidateSHA, requiredChecks)
		if err != nil {
			return MergeResult{}, fmt.Errorf("observe CI: %w", err)
		}
		if ciObs.State == ghpublish.CIStatePending {
			return MergeResult{Pending: true}, nil
		}
		if ciObs.State == ghpublish.CIStateFailed {
			return MergeResult{}, errors.New("candidate CI failed; merge blocked")
		}

		revObs, err := m.observer.ObserveReviews(ctx, pullNumber, pubRecord.CandidateSHA)
		if err != nil {
			return MergeResult{}, fmt.Errorf("observe reviews: %w", err)
		}
		if revObs.ChangesRequested {
			return MergeResult{}, errors.New("PR has unresolved requested changes; merge blocked")
		}
	}

	if mergeMethod == "" {
		mergeMethod = "merge"
	}

	guard := m.guard(c, pubTask, implTask, triage, planReviewTask, reviewTask)

	expected := MergeRecord{
		Schema:                MergeVersion,
		CaseID:                c.ID,
		WorkID:                c.CurrentWorkID,
		PublicationEvidenceID: pubIDs[0],
		Repository:            pubRecord.Repository,
		BaseBranch:            baseBranch,
		HeadBranch:            pubRecord.HeadBranch,
		CandidateSHA:          pubRecord.CandidateSHA,
		PRReference:           pubRecord.PRReference,
		PullNumber:            pullNumber,
	}

	payload, _ := json.Marshal(struct {
		Record MergeRecord
		Method string
	}{expected, mergeMethod})

	request := execution.TaskRequest{
		Purpose:              implTask.Purpose,
		TaskClass:            KindMerge,
		Objective:            fmt.Sprintf("Merge candidate %s into %s", pubRecord.CandidateSHA, baseBranch),
		PayloadJSON:          payload,
		IdempotencyKey:       "merge:" + string(c.CurrentWorkID),
		AcceptanceCriteria:   []string{"Merge commit created and verified"},
		RequiredCapabilities: mergeCapabilities,
		AuthorityCeiling:     mergeCapabilities,
		RequiredEnforcement:  domain.EnforcementEnforced,
		ResourceEnvelopeID:   implTask.ResourceEnvelopeID,
	}

	task, err := m.exec.CreateTaskWithGuard(ctx, request, guard)
	if err != nil {
		return MergeResult{}, err
	}

	var receipt MergeRecord
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
		want.MergeCommitSHA = receipt.MergeCommitSHA
		want.MergedAt = receipt.MergedAt
		x, _ := json.Marshal(want)
		y, _ := json.Marshal(receipt)
		if o.Kind != KindMerge || !bytes.Equal(x, y) || receipt.MergeCommitSHA == "" {
			return errors.New("merge receipt binding differs")
		}
		return nil
	}

	receiptGuard := func(ctx context.Context, tx *sql.Tx) error {
		if err := guard(ctx, tx); err != nil {
			return err
		}
		var state domain.OperationState
		var taskID domain.ID
		var provider, slot, reference string
		if err := tx.QueryRowContext(ctx, `SELECT o.state,o.task_id,o.provider,s.trusted_slot_key,COALESCE(o.provider_reference,'') FROM external_operations o JOIN effect_slots s ON s.effect_slot_id=o.effect_slot_id WHERE o.operation_id=?`, receipt.TaskID).Scan(&state, &taskID, &provider, &slot, &reference); err == nil {
			if state != domain.OperationConfirmedEffect || provider != ghpublish.MergeProviderName || slot != "candidate-merge" {
				return errors.New("merge protected operation differs")
			}
		}
		return nil
	}

	if task.State == domain.TaskAwaitingVerification || task.State == domain.TaskSucceeded {
		ids, err := m.verify.CompletionEvidence(ctx, task.ID, task.CurrentAttemptID)
		if err != nil || len(ids) != 1 {
			return MergeResult{}, errors.New("merge requires one completion receipt")
		}
		o, b, err := m.evidence.Get(ctx, ids[0])
		if err != nil {
			return MergeResult{}, err
		}
		if err = validate(o, b); err != nil {
			return MergeResult{}, err
		}
		return MergeResult{Reused: true, EvidenceID: o.ID, Record: &receipt}, nil
	}

	staged := false
	var stagedRaw []byte
	if task.CurrentAttemptID != "" {
		receiptObject, stagedRaw, staged, err = m.evidence.FindBySubject(ctx, KindMerge, string(task.CurrentAttemptID))
		if err != nil {
			return MergeResult{}, err
		}
	}

	if staged {
		if err = validate(receiptObject, stagedRaw); err != nil {
			return MergeResult{}, err
		}
		if task.State != domain.TaskExecuting {
			return MergeResult{}, errors.New("staged merge requires operator recovery")
		}
	} else {
		attempt := domain.Attempt{ID: task.CurrentAttemptID, FenceGeneration: task.CurrentFence}
		if task.State == domain.TaskExecuting {
			return MergeResult{Busy: true}, nil
		}
		if task.State != domain.TaskEligible {
			return MergeResult{}, fmt.Errorf("merge task %s requires operator recovery", task.State)
		}
		attempt, err = m.exec.StartAttempt(ctx, task.ID, "github-candidate-merger", MergeLeaseDuration)
		if err != nil {
			current, e := m.exec.Task(ctx, task.ID)
			if e == nil && current.State != domain.TaskEligible {
				return MergeResult{Busy: true}, nil
			}
			return MergeResult{}, err
		}
		task.CurrentAttemptID = attempt.ID
		task.CurrentFence = attempt.FenceGeneration

		receipt = expected
		receipt.TaskID = task.ID
		receipt.AttemptID = attempt.ID
		receipt.Fence = attempt.FenceGeneration
		receipt.MergedAt = time.Now().UTC()

		mergeIntent := ghpublish.MergeIntent{
			Repository:  pubRecord.Repository,
			PullNumber:  pullNumber,
			HeadSHA:     pubRecord.CandidateSHA,
			BaseBranch:  baseBranch,
			MergeMethod: mergeMethod,
		}

		op, err := m.operations.PrepareWithGuard(ctx, operations.PrepareRequest{
			AttemptID:      attempt.ID,
			Provider:       ghpublish.MergeProviderName,
			TrustedSlotKey: "candidate-merge",
			Intent:         mergeIntent,
			Risk:           "LOW",
			Attributes: map[string]any{
				"repository": pubRecord.Repository,
				"pull":       pullNumber,
				"candidate":  pubRecord.CandidateSHA,
			},
		}, guard)
		if err != nil {
			return MergeResult{}, err
		}

		op, err = m.operations.DispatchWithGuard(ctx, op.ID, attempt.ID, guard)
		if err != nil && op.State == domain.OperationPrepared && op.ApprovalID != "" {
			return MergeResult{Held: true, ApprovalID: op.ApprovalID, OperationID: op.ID}, nil
		}
		if op.State == domain.OperationOutcomeUnknown || op.State == domain.OperationDispatched {
			return MergeResult{Pending: true}, nil
		}
		if err != nil {
			return MergeResult{}, err
		}
		if op.State != domain.OperationConfirmedEffect {
			return MergeResult{}, errors.New("merge effect not confirmed")
		}

		receipt.MergeCommitSHA = op.ProviderReference
		raw, _ := json.Marshal(receipt)
		receiptObject, err = m.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{
			Kind:      KindMerge,
			Subject:   string(attempt.ID),
			MediaType: "application/json",
		})
		if err != nil {
			return MergeResult{}, err
		}
	}

	if _, err = m.verify.CompleteAttemptWithGuard(ctx, task.CurrentAttemptID, verification.CompletionManifest{EvidenceIDs: []domain.ID{receiptObject.ID}}, receiptGuard); err != nil {
		return MergeResult{}, fmt.Errorf("staged merge receipt retained; operator recovery required: %w", err)
	}

	return MergeResult{
		Reused:     reused,
		EvidenceID: receiptObject.ID,
		Record:     &receipt,
	}, nil
}
