package adoreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type ReviewInput struct {
	Case             workflowcase.Case
	WorkID           domain.ID
	Review           executors.ReviewResult
	ReviewEvidenceID domain.ID
	Repo             string
	PR               int64
	SourceCommit     string
	TargetCommit     string
	Project          string
	Caller           PRCaller
	RequireCI        bool
	CIStatus         string
	RemainingBudget  int64
}

type DecisionComment struct {
	Path string `json:"path"`
	Line int64  `json:"line"`
	Body string `json:"body"`
}

type ReviewDecision struct {
	Action   string            `json:"action"`
	Comments []DecisionComment `json:"comments"`
	Vote     string            `json:"vote"`
	Reason   string            `json:"reason"`
}

const (
	DecisionCommentAction = "comment"
	DecisionApproveAction = "approve"
	DecisionHoldAction    = "hold"

	effectApproveCapability = "ado.pr.approve"
	effectCommentCapability = "ado.pr.comment"
)

func AssessReview(ctx context.Context, cases *workflowcase.Service, evidenceStore *evidence.Store, input ReviewInput) (workflowcase.AssessmentResult, ReviewDecision, error) {
	var empty ReviewDecision
	if cases == nil || evidenceStore == nil {
		return workflowcase.AssessmentResult{}, empty, errors.New("case and evidence services are required")
	}
	if strings.TrimSpace(string(input.Case.ID)) == "" || strings.TrimSpace(string(input.WorkID)) == "" {
		return workflowcase.AssessmentResult{}, empty, errors.New("case and work IDs are required")
	}
	if strings.TrimSpace(string(input.ReviewEvidenceID)) == "" {
		return workflowcase.AssessmentResult{}, empty, errors.New("review evidence ID is required")
	}
	if strings.TrimSpace(input.Repo) == "" || input.PR <= 0 ||
		strings.TrimSpace(input.SourceCommit) == "" || strings.TrimSpace(input.TargetCommit) == "" {
		return workflowcase.AssessmentResult{}, empty, errors.New("review repo, pr, and commits are required")
	}
	if input.Caller == nil {
		return workflowcase.AssessmentResult{}, empty, errors.New("PR caller is required")
	}
	expectedCI := strings.TrimSpace(input.CIStatus)
	if expectedCI == "" {
		expectedCI = "succeeded"
	}

	objectID := input.Repo + "#" + strconv.FormatInt(input.PR, 10)
	revisionID := input.SourceCommit + ":" + input.TargetCommit
	signature := "ado:" + objectID + ":" + revisionID + ":" + string(input.Review.Verdict)

	// Replay story (honest): Assess idempotency is exact-request-JSON equality;
	// retries must be byte-identical (same evidence IDs, budget, signature).
	// EvidenceIDs embed random decision blob IDs so re-Put breaks equality;
	// the Slice 3 driver is single-pass by construction.
	finish := func(verdict workflow.Verdict, proposedActions []string, decision ReviewDecision) (workflowcase.AssessmentResult, ReviewDecision, error) {
		canonical, err := json.Marshal(decision)
		if err != nil {
			return workflowcase.AssessmentResult{}, empty, fmt.Errorf("encode review decision: %w", err)
		}
		object, err := evidenceStore.Put(ctx, strings.NewReader(string(canonical)),
			evidence.Metadata{MediaType: "application/json", Kind: "ado.review.decision"})
		if err != nil {
			return workflowcase.AssessmentResult{}, empty, fmt.Errorf("store review decision: %w", err)
		}
		next := workflow.WorkProposal{
			Kind:                 "publish-decision",
			RequiredCapabilities: append([]string(nil), input.Case.NextWork.RequiredCapabilities...),
			AuthorityCeiling:     append([]string(nil), input.Case.NextWork.AuthorityCeiling...),
			ProposedActions:      proposedActions,
		}
		result, err := cases.Assess(ctx, workflowcase.AssessmentRequest{
			CaseID: input.Case.ID, WorkID: input.WorkID,
			Assessment: workflow.Assessment{
				Verdict: verdict, Reason: decision.Reason,
				EvidenceIDs: []string{string(input.ReviewEvidenceID), string(object.ID)},
				Next:        &next,
			},
			RemainingBudget:   input.RemainingBudget,
			ProgressSignature: signature,
		})
		if err != nil {
			return workflowcase.AssessmentResult{}, empty, err
		}
		return result, decision, nil
	}
	hold := func(reason string) (workflowcase.AssessmentResult, ReviewDecision, error) {
		return finish(workflow.Unknown, nil, ReviewDecision{Action: DecisionHoldAction, Reason: reason})
	}

	getArgs := map[string]any{"action": "get", "repositoryId": input.Repo, "pullRequestId": input.PR}
	if strings.TrimSpace(input.Project) != "" {
		getArgs["project"] = input.Project
	}
	rawPR, err := input.Caller.Call(ctx, "ado.pr.get", getArgs)
	if err != nil {
		return hold(fmt.Sprintf("stale-review: %v", err))
	}
	prMap, ok := rawPR.(map[string]any)
	if !ok {
		return hold("stale-review: PR get response is not an object, cannot confirm freshness")
	}
	liveSource := stringField(prMap, "sourceCommit", "lastMergeSourceCommit")
	liveTarget := stringField(prMap, "targetCommit", "lastMergeTargetCommit")
	if liveSource == "" || liveTarget == "" {
		return hold("stale-review: PR get response is missing source or target commits")
	}
	if liveSource != input.SourceCommit || liveTarget != input.TargetCommit {
		return hold(fmt.Sprintf("stale-review: PR is now at %s:%s, review covered %s:%s",
			liveSource, liveTarget, input.SourceCommit, input.TargetCommit))
	}
	if !contains(input.Review.ReviewedCommits, liveSource) {
		return hold(fmt.Sprintf("stale-review: review did not cover current source commit %s", liveSource))
	}

	// Verdict-level checks run before path-membership consistency so the more
	// diagnostic reason wins (both hold; only the reason string changes).
	if input.Review.Verdict == executors.ReviewUncertain {
		return hold(fmt.Sprintf("uncertain-review: review of %s %s is inconclusive", objectID, revisionID))
	}
	if input.Review.Verdict == executors.ReviewClean && len(input.Review.Findings) > 0 {
		return hold("verdict-findings-mismatch: CLEAN verdict carries findings")
	}

	files := make(map[string]struct{}, len(input.Review.ReviewedFiles))
	for _, file := range input.Review.ReviewedFiles {
		files[file] = struct{}{}
	}
	for _, finding := range input.Review.Findings {
		if strings.TrimSpace(finding.Path) == "" {
			return hold("invalid-finding: review finding is missing path")
		}
		if _, ok := files[finding.Path]; !ok {
			return hold(fmt.Sprintf("uncovered-finding: finding path %q is outside the reviewed files", finding.Path))
		}
	}
	if input.Review.Verdict == executors.ReviewFindings && len(input.Review.Findings) == 0 {
		return hold("empty-findings: FINDINGS verdict carries no findings")
	}

	if input.RequireCI {
		buildID, ok := stringOrNumberField(prMap, "mergeBuildId", "buildId")
		if !ok || strings.TrimSpace(buildID) == "" {
			return hold("ci-unknown: PR get response is missing build ID")
		}
		rawBuild, err := input.Caller.Call(ctx, "ado.build.status", map[string]any{"action": "get_status", "buildId": buildID})
		if err != nil {
			return hold(fmt.Sprintf("ci-unknown: %v", err))
		}
		status := ""
		if buildMap, ok := rawBuild.(map[string]any); ok {
			status = stringField(buildMap, "status", "result")
		}
		if strings.TrimSpace(status) == "" {
			return hold("ci-unknown: build status is missing")
		}
		if status != expectedCI {
			return hold(fmt.Sprintf("ci-failed: build status %q does not satisfy required %q", status, expectedCI))
		}
	}

	switch input.Review.Verdict {
	case executors.ReviewUncertain:
		return hold(fmt.Sprintf("uncertain-review: review of %s %s is inconclusive", objectID, revisionID))
	case executors.ReviewClean:
		if len(input.Review.Findings) > 0 {
			return hold("verdict-findings-mismatch: CLEAN verdict carries findings")
		}
		if !contains(input.Case.Grant.Actions, effectApproveCapability) {
			return hold("grant-denies-ado.pr.approve: grant does not permit approving the PR")
		}
		if !contains(input.Case.Grant.Capabilities, effectApproveCapability) {
			return hold("grant-denies-capability:ado.pr.approve: grant does not permit approving the PR")
		}
		input.Case.NextWork.RequiredCapabilities = append(input.Case.NextWork.RequiredCapabilities, effectApproveCapability)
		input.Case.NextWork.AuthorityCeiling = append(input.Case.NextWork.AuthorityCeiling, effectApproveCapability)
		return finish(workflow.Continue, []string{effectApproveCapability}, ReviewDecision{
			Action: DecisionApproveAction, Vote: "approve",
			Reason: fmt.Sprintf("review CLEAN for %s %s; approving PR", objectID, revisionID),
		})
	case executors.ReviewFindings:
		if !contains(input.Case.Grant.Actions, effectCommentCapability) {
			return hold("grant-denies-ado.pr.comment: grant does not permit commenting on the PR")
		}
		if !contains(input.Case.Grant.Capabilities, effectCommentCapability) {
			return hold("grant-denies-capability:ado.pr.comment: grant does not permit commenting on the PR")
		}
		input.Case.NextWork.RequiredCapabilities = append(input.Case.NextWork.RequiredCapabilities, effectCommentCapability)
		input.Case.NextWork.AuthorityCeiling = append(input.Case.NextWork.AuthorityCeiling, effectCommentCapability)
		comments := make([]DecisionComment, 0, len(input.Review.Findings))
		for _, finding := range input.Review.Findings {
			comments = append(comments, renderComment(finding))
		}
		return finish(workflow.Continue, []string{effectCommentCapability}, ReviewDecision{
			Action: DecisionCommentAction, Comments: comments,
			Reason: fmt.Sprintf("review found %d findings for %s %s; commenting on PR", len(comments), objectID, revisionID),
		})
	default:
		return workflowcase.AssessmentResult{}, empty, fmt.Errorf("invalid review verdict %q", input.Review.Verdict)
	}
}

func renderComment(finding executors.ReviewFinding) DecisionComment {
	var body string
	if finding.Line > 0 {
		body = fmt.Sprintf("%s:%d: %s", finding.Path, finding.Line, finding.Explanation)
	} else {
		body = fmt.Sprintf("%s: %s", finding.Path, finding.Explanation)
	}
	if strings.TrimSpace(finding.Evidence) != "" {
		body += "\n\nEvidence: " + finding.Evidence
	}
	return DecisionComment{Path: finding.Path, Line: finding.Line, Body: body}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stringOrNumberField(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := m[key]
		if !ok {
			continue
		}
		switch number := value.(type) {
		case string:
			if strings.TrimSpace(number) != "" {
				return number, true
			}
		case float64:
			return strconv.FormatInt(int64(number), 10), true
		case float32:
			return strconv.FormatInt(int64(number), 10), true
		case int:
			return strconv.Itoa(number), true
		case int32:
			return strconv.FormatInt(int64(number), 10), true
		case int64:
			return strconv.FormatInt(number, 10), true
		case json.Number:
			if parsed, err := number.Int64(); err == nil {
				return strconv.FormatInt(parsed, 10), true
			}
			if strings.TrimSpace(number.String()) != "" {
				return number.String(), true
			}
		}
	}
	return "", false
}
