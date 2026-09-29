package ghissue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const readCapability = "github.issue.read"

// SnapshotKind is the evidence kind the canonical issue snapshot is stored
// under. It is exported because it is not only this package's spelling: the
// triage executor and the triage reviewer both read that snapshot back, and a
// private copy of the string in either of them is a second writer of a contract
// nothing binds.
const SnapshotKind = "github.issue.snapshot"

// ObserveConfig configures issue intake. Config in client.go is the transport
// configuration; this is the observer's.
type ObserveConfig struct {
	MissionID          domain.ID
	Repository         string
	Maintainers        []string
	Grant              workflow.Grant
	WorkCapabilities   []string
	ResourceEnvelopeID domain.ID
	MaxSteps           int
	RemainingBudget    int64
}

// normalized trims and deduplicates every string collection so validation,
// FirstWork.AuthorityCeiling and workflow.Decide all compare the same tokens.
func (c ObserveConfig) normalized() ObserveConfig {
	normalized := ObserveConfig{
		MissionID:          domain.ID(strings.TrimSpace(string(c.MissionID))),
		Repository:         strings.TrimSpace(c.Repository),
		Maintainers:        normalizeTokens(c.Maintainers),
		Grant:              workflow.Grant{Capabilities: normalizeTokens(c.Grant.Capabilities), Actions: normalizeTokens(c.Grant.Actions)},
		WorkCapabilities:   normalizeTokens(c.WorkCapabilities),
		ResourceEnvelopeID: domain.ID(strings.TrimSpace(string(c.ResourceEnvelopeID))),
		MaxSteps:           c.MaxSteps,
		RemainingBudget:    c.RemainingBudget,
	}
	return normalized
}

func (c ObserveConfig) validate() error {
	if strings.TrimSpace(string(c.MissionID)) == "" {
		return errors.New("mission is required")
	}
	if strings.TrimSpace(c.Repository) == "" {
		return errors.New("repository is required")
	}
	if len(c.Maintainers) == 0 {
		return errors.New("at least one maintainer login is required")
	}
	if strings.TrimSpace(string(c.ResourceEnvelopeID)) == "" {
		return errors.New("resource envelope is required")
	}
	if c.MaxSteps <= 0 || c.RemainingBudget <= 0 {
		return errors.New("positive max steps and remaining budget are required")
	}
	granted := make(map[string]struct{}, len(c.Grant.Capabilities))
	for _, capability := range c.Grant.Capabilities {
		if trimmed := strings.TrimSpace(capability); trimmed != "" {
			granted[trimmed] = struct{}{}
		}
	}
	if len(granted) == 0 {
		return errors.New("grant capabilities are required")
	}
	if _, ok := granted[readCapability]; !ok {
		return fmt.Errorf("grant must include %s", readCapability)
	}
	for _, capability := range c.effectiveCapabilities() {
		if _, ok := granted[capability]; !ok {
			return fmt.Errorf("work capability %q exceeds the configured grant", capability)
		}
	}
	return nil
}

// effectiveCapabilities always includes github.issue.read; extra work
// capabilities are additive only.
func (c ObserveConfig) effectiveCapabilities() []string {
	seen := make(map[string]struct{}, len(c.WorkCapabilities)+1)
	out := make([]string, 0, len(c.WorkCapabilities)+1)
	for _, capability := range append([]string{readCapability}, c.WorkCapabilities...) {
		trimmed := strings.TrimSpace(capability)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// FailedIssue records one issue that could not be completed durably this tick;
// it is retried on the next tick while the issue is still eligible.
type FailedIssue struct {
	Issue Issue
	Err   string
}

// PerIssueError classifies the failures of individual issues in an otherwise
// successful tick: the tick did its job, but these issues could not be
// registered durably. Run absorbs it on every tick, so an unretriable cause such
// as a policy denial can never stop intake for the other issues.
type PerIssueError struct {
	Err error
}

func (e *PerIssueError) Error() string { return e.Err.Error() }

func (e *PerIssueError) Unwrap() error { return e.Err }

// ObserveResult summarizes one tick.
type ObserveResult struct {
	Ensured             []domain.ID
	Materialized        []domain.ID
	Excluded            []ExcludedIssue
	Failed              []FailedIssue
	PullRequestsSkipped int
}

func ObserveOnce(ctx context.Context, lister IssueLister, cases *workflowcase.Service, execSvc *execution.Service, evidenceStore *evidence.Store, cfg ObserveConfig) (ObserveResult, error) {
	var result ObserveResult
	if ctx == nil {
		return result, errors.New("observer context is required")
	}
	if cases == nil || execSvc == nil || evidenceStore == nil {
		return result, errors.New("workflow case, execution and evidence services are required")
	}
	if lister == nil {
		return result, errors.New("issue lister is required")
	}
	cfg = cfg.normalized()
	if err := cfg.validate(); err != nil {
		return result, err
	}
	if name := strings.TrimSpace(lister.Name()); !strings.EqualFold(name, cfg.Repository) {
		return result, fmt.Errorf("issue lister repository %q does not match configured repository %q", name, cfg.Repository)
	}
	issues, unparseable, err := CollectIssues(ctx, lister)
	if err != nil {
		return result, err
	}
	for range unparseable {
		result.Excluded = append(result.Excluded, ExcludedIssue{Reason: ReasonUnparseable})
	}
	kept, excluded, skipped := FilterIssues(issues, cfg.Maintainers)
	result.PullRequestsSkipped = skipped
	result.Excluded = append(result.Excluded, excluded...)
	var failures []error
	for _, issue := range kept {
		if err := observeIssue(ctx, cases, execSvc, evidenceStore, cfg, issue, &result); err != nil {
			result.Failed = append(result.Failed, FailedIssue{Issue: issue, Err: err.Error()})
			failures = append(failures, err)
		}
	}
	if joined := errors.Join(failures...); joined != nil {
		return result, &PerIssueError{Err: joined}
	}
	return result, nil
}

func observeIssue(ctx context.Context, cases *workflowcase.Service, execSvc *execution.Service, evidenceStore *evidence.Store, cfg ObserveConfig, issue Issue, result *ObserveResult) error {
	existing, found, err := cases.Find(ctx, cfg.MissionID, "github", issue.ObjectID(), issue.RevisionID())
	if err != nil {
		return err
	}
	if found {
		if existing.State != workflowcase.Active {
			result.Excluded = append(result.Excluded, ExcludedIssue{Issue: issue, Reason: ReasonCaseNotActive})
			return nil
		}
		return materializeHit(ctx, cases, execSvc, cfg, issue, existing, result)
	}
	snapshot, err := CanonicalSnapshot(issue)
	if err != nil {
		return err
	}
	object, err := putSnapshot(ctx, evidenceStore, snapshot)
	if err != nil {
		return err
	}
	template, err := taskTemplate(cfg, issue, string(object.ID))
	if err != nil {
		return err
	}
	created, task, err := cases.EnsureAndMaterialize(ctx, execSvc, workflowcase.Observation{
		MissionID: cfg.MissionID, Source: "github", ObjectID: issue.ObjectID(), RevisionID: issue.RevisionID(),
		EvidenceID: string(object.ID),
		FirstWork: workflow.WorkProposal{
			Kind: "github.issue.triage", RequiredCapabilities: cfg.effectiveCapabilities(),
			AuthorityCeiling: append([]string(nil), cfg.Grant.Capabilities...),
		},
		Grant: cfg.Grant, MaxSteps: cfg.MaxSteps, RemainingBudget: cfg.RemainingBudget,
	}, template)
	if err != nil {
		result.Excluded = append(result.Excluded, ExcludedIssue{Issue: issue, Reason: ReasonEnsureFailed, Detail: err.Error()})
		return nil
	}
	result.Ensured = append(result.Ensured, created.ID)
	result.Materialized = append(result.Materialized, task.ID)
	return nil
}

// materializeHit rebuilds the Task of an already-registered case. The request
// hash covers the envelope, objective and payload, so those come from the
// already-materialized work keyed by the case's current work ID: deriving them
// from mutable config would fail the replay and brick a live case.
func materializeHit(ctx context.Context, cases *workflowcase.Service, execSvc *execution.Service, cfg ObserveConfig, issue Issue, existing workflowcase.Case, result *ObserveResult) error {
	template, err := taskTemplate(cfg, issue, existing.ObservationEvidenceID)
	if err != nil {
		return err
	}
	work, materialized, err := execSvc.FindByIdempotencyKey(ctx, string(existing.CurrentWorkID))
	if err != nil {
		return err
	}
	if materialized {
		template.Objective = work.Objective
		template.PayloadJSON = work.PayloadJSON
		template.ResourceEnvelopeID = work.ResourceEnvelopeID
	}
	task, err := cases.MaterializeTask(ctx, execSvc, existing.ID, existing.CurrentWorkID, template)
	if err != nil {
		current, stillThere, findErr := cases.Find(ctx, cfg.MissionID, "github", issue.ObjectID(), issue.RevisionID())
		if findErr != nil {
			return findErr
		}
		if !stillThere || current.State != workflowcase.Active {
			result.Excluded = append(result.Excluded, ExcludedIssue{Issue: issue, Reason: ReasonCaseNotActive})
			return nil
		}
		result.Failed = append(result.Failed, FailedIssue{Issue: issue, Err: err.Error()})
		return nil
	}
	result.Ensured = append(result.Ensured, existing.ID)
	result.Materialized = append(result.Materialized, task.ID)
	return nil
}

// putSnapshot reuses the evidence row that already holds these exact snapshot
// bytes: a rolled-back ensure would otherwise leave one orphan row per tick.
func putSnapshot(ctx context.Context, evidenceStore *evidence.Store, snapshot []byte) (evidence.EvidenceObject, error) {
	metadata := evidence.Metadata{MediaType: "application/json", Kind: SnapshotKind}
	digest := sha256.Sum256(snapshot)
	existing, found, err := evidenceStore.FindByContentHash(ctx, hex.EncodeToString(digest[:]), metadata.Kind)
	if err == nil && found {
		return existing, nil
	}
	return evidenceStore.Put(ctx, bytes.NewReader(snapshot), metadata)
}

// TriageTaskPayload is the payload of the github.issue.triage task intake
// materializes for one issue. It is the whole of the contract between the two
// writers of those bytes: intake writes the keys, the triage executor reads four
// of them, and nothing in either package can see the other's struct tags. So the
// builder is exported and the executor's decoder is tested against what it
// returns rather than against a hand-written map that agrees with it only
// because one person wrote both.
//
// The key set is pinned by TestTriageTaskPayloadKeys on this side, so a rename
// cannot land as a single commit here either.
func TriageTaskPayload(issue Issue, snapshotID string) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{
		"repo": issue.Repository, "issue": issue.Number, "revision": issue.RevisionID(),
		"title": issue.Title, "url": issue.URL, "author": issue.Author,
		"labels": nonNil(issue.Labels), "triage": issue.Triage, "issueSnapshot": snapshotID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode task payload: %w", err)
	}
	return payload, nil
}

func taskTemplate(cfg ObserveConfig, issue Issue, snapshotID string) (execution.TaskRequest, error) {
	payload, err := TriageTaskPayload(issue, snapshotID)
	if err != nil {
		return execution.TaskRequest{}, err
	}
	return execution.TaskRequest{
		Objective:          fmt.Sprintf("Triage GitHub issue %s#%d", issue.Repository, issue.Number),
		PayloadJSON:        payload,
		AcceptanceCriteria: []string{"triage decision recorded for " + issue.RevisionID()},
		ResourceEnvelopeID: cfg.ResourceEnvelopeID,
	}, nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// maxObserverBackoff caps the delay after a failing tick so a persistent
// outage neither hammers GitHub nor stops intake.
const maxObserverBackoff = time.Minute

// observeWait waits for the next poll; the observer seam tests drive.
var observeWait = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextBackoff(backoff, interval time.Duration) time.Duration {
	if backoff <= 0 {
		backoff = interval
	}
	backoff *= 2
	if limit := max(maxObserverBackoff, interval); backoff > limit {
		return limit
	}
	return backoff
}

// tickError classifies one tick. The hit path records its materialize failure
// in the Failed bucket without returning an error, so a tick can fail per issue
// either way; both are PerIssueError. An error that is not per-issue means the
// tick could not do its job at all, and stays fatal on the first tick.
func tickError(result ObserveResult, err error) error {
	if err != nil {
		return err
	}
	if len(result.Failed) == 0 {
		return nil
	}
	failures := make([]error, 0, len(result.Failed))
	for _, failed := range result.Failed {
		failures = append(failures, errors.New(failed.Err))
	}
	return &PerIssueError{Err: errors.Join(failures...)}
}

// perIssueError reports whether a tick failed only per issue. Those are
// reported and backed off on every tick, while a tick-level error is fatal on
// the first tick: collect, transport and config failures mean the setup is
// wrong, and nothing later would succeed either.
func perIssueError(err error) bool {
	var perIssue *PerIssueError
	return errors.As(err, &perIssue)
}

// observeTick runs one tick and hands the result and error to report, which
// may be nil. Reporting happens before classification so a caller sees every
// tick, including the one that ends the loop.
func observeTick(ctx context.Context, lister IssueLister, cases *workflowcase.Service, execSvc *execution.Service, evidenceStore *evidence.Store, cfg ObserveConfig, report func(ObserveResult, error)) error {
	result, err := ObserveOnce(ctx, lister, cases, execSvc, evidenceStore, cfg)
	if report != nil {
		report(result, err)
	}
	return tickError(result, err)
}

// RunReporting polls GitHub issues and hands every tick to report, which may
// be nil. A caller that surfaces the report is the only way an operator can
// tell an idle tick from one whose durable writes keep failing. A tick that
// failed only per issue is backed off and retried on every tick; a tick-level
// error is fatal on the first tick, because a collect, transport or config
// failure there is a setup error that no later tick would survive.
func RunReporting(ctx context.Context, lister IssueLister, cases *workflowcase.Service, execSvc *execution.Service, evidenceStore *evidence.Store, cfg ObserveConfig, interval time.Duration, report func(ObserveResult, error)) error {
	if ctx == nil {
		return errors.New("observer context is required")
	}
	if interval <= 0 {
		return errors.New("observer requires a positive poll interval")
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	var backoff time.Duration
	if err := observeTick(ctx, lister, cases, execSvc, evidenceStore, cfg, report); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if !perIssueError(err) {
			return err
		}
		backoff = nextBackoff(backoff, interval)
	}
	for {
		wait := interval
		if backoff > 0 {
			wait = backoff
		}
		if err := observeWait(ctx, wait); err != nil {
			return nil
		}
		if err := observeTick(ctx, lister, cases, execSvc, evidenceStore, cfg, report); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			backoff = nextBackoff(backoff, interval)
			continue
		}
		backoff = 0
	}
}

func Run(ctx context.Context, lister IssueLister, cases *workflowcase.Service, execSvc *execution.Service, evidenceStore *evidence.Store, cfg ObserveConfig, interval time.Duration) error {
	return RunReporting(ctx, lister, cases, execSvc, evidenceStore, cfg, interval, nil)
}
