package ghtriage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const ImplementationVersion = "github.issue.implementation.v1"
const KindImplementation = "github.issue.implementation"
const ImplementationLeaseDuration = 15 * time.Minute

type ImplementationInput struct {
	Schema   string                 `json:"schema"`
	Question string                 `json:"question"`
	Plan     Plan                   `json:"plan"`
	Snapshot Snapshot               `json:"snapshot"`
	Context  repoworkspace.Snapshot `json:"context"`
}
type ImplementationOutput struct {
	Summary string               `json:"summary"`
	Edits   []repoworkspace.Edit `json:"edits"`
}

func (o ImplementationOutput) Validate() error {
	if err := textLength(o.Summary, 1, 1000); err != nil {
		return err
	}
	paths := make([]string, len(o.Edits))
	for n, e := range o.Edits {
		paths[n] = e.Path
	}
	return repoworkspace.ValidateEdits(paths, o.Edits)
}
func ParseImplementationOutput(raw []byte) (ImplementationOutput, error) {
	var out ImplementationOutput
	if err := decodeExactlyOne(raw, &out); err != nil {
		return out, err
	}
	return out, out.Validate()
}

type ImplementationModel interface {
	Implement(context.Context, ImplementationInput) (ImplementationOutput, error)
}
type ImplementationRecord struct {
	SourceStateHash   string    `json:"source_state_hash"`
	Schema            string    `json:"schema"`
	CaseID            domain.ID `json:"case_id"`
	WorkID            domain.ID `json:"work_id"`
	TaskID            domain.ID `json:"task_id"`
	AttemptID         domain.ID `json:"attempt_id"`
	Fence             int64     `json:"fence"`
	PlanEvidenceID    domain.ID `json:"plan_evidence_id"`
	ContextEvidenceID domain.ID `json:"context_evidence_id"`
	ReviewEvidenceID  domain.ID `json:"review_evidence_id"`
	Workspace         string    `json:"workspace"`
	Summary           string    `json:"summary"`
	repoworkspace.Candidate
	Commands  []repoworkspace.CommandResult `json:"commands"`
	AllPassed bool                          `json:"all_passed"`
}
type ImplementationResult struct {
	Busy       bool                  `json:"busy"`
	Reused     bool                  `json:"reused"`
	EvidenceID domain.ID             `json:"evidence_id,omitempty"`
	Record     *ImplementationRecord `json:"record,omitempty"`
}
type Implementer struct {
	cases    *workflowcase.Service
	exec     *execution.Service
	verify   *verification.Service
	evidence *evidence.Store
	model    ImplementationModel
}

func NewImplementer(cases *workflowcase.Service, exec *execution.Service, verify *verification.Service, store *evidence.Store, model ImplementationModel) *Implementer {
	return &Implementer{cases, exec, verify, store, model}
}

func (i *Implementer) Prepare(ctx context.Context, id domain.ID, sourceRepo, root string) (ImplementationResult, error) {
	if ctx == nil || i == nil || i.cases == nil || i.exec == nil || i.verify == nil || i.evidence == nil || i.model == nil {
		return ImplementationResult{}, errors.New("implementation services and context required")
	}
	if id == "" || !filepath.IsAbs(sourceRepo) || !filepath.IsAbs(root) {
		return ImplementationResult{}, errors.New("case and absolute source/workspace root required")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return ImplementationResult{}, errors.New("workspace root must be an existing regular directory")
	}
	rel, err := filepath.Rel(sourceRepo, root)
	if err != nil || rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return ImplementationResult{}, errors.New("workspace root must be outside source repository")
	}
	c, err := i.cases.Get(ctx, id)
	if err != nil {
		return ImplementationResult{}, err
	}
	input, planID, reviewID, reviewTask, err := i.loadInput(ctx, c)
	if err != nil {
		return ImplementationResult{}, err
	}
	paths := make([]string, len(input.Context.Files))
	for n, f := range input.Context.Files {
		paths[n] = f.Path
	}
	cfg := repoworkspace.Config{LocalPath: sourceRepo, Repository: input.Plan.Repository, Commit: input.Plan.Source.BaseSHA, Paths: paths}
	source, err := repoworkspace.Capture(ctx, cfg)
	if err != nil {
		return ImplementationResult{}, err
	}
	raw, _ := source.Canonical()
	expectedRaw, _ := input.Context.Canonical()
	if !bytes.Equal(raw, expectedRaw) {
		return ImplementationResult{}, errors.New("local repository differs from cited source context")
	}
	sourceState, err := repoworkspace.SourceState(ctx, sourceRepo)
	if err != nil {
		return ImplementationResult{}, err
	}
	expected := ImplementationRecord{Schema: ImplementationVersion, CaseID: c.ID, WorkID: c.CurrentWorkID, PlanEvidenceID: planID, ContextEvidenceID: input.Plan.Source.ContextEvidenceID, ReviewEvidenceID: reviewID}
	payload, _ := json.Marshal(expected)
	task, err := i.cases.MaterializeTask(ctx, i.exec, c.ID, c.CurrentWorkID, execution.TaskRequest{Objective: "Implement accepted issue plan " + string(planID), PayloadJSON: payload, AcceptanceCriteria: []string{"Exact candidate, real validation and independent code review before acceptance"}, RequiredEnforcement: domain.EnforcementUnenforced, ResourceEnvelopeID: reviewTask.ResourceEnvelopeID})
	if err != nil {
		return ImplementationResult{}, err
	}
	if task.State == domain.TaskAwaitingVerification || task.State == domain.TaskSucceeded {
		return i.replay(ctx, task, expected, input.Plan)
	}
	if task.CurrentAttemptID != "" {
		recovered, found, err := i.resumeStaged(ctx, task, expected, input.Plan, c, sourceState)
		if err != nil || found {
			return recovered, err
		}
	}
	if task.State == domain.TaskExecuting {
		return ImplementationResult{Busy: true}, nil
	}
	if task.State != domain.TaskEligible {
		return ImplementationResult{}, fmt.Errorf("implementation task %s requires operator attention", task.State)
	}
	attempt, err := i.exec.StartAttempt(ctx, task.ID, "github-issue-structured-implementer", ImplementationLeaseDuration)
	if err != nil {
		current, getErr := i.exec.Task(ctx, task.ID)
		if getErr == nil && current.State != domain.TaskEligible {
			return ImplementationResult{Busy: true}, nil
		}
		return ImplementationResult{}, err
	}
	fail := func(err error) (ImplementationResult, error) {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := i.exec.FailAttempt(cleanup, attempt.ID, domain.FailureExecution, "implementation:"+string(c.CurrentWorkID), nil)
		return ImplementationResult{}, errors.Join(err, cleanupErr)
	}
	runCtx, cancel := context.WithTimeout(ctx, 14*time.Minute)
	defer cancel()
	destination := filepath.Join(root, string(attempt.ID))
	if _, err := repoworkspace.Checkout(runCtx, cfg, destination); err != nil {
		return fail(err)
	}
	// Freeze a private model copy so a provider cannot mutate canonical scope or argv.
	inputRaw, _ := json.Marshal(input)
	var modelInput ImplementationInput
	if err := json.Unmarshal(inputRaw, &modelInput); err != nil {
		return fail(err)
	}
	modelCtx, modelCancel := context.WithTimeout(runCtx, 2*time.Minute)
	output, modelErr := i.model.Implement(modelCtx, modelInput)
	modelCancel()
	if modelErr != nil {
		return fail(modelErr)
	}
	if err := output.Validate(); err != nil {
		return fail(err)
	}
	if _, _, _, _, err := i.loadInput(runCtx, c); err != nil {
		return fail(err)
	}
	candidate, err := repoworkspace.PrepareCandidate(runCtx, destination, cfg.Commit, input.Plan.Source.AllowedPaths, output.Edits)
	if err != nil {
		return fail(err)
	}
	commands, err := repoworkspace.ValidateCandidate(runCtx, destination, candidate, input.Plan.Source.ValidationCommands)
	if err != nil {
		return fail(err)
	}
	finalSourceState, err := repoworkspace.SourceState(runCtx, sourceRepo)
	if err != nil {
		return fail(err)
	}
	if finalSourceState != sourceState {
		return fail(errors.New("source repository changed during implementation or validation"))
	}
	expected.SourceStateHash = sourceState
	expected.TaskID = task.ID
	expected.AttemptID = attempt.ID
	expected.Fence = attempt.FenceGeneration
	expected.Workspace = destination
	expected.Summary = output.Summary
	expected.Candidate = candidate
	expected.Commands = commands
	expected.AllPassed = true
	for _, command := range commands {
		expected.AllPassed = expected.AllPassed && command.Passed
	}
	recordRaw, err := json.Marshal(expected)
	if err != nil {
		return fail(err)
	}
	object, err := i.evidence.Put(runCtx, bytes.NewReader(recordRaw), evidence.Metadata{Kind: KindImplementation, Subject: string(attempt.ID), MediaType: "application/json"})
	if err != nil {
		return ImplementationResult{}, err
	}
	// Execution is finished: persistence/completion errors must not requeue paid work.
	if err := i.completeRecord(runCtx, c, attempt.ID, object.ID); err != nil {
		return ImplementationResult{}, err
	}

	return ImplementationResult{EvidenceID: object.ID, Record: &expected}, nil
}
func (i *Implementer) replay(ctx context.Context, task domain.Task, expected ImplementationRecord, plan Plan) (ImplementationResult, error) {
	ids, err := i.verify.CompletionEvidence(ctx, task.ID, task.CurrentAttemptID)
	if err != nil {
		return ImplementationResult{}, err
	}
	if len(ids) != 1 {
		return ImplementationResult{}, errors.New("implementation completion requires one record")
	}
	object, raw, err := i.evidence.Get(ctx, ids[0])
	if err != nil {
		return ImplementationResult{}, err
	}
	return validateImplementationRecord(object, raw, task, expected, plan)
}
func validateImplementationRecord(object evidence.EvidenceObject, raw []byte, task domain.Task, expected ImplementationRecord, plan Plan) (ImplementationResult, error) {
	var record ImplementationRecord
	if err := decodeExactlyOne(raw, &record); err != nil {
		return ImplementationResult{}, err
	}
	if object.Kind != KindImplementation || record.Schema != expected.Schema || record.CaseID != expected.CaseID || record.WorkID != expected.WorkID || record.PlanEvidenceID != expected.PlanEvidenceID || record.ContextEvidenceID != expected.ContextEvidenceID || record.ReviewEvidenceID != expected.ReviewEvidenceID || record.TaskID != task.ID || record.AttemptID != task.CurrentAttemptID || record.Fence != task.CurrentFence || record.BaseSHA != plan.Source.BaseSHA || len(record.SourceStateHash) != 64 || !repoworkspace.FullCommit(record.CandidateSHA) || !repoworkspace.FullCommit(record.TreeSHA) || len(record.DiffHash) != 64 || len(record.ChangedPaths) < 1 || len(record.Commands) != len(plan.Source.ValidationCommands) {
		return ImplementationResult{}, errors.New("implementation record binding differs from current work")
	}
	for _, path := range record.ChangedPaths {
		if !contains(plan.Source.AllowedPaths, path) {
			return ImplementationResult{}, errors.New("record exceeds owner scope")
		}
	}
	allPassed := true
	for n, result := range record.Commands {
		if !reflect.DeepEqual(result.Argv, plan.Source.ValidationCommands[n]) || result.Passed != (result.ExitCode == 0 && !result.TimedOut) || len(result.Output) > repoworkspace.MaxValidationOutput {
			return ImplementationResult{}, errors.New("validation record differs from command contract")
		}
		allPassed = allPassed && result.Passed
	}
	if allPassed != record.AllPassed {
		return ImplementationResult{}, errors.New("validation verdict inconsistent")
	}
	return ImplementationResult{Reused: true, EvidenceID: object.ID, Record: &record}, nil
}

func (i *Implementer) completeRecord(ctx context.Context, c workflowcase.Case, attemptID, evidenceID domain.ID) error {
	_, err := i.verify.CompleteAttemptWithGuard(ctx, attemptID, verification.CompletionManifest{EvidenceIDs: []domain.ID{evidenceID}}, func(ctx context.Context, tx *sql.Tx) error {
		current, err := i.cases.GuardCurrentWork(ctx, tx, c.ID, c.CurrentWorkID)
		if err != nil {
			return err
		}
		if current.RevisionID != c.RevisionID || !implementationAuthorized(current) {
			return errors.New("implementation authority or revision changed")
		}
		return nil
	})
	return err
}
func (i *Implementer) resumeStaged(ctx context.Context, task domain.Task, expected ImplementationRecord, plan Plan, c workflowcase.Case, sourceState string) (ImplementationResult, bool, error) {
	object, raw, found, err := i.evidence.FindBySubject(ctx, KindImplementation, string(task.CurrentAttemptID))
	if err != nil || !found {
		return ImplementationResult{}, found, err
	}
	result, err := validateImplementationRecord(object, raw, task, expected, plan)
	if err != nil {
		return ImplementationResult{}, true, err
	}
	if task.State != domain.TaskExecuting {
		return ImplementationResult{}, true, errors.New("staged implementation exists for a non-executing attempt; operator recovery required")
	}
	if result.Record.SourceStateHash != sourceState {
		return ImplementationResult{}, true, errors.New("source changed since staged execution; operator recovery required")
	}
	if err := repoworkspace.VerifyCandidate(ctx, result.Record.Workspace, result.Record.Candidate); err != nil {
		return ImplementationResult{}, true, err
	}
	if err := i.completeRecord(ctx, c, task.CurrentAttemptID, object.ID); err != nil {
		current, getErr := i.exec.Task(ctx, task.ID)
		if getErr == nil && (current.State == domain.TaskAwaitingVerification || current.State == domain.TaskSucceeded) {
			replay, replayErr := i.replay(ctx, current, expected, plan)
			return replay, true, replayErr
		}
		return ImplementationResult{}, true, fmt.Errorf("staged execution retained; completion requires a live current fence or operator recovery: %w", err)
	}
	return result, true, nil
}
