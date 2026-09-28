# GitHub Issue Triage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the already-registered `github.issue.triage` Tasks into a recorded, explainable triage decision per GitHub issue, and nothing more.

**Architecture:** A three-stage decision pipeline — deterministic stage 1, model stage 2, deterministic stage 3 — recorded as one evidence document, executed by a single Task per issue. A separate driver applies the case transition and Task acceptance after the lease completes, and marks superseded revisions. A separate reviewer, built last, re-derives the decision from stored inputs and reports one plausibility boolean. The model proposes, deterministic code disposes.

**Tech Stack:** Go 1.27, SQLite via `modernc.org/sqlite`, goose migrations, `encoding/json`, `database/sql`, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-09-28-github-issue-triage-design.md`

## Global Constraints

- Run every Go command with `GOCACHE=/tmp/summa42-full-go-cache`. The default `GOCACHE` is not writable in this environment.
- The full validation matrix from `CONTRIBUTING.md` applies before the branch is considered done: `go test ./... -count=1`, `go test -race ./... -count=1`, `go vet ./...`, `go build ./cmd/summa42 ./cmd/summa42-box`, the SQLite persistence spike, and the OCI capability acceptance profile.
- The concurrency flake gate is `go test -run TestCloseConcurrentReplay ./internal/workflowcase -count=600 -timeout 30m`. Do not multiply the whole `internal/workflowcase` package by a high `-count`; the package takes about 12 s per run.
- The repository is not globally gofmt-clean. Only the files this plan touches must be gofmt-clean. Verify with `gofmt -l <touched files>`, never with a whole-repo `gofmt -l internal cmd`.
- `AGENTS.md` at the repository root is untracked on purpose. Never `git add` it.
- No GitHub write of any kind. No planning. No code writing. No new column on `workflow_cases`.
- Fixed identifiers, used by every task. Do not rename them:
  - New package `internal/ghtriage`; executor kind `github-issue-triage`; task class `github.issue.triage`; capability `github.issue.read`
  - Evidence kinds `github.issue.triage.decision`, `github.issue.triage.failure`, `github.issue.triage.review`, `github.issue.triage.accepted`
  - Rule versions `ghtriage.rules.v1`, `ghtriage.dispositions.v1`, `ghtriage.reviewer.v1`
  - Document schemas `github.issue.triage.decision.v1`, `github.issue.triage.stage2.input.v1`, `github.issue.triage.review.v1`
  - Commands `run-gh-triage-driver`, `run-gh-triage-review`

### Verified API surface

Every constructor and method below was read from the code before this plan was written. Use these exact forms; do not invent alternatives.

```go
stateStore  *state.Store                 // internal/state/sqlite
evidence.New(store *state.Store, root string, clk clock.Clock) (*evidence.Store, error)
evidenceStore.Put(ctx, io.Reader, evidence.Metadata{MediaType, Kind}) (evidence.EvidenceObject, error)
evidenceStore.Get(ctx, id domain.ID) (evidence.EvidenceObject, []byte, error)
evidenceStore.FindByContentHash(ctx, contentHash, kind string) (evidence.EvidenceObject, bool, error)

purpose.New(store, clk) *purpose.Service
execution.New(store, clk, purposes) *execution.Service
executionSvc.Task(ctx, id domain.ID) (domain.Task, error)
executionSvc.FindByIdempotencyKey(ctx, key string) (domain.Task, bool, error)
executionSvc.CreateTask(ctx, execution.TaskRequest) (domain.Task, error)
executionSvc.ChallengeTask(ctx, taskID, scope, reason, evidenceIDs) error

verification.New(store, clk, executionService) *verification.Service
verificationSvc.CompleteAttempt(ctx, attemptID, verification.CompletionManifest{EvidenceIDs []domain.ID}) (verification.CompletionRecord, error)
verificationSvc.AcceptTask(ctx, taskID, verification.AcceptanceRequest{
    VerifierID, VerifierType string/ID, CriteriaMet bool, EvidenceIDs []domain.ID,
}) (verification.AcceptanceRecord, error)

workflowcase.New(store, clk, purposes) *workflowcase.Service
casesSvc.Get(ctx, caseID) (workflowcase.Case, error)
casesSvc.ListActive(ctx, missionID) ([]workflowcase.Case, error)
casesSvc.ListAssessments(ctx, caseID) ([]workflowcase.AssessmentRecord, error)
casesSvc.Assess(ctx, workflowcase.AssessmentRequest{
    CaseID, WorkID domain.ID, Assessment workflow.Assessment,
    RemainingBudget int64, ProgressSignature string,
}) (workflowcase.AssessmentResult, error)

runmanifest.New(store, runmanifest.StaticContext) *runmanifest.Service
manifestSvc.Provenance(ctx, attemptID) (runmanifest.Provenance, error) // .OutputEvidence []runmanifest.EvidenceRef{ID, ContentHash}

scheduler.New(store, clk, purposes, execSvc, resourceSvc, leaseDuration, preferences ...ExecutorPreference) *scheduler.Service
```

`workflowcase.Assessment` is `workflow.Assessment{Verdict workflow.Verdict, Reason string, EvidenceIDs []string, Next *workflow.WorkProposal}`. `workflow.Unknown` yields `OutcomeBlocked`. `workflow.Assess` requires the reason's `EvidenceIDs` to be non-empty, which is why a failure document must exist before a `triage-failed` block.

`internal/verification` exposes **no** read accessor for an acceptance record. The driver therefore keeps its own `github.issue.triage.accepted` evidence, written before `AcceptTask`, as the restart replay path. Do not add a method to `internal/verification`.

### Two decisions an implementer must not "fix"

- **There is no `state_reason` column, and that is deliberate.** A column was considered and rejected on review. `Assess` with `Verdict: workflow.Unknown` already performs `ACTIVE` → `BLOCKED`, already persists the reason in `workflow_assessments.result_json`, and is already idempotent by exact-request replay. A `CHECK` tying `BLOCKED` to a non-empty reason would break the two shipped paths that block with no reason at all: budget and progress exhaustion, and `Reject`. No migration to `workflow_cases` belongs in this plan.
- **A model call is at least once, not exactly once.** The executor cannot inspect a completed decision in its own attempt's provenance before returning, because `Worker.completeExecution` persists the returned evidence and calls `CompleteAttempt` only after `Executor.Start` has returned, and every retry leases a new attempt ID. A crash between the model response and attempt completion therefore causes another model call. Triage makes no external write whose repetition would duplicate an effect, so the design accepts this. The durable boundary is `CompleteAttempt`, and the driver is the component that enforces one accepted decision per Task.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/ghtriage/document.go` | Decision document, dispositions, rule versions, canonical JSON, validation |
| `internal/ghtriage/stage1.go` | Snapshot type, signals, classification, the one decisive rule |
| `internal/ghtriage/stage3.go` | The total, ordered stage 3 rule table |
| `internal/ghtriage/stage2.go` | Building `Stage2Input`: filtered context, truncation, the fixed question |
| `internal/ghtriage/model.go` | Model interfaces, input and output types, response parsing |
| `internal/ghtriage/executor.go` | The `executors.Executor` implementation |
| `internal/ghtriage/driver.go` | Acceptance, case transition, failure blocking, supersession |
| `internal/ghtriage/reviewindex.go` | The review scheduling index |
| `internal/ghtriage/reviewer.go` | Stage 4 |
| `internal/ghtriage/climodel/adapter.go` | Production adapter exec'ing the configured model CLI |
| `internal/ghtriage/fakemodel/fake.go` | Scripted fake recording every input |
| `internal/state/sqlite/migrations/00016_github_issue_triage_reviews.sql` | Review scheduling table |

Migrations are discovered from the embedded filesystem by filename through goose, so creating the file is the entire registration step.

Modified existing files: `internal/execution/service.go` (guarded challenge), `internal/scheduler/service.go` and `internal/scheduler/routing.go` (routing preference), `internal/runtime/box.go` (routing config), `internal/workflowcase/service.go` (read path), `cmd/summa42-box/main.go` (capability, registration, two subcommands).

---

### Task 1: Decision document, stage 1 and stage 3

Pure functions, no I/O. Everything else consumes this.

**Files:**
- Create: `internal/ghtriage/document.go`
- Create: `internal/ghtriage/stage1.go`
- Create: `internal/ghtriage/stage3.go`
- Test: `internal/ghtriage/document_test.go`, `internal/ghtriage/stage1_test.go`, `internal/ghtriage/stage3_test.go`

**Interfaces:**
- Consumes: nothing. This is the first task.
- Produces: `Disposition`, `Triage`, `Scope`, `SuggestedType` string types with their constants; `Snapshot`; `Stage1Result` with `Resolved() bool`; `Stage2Output`; `Stage3Result`; `Decision` with `FinalDisposition() Disposition`, `Canonical() ([]byte, error)`, `Validate() error`; constants `TriageRulesVersion`, `DispositionRulesVersion`, `DecisionSchema`, `Stage2InputSchema`, `ReviewSchema`, `KindDecision`, `KindFailure`, `KindReview`, `KindAccepted`; `Stage1(Snapshot) Stage1Result`; `Stage3(Stage2Output) Stage3Result`.

- [ ] **Step 1: Write the failing stage 1 test**

Create `internal/ghtriage/stage1_test.go`:

```go
package ghtriage

import "testing"

func snapshot(labels []string, title, body, triage string) Snapshot {
	return Snapshot{
		Repo: "o/r", Issue: 42, Title: title, Body: body,
		Author: "maintainer", Labels: labels, Triage: triage,
		UpdatedAt: "2026-09-28T10:00:00Z",
	}
}

func TestStage1ResolvesOnlyAValidDuplicateLabel(t *testing.T) {
	cases := []struct {
		name       string
		snap       Snapshot
		wantTriage Triage
		wantDisp   Disposition
		wantRule   string
	}{
		{"single valid duplicate label resolves",
			snapshot([]string{"duplicate-of:41", "bug"}, "Crash on save", "boom", TriageBug),
			TriageDuplicate, DispositionDuplicate, "explicit-duplicate-label"},
		{"bare hash reference is not proof",
			snapshot([]string{"bug"}, "Crash on save", "same as #41 happened", TriageBug),
			TriageBug, "", ""},
		{"self reference is not a duplicate",
			snapshot([]string{"duplicate-of:42"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
		{"conflicting duplicate labels are unresolved",
			snapshot([]string{"duplicate-of:41", "duplicate-of:43"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
		{"malformed duplicate label is unresolved",
			snapshot([]string{"duplicate-of:abc"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Stage1(tc.snap)
			if got.Triage != tc.wantTriage {
				t.Fatalf("triage = %q, want %q", got.Triage, tc.wantTriage)
			}
			if got.Disposition != tc.wantDisp {
				t.Fatalf("disposition = %q, want %q", got.Disposition, tc.wantDisp)
			}
			if got.Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", got.Rule, tc.wantRule)
			}
		})
	}
}

func TestStage1WidensOnlyUnclassifiedToQuestion(t *testing.T) {
	labelled := Stage1(snapshot([]string{"bug", "question"}, "Crash", "boom", TriageBug))
	if labelled.Triage != TriageBug {
		t.Fatalf("a labelled bug widened to %q", labelled.Triage)
	}
	prefix := Stage1(snapshot(nil, "[question] Why is this slow", "boom", TriageUnclassified))
	if prefix.Triage != TriageQuestion {
		t.Fatalf("unclassified with a [question] prefix = %q, want question", prefix.Triage)
	}
}

func TestStage1ExtractsReproSignal(t *testing.T) {
	fenced := Stage1(snapshot([]string{"bug"}, "Crash", "steps\n```\npanic\n```\n", TriageBug))
	if !containsString(fenced.Signals, "has-repro") {
		t.Fatalf("signals %v lack has-repro", fenced.Signals)
	}
	plain := Stage1(snapshot([]string{"bug"}, "Crash", "it just crashes", TriageBug))
	if containsString(plain.Signals, "has-repro") {
		t.Fatalf("signals %v claim has-repro", plain.Signals)
	}
}

func TestStage1SignalsAreSorted(t *testing.T) {
	got := Stage1(snapshot([]string{"bug", "enhancement", "p1"}, "[bug] Crash", "x", TriageBug))
	for i := 1; i < len(got.Signals); i++ {
		if got.Signals[i-1] > got.Signals[i] {
			t.Fatalf("signals are not sorted: %v", got.Signals)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestStage1 -count=1`
Expected: FAIL, `undefined: Stage1` and `undefined: Snapshot`.

- [ ] **Step 3: Write the document types**

Create `internal/ghtriage/document.go`:

```go
package ghtriage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	TriageRulesVersion      = "ghtriage.rules.v1"
	DispositionRulesVersion = "ghtriage.dispositions.v1"
	ReviewerVersion         = "ghtriage.reviewer.v1"

	DecisionSchema   = "github.issue.triage.decision.v1"
	Stage2InputSchema = "github.issue.triage.stage2.input.v1"
	ReviewSchema     = "github.issue.triage.review.v1"

	KindDecision  = "github.issue.triage.decision"
	KindFailure   = "github.issue.triage.failure"
	KindReview    = "github.issue.triage.review"
	KindAccepted  = "github.issue.triage.accepted"
)

// Disposition is a closed vocabulary. Stage 3 is total over it, so no
// decision can name a disposition outside this set.
type Disposition string

const (
	DispositionReadyToPlan   Disposition = "ready-to-plan"
	DispositionNotActionable Disposition = "not-actionable"
	DispositionDuplicate     Disposition = "duplicate"
	DispositionNeedsHuman    Disposition = "needs-human"
)

func (d Disposition) valid() bool {
	switch d {
	case DispositionReadyToPlan, DispositionNotActionable, DispositionDuplicate, DispositionNeedsHuman:
		return true
	}
	return false
}

type Triage string

const (
	TriageBug          Triage = "bug"
	TriageFeature      Triage = "feature"
	TriageUnclassified Triage = "unclassified"
	TriageQuestion     Triage = "question"
	TriageDuplicate    Triage = "duplicate"
)

type Scope string

const (
	ScopeSmall   Scope = "small"
	ScopeMedium  Scope = "medium"
	ScopeLarge   Scope = "large"
	ScopeUnknown Scope = "unknown"
)

type SuggestedType string

const (
	SuggestedTypeBug      SuggestedType = "bug"
	SuggestedTypeFeature  SuggestedType = "feature"
	SuggestedTypeQuestion SuggestedType = "question"
	SuggestedTypeOther    SuggestedType = "other"
)

// Snapshot mirrors the JSON that ghissue.CanonicalSnapshot writes, so a stored
// issue snapshot decodes without a conversion.
type Snapshot struct {
	Repo      string   `json:"repo"`
	Issue     int64    `json:"issue"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Author    string   `json:"author"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
	URL       string   `json:"url"`
	UpdatedAt string   `json:"updatedAt"`
	Triage    string   `json:"triage"`
}

type Stage1Result struct {
	Triage      Triage      `json:"triage"`
	Signals     []string    `json:"signals"`
	Disposition Disposition `json:"disposition,omitempty"`
	Rule        string      `json:"rule,omitempty"`
}

func (s Stage1Result) Resolved() bool { return s.Disposition != "" }

type Stage2Output struct {
	IsActionable  bool         `json:"is_actionable"`
	NeedsRepro    bool         `json:"needs_repro"`
	Scope         Scope        `json:"scope"`
	SuggestedType SuggestedType `json:"suggested_type"`
	Rationale     string       `json:"rationale"`
}

type Stage3Result struct {
	Disposition Disposition `json:"disposition"`
	Rule        string      `json:"rule"`
}

type Decision struct {
	Schema                  string        `json:"schema"`
	Repository              string        `json:"repository"`
	Issue                   int64         `json:"issue"`
	Revision                string        `json:"revision"`
	SnapshotEvidenceID      string        `json:"snapshot_evidence_id"`
	TriageRulesVersion      string        `json:"triage_rules_version"`
	DispositionRulesVersion string        `json:"disposition_rules_version"`
	Stage1                  Stage1Result  `json:"stage1"`
	Stage2                  *Stage2Output `json:"stage2,omitempty"`
	Stage3                  *Stage3Result `json:"stage3,omitempty"`
}

func (d Decision) FinalDisposition() Disposition {
	if d.Stage1.Resolved() {
		return d.Stage1.Disposition
	}
	if d.Stage3 != nil {
		return d.Stage3.Disposition
	}
	return ""
}

// Canonical is deterministic because Decision is a struct, so encoding/json
// emits its fields in declaration order. The absence of stage 2 and stage 3 on
// a stage 1 decision is the record that the model was not consulted.
func (d Decision) Canonical() ([]byte, error) { return json.Marshal(d) }

func (d *Decision) Validate() error {
	if d == nil {
		return errors.New("decision is required")
	}
	if d.Schema != DecisionSchema {
		return fmt.Errorf("decision schema = %q, want %q", d.Schema, DecisionSchema)
	}
	if strings.TrimSpace(d.Repository) == "" {
		return errors.New("decision repository is required")
	}
	if d.Issue <= 0 {
		return errors.New("decision issue number must be positive")
	}
	if strings.TrimSpace(d.Revision) == "" {
		return errors.New("decision revision is required")
	}
	if strings.TrimSpace(d.SnapshotEvidenceID) == "" {
		return errors.New("decision snapshot evidence id is required")
	}
	if d.TriageRulesVersion == "" || d.DispositionRulesVersion == "" {
		return errors.New("decision rule versions are required")
	}
	if d.Stage1.Resolved() {
		if d.Stage2 != nil || d.Stage3 != nil {
			return errors.New("a stage 1 decision must not carry stage 2 or stage 3")
		}
		return d.Stage1.Disposition.valid()
	}
	if d.Stage2 == nil || d.Stage3 == nil {
		return errors.New("an unresolved stage 1 requires both stage 2 and stage 3")
	}
	return d.Stage3.Disposition.valid()
}
```

- [ ] **Step 4: Write stage 1**

Create `internal/ghtriage/stage1.go`:

```go
package ghtriage

import (
	"sort"
	"strconv"
	"strings"
)

const (
	duplicateLabelPrefix = "duplicate-of:"
	questionLabel        = "question"
)

// Stage1 is a pure function from the canonical snapshot to signals, a
// classification, and — for exactly one decisive case — a disposition. It is
// deliberately conservative: the only decisive rule requires an explicit,
// well-formed duplicate label, because a bare #N in the body is context and
// the snapshot carries no other issue's state to verify one against.
func Stage1(snap Snapshot) Stage1Result {
	signals := make([]string, 0, len(snap.Labels)+3)
	for _, label := range snap.Labels {
		signals = append(signals, "label:"+label)
	}
	if prefix := titlePrefix(snap.Title); prefix != "" {
		signals = append(signals, "title:["+prefix+"]")
	}
	if hasFencedBlock(snap.Body) {
		signals = append(signals, "has-repro")
	}
	target, duplicate := validDuplicateTarget(snap)
	if duplicate {
		signals = append(signals, duplicateLabelPrefix+strconv.FormatInt(target, 10))
	}
	sort.Strings(signals)

	triage := Triage(snap.Triage)
	switch {
	case duplicate:
		triage = TriageDuplicate
	case triage == TriageUnclassified && questionSignalled(snap):
		triage = TriageQuestion
	}

	result := Stage1Result{Triage: triage, Signals: signals}
	if duplicate {
		result.Disposition = DispositionDuplicate
		result.Rule = "explicit-duplicate-label"
	}
	return result
}

func titlePrefix(title string) string {
	trimmed := strings.TrimSpace(title)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	end := strings.Index(trimmed, "]")
	if end < 2 {
		return ""
	}
	return strings.ToLower(trimmed[1:end])
}

func questionSignalled(snap Snapshot) bool {
	if titlePrefix(snap.Title) == "question" {
		return true
	}
	for _, label := range snap.Labels {
		if strings.EqualFold(strings.TrimSpace(label), questionLabel) {
			return true
		}
	}
	return false
}

// validDuplicateTarget accepts exactly one well-formed duplicate-of label
// naming a positive issue other than this one. Zero, several, or a malformed
// label all leave the issue unresolved.
func validDuplicateTarget(snap Snapshot) (int64, bool) {
	target := int64(0)
	found := 0
	for _, label := range snap.Labels {
		raw, ok := strings.CutPrefix(strings.TrimSpace(label), duplicateLabelPrefix)
		if !ok {
			continue
		}
		number, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || number <= 0 || number == snap.Issue {
			return 0, false
		}
		target = number
		found++
	}
	if found != 1 {
		return 0, false
	}
	return target, true
}

func hasFencedBlock(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return true
		}
	}
	return false
}
```

- [ ] **Step 5: Run the stage 1 test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestStage1 -count=1`
Expected: PASS.

- [ ] **Step 6: Write the failing stage 3 test**

Create `internal/ghtriage/stage3_test.go`:

```go
package ghtriage

import "testing"

func TestStage3RuleTableIsTotal(t *testing.T) {
	cases := []struct {
		name     string
		out      Stage2Output
		wantDisp Disposition
		wantRule string
	}{
		{"not actionable wins first", Stage2Output{IsActionable: false, NeedsRepro: true, Scope: ScopeSmall}, DispositionNotActionable, "not-actionable"},
		{"needs repro", Stage2Output{IsActionable: true, NeedsRepro: true, Scope: ScopeSmall}, DispositionNeedsHuman, "needs-repro-required"},
		{"small is ready", Stage2Output{IsActionable: true, Scope: ScopeSmall}, DispositionReadyToPlan, "actionable-without-repro-small-or-medium"},
		{"medium is ready", Stage2Output{IsActionable: true, Scope: ScopeMedium}, DispositionReadyToPlan, "actionable-without-repro-small-or-medium"},
		{"large needs decomposition", Stage2Output{IsActionable: true, Scope: ScopeLarge}, DispositionNeedsHuman, "scope-large-needs-decomposition"},
		{"unknown needs scoping", Stage2Output{IsActionable: true, Scope: ScopeUnknown}, DispositionNeedsHuman, "scope-unknown-needs-scoping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Stage3(tc.out)
			if got.Disposition != tc.wantDisp {
				t.Fatalf("disposition = %q, want %q", got.Disposition, tc.wantDisp)
			}
			if got.Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", got.Rule, tc.wantRule)
			}
			if !got.Disposition.valid() {
				t.Fatalf("disposition %q is outside the closed set", got.Disposition)
			}
		})
	}
}

func TestStage3NeverProducesDuplicate(t *testing.T) {
	for _, scope := range []Scope{ScopeSmall, ScopeMedium, ScopeLarge, ScopeUnknown} {
		if got := Stage3(Stage2Output{IsActionable: true, Scope: scope}); got.Disposition == DispositionDuplicate {
			t.Fatalf("stage 3 produced duplicate for scope %q", scope)
		}
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestStage3 -count=1`
Expected: FAIL, `undefined: Stage3`.

- [ ] **Step 8: Write stage 3**

Create `internal/ghtriage/stage3.go`:

```go
package ghtriage

// Stage3 is a pure, total function from a validated stage 2 document to a
// disposition and the rule that produced it. The first matching branch wins
// and the vocabulary is covered exhaustively, so this cannot fall through.
// It never returns duplicate: an explicit duplicate label is a stage 1 signal
// that resolves the issue decisively, so the disposition has exactly one
// producer.
func Stage3(out Stage2Output) Stage3Result {
	switch {
	case !out.IsActionable:
		return Stage3Result{Disposition: DispositionNotActionable, Rule: "not-actionable"}
	case out.NeedsRepro:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "needs-repro-required"}
	case out.Scope == ScopeSmall || out.Scope == ScopeMedium:
		return Stage3Result{Disposition: DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"}
	case out.Scope == ScopeLarge:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "scope-large-needs-decomposition"}
	default:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "scope-unknown-needs-scoping"}
	}
}
```

- [ ] **Step 9: Write the failing document test**

Create `internal/ghtriage/document_test.go`:

```go
package ghtriage

import (
	"encoding/json"
	"strings"
	"testing"
)

func stage1Decision() Decision {
	return Decision{
		Schema:                  DecisionSchema,
		Repository:              "o/r",
		Issue:                   42,
		Revision:                "2026-09-28T10:00:00Z",
		SnapshotEvidenceID:      "evidence-1",
		TriageRulesVersion:      TriageRulesVersion,
		DispositionRulesVersion: DispositionRulesVersion,
		Stage1: Stage1Result{
			Triage: TriageDuplicate, Signals: []string{"duplicate-of:41"},
			Disposition: DispositionDuplicate, Rule: "explicit-duplicate-label",
		},
	}
}

func stage3Decision() Decision {
	d := stage1Decision()
	d.Stage1 = Stage1Result{Triage: TriageBug, Signals: []string{"has-repro"}}
	d.Stage2 = &Stage2Output{IsActionable: true, Scope: ScopeSmall, SuggestedType: SuggestedTypeBug, Rationale: "enough detail"}
	d.Stage3 = &Stage3Result{Disposition: DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"}
	return d
}

func TestDecisionValidate(t *testing.T) {
	if err := stage1Decision().Validate(); err != nil {
		t.Fatalf("a stage 1 decision was rejected: %v", err)
	}
	if err := stage3Decision().Validate(); err != nil {
		t.Fatalf("a stage 3 decision was rejected: %v", err)
	}
	carriesStage3 := stage1Decision()
	carriesStage3.Stage3 = &Stage3Result{Disposition: DispositionReadyToPlan, Rule: "x"}
	if err := carriesStage3.Validate(); err == nil {
		t.Fatal("a stage 1 decision carrying stage 3 was accepted")
	}
	noSnapshot := stage1Decision()
	noSnapshot.SnapshotEvidenceID = ""
	if err := noSnapshot.Validate(); err == nil {
		t.Fatal("a decision without a snapshot evidence id was accepted")
	}
	noStage2 := stage3Decision()
	noStage2.Stage2 = nil
	if err := noStage2.Validate(); err == nil {
		t.Fatal("an unresolved stage 1 without stage 2 was accepted")
	}
}

func TestDecisionFinalDisposition(t *testing.T) {
	if got := stage1Decision().FinalDisposition(); got != DispositionDuplicate {
		t.Fatalf("stage 1 final disposition = %q", got)
	}
	if got := stage3Decision().FinalDisposition(); got != DispositionReadyToPlan {
		t.Fatalf("stage 3 final disposition = %q", got)
	}
}

func TestDecisionCanonicalIsStableAndOmitsAbsentStages(t *testing.T) {
	first, err := stage1Decision().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	second, err := stage1Decision().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("canonical bytes are not stable across calls")
	}
	if strings.Contains(string(first), "stage2") || strings.Contains(string(first), "stage3") {
		t.Fatalf("a stage 1 decision leaked later stages: %s", first)
	}
	var round Decision
	if err := json.Unmarshal(first, &round); err != nil {
		t.Fatal(err)
	}
	if round.Stage1.Rule != "explicit-duplicate-label" {
		t.Fatalf("the round trip lost the stage 1 rule: %+v", round.Stage1)
	}
}
```

- [ ] **Step 10: Run the package tests to verify they pass**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -count=1`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
gofmt -l internal/ghtriage
git add internal/ghtriage
git commit -m "feat(ghtriage): add the decision document, stage 1 and stage 3

Stage 3 is total, so it cannot fall through, and it never returns
duplicate: an explicit duplicate label is a stage 1 signal that resolves
the issue decisively, so the disposition has exactly one producer. Stage 1
stays deliberately conservative and treats a bare #N as context rather
than proof, so the only decisive rule requires one well-formed
duplicate-of label naming another issue."
```

---

### Task 2: Stage 2 input preparation

The "do not stuff the context" rule as one pure function with a testable constant.

**Files:**
- Create: `internal/ghtriage/stage2.go`
- Test: `internal/ghtriage/stage2_test.go`

**Interfaces:**
- Consumes: `Snapshot` from Task 1.
- Produces: `Stage2Input`; `Stage2Question`; `BuildStage2Input(snap Snapshot, signals []string) Stage2Input`.

- [ ] **Step 1: Write the failing test**

Create `internal/ghtriage/stage2_test.go`:

```go
package ghtriage

import (
	"strings"
	"testing"
)

func TestStage2QuestionIsTheFixedLiteral(t *testing.T) {
	input := BuildStage2Input(snapshot(nil, "Crash", "boom", TriageBug), nil)
	if input.Question != Stage2Question {
		t.Fatalf("question = %q, want the fixed literal", input.Question)
	}
	if !strings.Contains(input.Question, "Answer with JSON only") {
		t.Fatalf("the question does not constrain the response format: %q", input.Question)
	}
}

func TestBuildStage2InputNeverCarriesTheReproductionBlock(t *testing.T) {
	body := "steps to reproduce\n```\npanic: nil\n```\nand then it dies"
	input := BuildStage2Input(snapshot([]string{"bug"}, "Crash on save", body, TriageBug), []string{"has-repro", "label:bug"})

	if strings.Contains(input.BodyExcerpt, "panic: nil") {
		t.Fatalf("the reproduction block reached the model: %q", input.BodyExcerpt)
	}
	if !strings.Contains(input.BodyExcerpt, "and then it dies") {
		t.Fatalf("prose was lost with the reproduction block: %q", input.BodyExcerpt)
	}
	if input.Schema != Stage2InputSchema {
		t.Fatalf("schema = %q", input.Schema)
	}
}

func TestBuildStage2InputTruncatesTitleAndBody(t *testing.T) {
	input := BuildStage2Input(
		snapshot(nil, strings.Repeat("a", 500), strings.Repeat("b", 9000), TriageBug), nil)

	if got := len([]rune(input.Title)); got != 200 {
		t.Fatalf("title runes = %d, want 200", got)
	}
	if !strings.HasSuffix(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a truncated body lacks the marker: %q", input.BodyExcerpt)
	}
	if got := len([]rune(strings.TrimSuffix(input.BodyExcerpt, truncationMarker))); got != 4000 {
		t.Fatalf("body excerpt runes = %d, want 4000", got)
	}
}

func TestBuildStage2InputUntruncatedBodyHasNoMarker(t *testing.T) {
	input := BuildStage2Input(snapshot(nil, "Crash", "short body", TriageBug), nil)
	if strings.Contains(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a short body was marked truncated: %q", input.BodyExcerpt)
	}
}

func TestBuildStage2InputSortsLabelsAndSignals(t *testing.T) {
	input := BuildStage2Input(
		snapshot([]string{"p1", "bug", "enhancement"}, "t", "b", TriageBug),
		[]string{"title:[bug]", "has-repro"})

	if input.Labels[0] != "bug" {
		t.Fatalf("labels are not sorted: %v", input.Labels)
	}
	if input.Signals[0] != "has-repro" {
		t.Fatalf("signals are not sorted: %v", input.Signals)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run 'TestStage2|TestBuildStage2' -count=1`
Expected: FAIL, `undefined: BuildStage2Input` and `undefined: Stage2Question`.

- [ ] **Step 3: Write the implementation**

Create `internal/ghtriage/stage2.go`:

```go
package ghtriage

import (
	"sort"
	"strings"
)

const (
	titleRuneLimit   = 200
	bodyRuneLimit    = 4000
	truncationMarker = "[…truncated]"
)

// Stage2Question is the one and only question stage 2 is asked. It is an
// exported constant so a test can assert on the exact prompt the model
// receives, rather than on a string buried in a call site.
const Stage2Question = "Classify this issue. State whether it is actionable without further " +
	"information from the reporter, whether it needs a reproduction before it can be planned, " +
	"how large the change would be, and what type of work it is. Answer with JSON only."

type Stage2Input struct {
	Schema      string   `json:"schema"`
	Title       string   `json:"title"`
	Labels      []string `json:"labels"`
	Signals     []string `json:"signals"`
	BodyExcerpt string   `json:"body_excerpt"`
	Question    string   `json:"question"`
}

// BuildStage2Input prepares the only context the model ever sees. The raw
// issue is never passed through: the reproduction block that stage 1 already
// turned into a signal is removed, and both free-text fields are bounded.
func BuildStage2Input(snap Snapshot, signals []string) Stage2Input {
	labels := append([]string(nil), snap.Labels...)
	sort.Strings(labels)
	sorted := append([]string(nil), signals...)
	sort.Strings(sorted)

	body := stripLeadingFence(snap.Body)
	if runes := []rune(body); len(runes) > bodyRuneLimit {
		body = string(runes[:bodyRuneLimit]) + truncationMarker
	}

	return Stage2Input{
		Schema:      Stage2InputSchema,
		Title:       truncateRunes(snap.Title, titleRuneLimit),
		Labels:      labels,
		Signals:     sorted,
		BodyExcerpt: body,
		Question:    Stage2Question,
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// stripLeadingFence drops a reproduction block that opens the body, because
// stage 1 has already turned it into the has-repro signal.
func stripLeadingFence(body string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			start = i
		}
		break
	}
	if start < 0 {
		return body
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return body
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/ghtriage
git add internal/ghtriage
git commit -m "feat(ghtriage): prepare a bounded, filtered stage 2 context

The model never receives the raw issue. The reproduction block stage 1
already turned into a signal is stripped, the title and body are bounded
by rune count, and the question is an exported constant so the exact
prompt is assertable in a test rather than only inspectable by eye."
```

---

### Task 3: Model interfaces, CLI adapter and fake

The repository has no provider abstraction, so this slice follows the existing precedent of exec'ing a model CLI rather than inventing one.

**Files:**
- Create: `internal/ghtriage/model.go`
- Create: `internal/ghtriage/climodel/adapter.go`
- Create: `internal/ghtriage/climodel/adapter_test.go`
- Create: `internal/ghtriage/fakemodel/fake.go`

**Interfaces:**
- Consumes: `Stage2Input`, `Stage2Output`, `Decision` from Tasks 1-2.
- Produces: `ClassifierModel` and `ReviewerModel` interfaces; `ReviewInput`; `ReviewerQuestion`; `ParseStage2Output([]byte) (Stage2Output, error)`; `ParseReviewVerdict([]byte) (bool, error)`; `climodel.New(climodel.Config) (*climodel.Adapter, error)` with `Config{Binary string, Args []string, Timeout time.Duration}`; `(*Adapter).Classify`; `(*Adapter).Review`; `fakemodel.New() *Fake` with `Scripted`, `ScriptedReview`, `ClassifyInputs`, `ReviewInputs`, `Err`, `ClassifyCallCount()`, `ReviewCallCount()`.

- [ ] **Step 1: Write the failing adapter test**

Create `internal/ghtriage/climodel/adapter_test.go`:

```go
package climodel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// writeScript creates an executable that consumes the prompt on stdin and
// prints body on stdout, so the adapter can be driven without a real model.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.sh")
	script := "#!/bin/sh\ncat > /dev/null\ncat <<'JSON'\n" + body + "\nJSON\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("an empty binary was accepted")
	}
	if _, err := New(Config{Binary: filepath.Join(t.TempDir(), "absent"), Timeout: time.Second}); err == nil {
		t.Fatal("a missing binary was accepted")
	}
	if _, err := New(Config{Binary: writeScript(t, "{}")}); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}

func TestClassifyRequiresExactlyOneJSONObject(t *testing.T) {
	valid := `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"enough detail"}`
	cases := []struct {
		name    string
		stdout  string
		wantErr bool
	}{
		{"one object", valid, false},
		{"two objects", valid + "\n" + valid, true},
		{"trailing prose", valid + "\nsome explanation", true},
		{"not json", "I cannot answer that", true},
		{"wrong field type", `{"is_actionable":"yes"}`, true},
		{"unknown field", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug","rationale":"x","severity":"high"}`, true},
		{"missing rationale", `{"is_actionable":true,"needs_repro":false,"scope":"small","suggested_type":"bug"}`, true},
		{"scope outside the vocabulary", `{"is_actionable":true,"needs_repro":false,"scope":"enormous","suggested_type":"bug","rationale":"x"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := New(Config{Binary: writeScript(t, tc.stdout), Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, err = adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema})
			if tc.wantErr && err == nil {
				t.Fatal("a non-conforming response was accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a conforming response was rejected: %v", err)
			}
		})
	}
}

func TestReviewReturnsOnlyABoolean(t *testing.T) {
	adapter, err := New(Config{Binary: writeScript(t, `{"plausible":true}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	plausible, err := adapter.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema})
	if err != nil {
		t.Fatal(err)
	}
	if !plausible {
		t.Fatal("plausible = false, want true")
	}
	verbose, err := New(Config{Binary: writeScript(t, `{"plausible":true,"severity":"high"}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verbose.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict carrying a severity was accepted")
	}
	missing, err := New(Config{Binary: writeScript(t, `{}`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Review(context.Background(), ghtriage.ReviewInput{Schema: ghtriage.ReviewSchema}); err == nil {
		t.Fatal("a verdict without plausible was accepted")
	}
}

func TestClassifyFailsWhenTheProcessFails(t *testing.T) {
	failing := filepath.Join(t.TempDir(), "failing.sh")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\ncat > /dev/null\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	adapter, err := New(Config{Binary: failing, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Classify(context.Background(), ghtriage.Stage2Input{Schema: ghtriage.Stage2InputSchema}); err == nil {
		t.Fatal("a failing model process was treated as success")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/climodel -count=1`
Expected: FAIL, `undefined: New`.

- [ ] **Step 3: Write the model interfaces and response parsers**

Create `internal/ghtriage/model.go`:

```go
package ghtriage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const ReviewerQuestion = "Does the recorded classification follow from what this issue " +
	"actually describes? Answer with JSON containing only a single boolean field named plausible."

// ClassifierModel turns a prepared, bounded context into signals. It never
// returns a disposition: stage 3 owns that.
type ClassifierModel interface {
	Classify(ctx context.Context, input Stage2Input) (Stage2Output, error)
}

// ReviewerModel answers one plausibility question with one boolean.
type ReviewerModel interface {
	Review(ctx context.Context, input ReviewInput) (bool, error)
}

type ReviewInput struct {
	Schema   string   `json:"schema"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Question string   `json:"question"`
	Decision Decision `json:"decision"`
}

type reviewResponse struct {
	Plausible *bool `json:"plausible"`
}

// ParseStage2Output accepts exactly one conforming object. Unknown fields are
// rejected rather than ignored, so a model that invents a field is treated as
// non-conforming rather than partially understood.
func ParseStage2Output(raw []byte) (Stage2Output, error) {
	var out Stage2Output
	if err := decodeExactlyOne(raw, &out); err != nil {
		return Stage2Output{}, err
	}
	if strings.TrimSpace(out.Rationale) == "" {
		return Stage2Output{}, errors.New("stage 2 rationale is required")
	}
	switch out.Scope {
	case ScopeSmall, ScopeMedium, ScopeLarge, ScopeUnknown:
	default:
		return Stage2Output{}, fmt.Errorf("stage 2 scope %q is outside the vocabulary", out.Scope)
	}
	switch out.SuggestedType {
	case SuggestedTypeBug, SuggestedTypeFeature, SuggestedTypeQuestion, SuggestedTypeOther:
	default:
		return Stage2Output{}, fmt.Errorf("stage 2 suggested_type %q is outside the vocabulary", out.SuggestedType)
	}
	return out, nil
}

// ParseReviewVerdict accepts a response whose only content is plausible.
func ParseReviewVerdict(raw []byte) (bool, error) {
	var response reviewResponse
	if err := decodeExactlyOne(raw, &response); err != nil {
		return false, err
	}
	if response.Plausible == nil {
		return false, errors.New("review response carries no plausible field")
	}
	return *response.Plausible, nil
}

func decodeExactlyOne(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode model response: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("model response must contain exactly one JSON object and nothing else")
	}
	return nil
}
```

- [ ] **Step 4: Write the adapter**

Create `internal/ghtriage/climodel/adapter.go`:

```go
package climodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

type Config struct {
	Binary  string
	Args    []string
	Timeout time.Duration
}

type Adapter struct {
	binary  string
	args    []string
	timeout time.Duration
}

// New validates the model configuration at construction, so an unusable
// configuration is a startup error rather than a per-tick error.
func New(config Config) (*Adapter, error) {
	if config.Binary == "" {
		return nil, errors.New("model binary is required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("model timeout must be positive")
	}
	if _, err := exec.LookPath(config.Binary); err != nil {
		return nil, fmt.Errorf("model binary %q is not usable: %w", config.Binary, err)
	}
	return &Adapter{binary: config.Binary, args: config.Args, timeout: config.Timeout}, nil
}

func (a *Adapter) Classify(ctx context.Context, input ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return ghtriage.Stage2Output{}, fmt.Errorf("encode stage 2 input: %w", err)
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return ghtriage.Stage2Output{}, err
	}
	return ghtriage.ParseStage2Output(raw)
}

func (a *Adapter) Review(ctx context.Context, input ghtriage.ReviewInput) (bool, error) {
	prompt, err := json.Marshal(input)
	if err != nil {
		return false, fmt.Errorf("encode review input: %w", err)
	}
	raw, err := a.run(ctx, prompt)
	if err != nil {
		return false, err
	}
	return ghtriage.ParseReviewVerdict(raw)
}

func (a *Adapter) run(ctx context.Context, prompt []byte) ([]byte, error) {
	if a == nil {
		return nil, errors.New("model adapter is not configured")
	}
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, a.binary, a.args...)
	cmd.Stdin = bytes.NewReader(prompt)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("model invocation failed: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}
```

- [ ] **Step 5: Run the adapter test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/... -count=1`
Expected: PASS.

- [ ] **Step 6: Write the fake**

Create `internal/ghtriage/fakemodel/fake.go`:

```go
package fakemodel

import (
	"context"
	"errors"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// Fake is the deterministic test double for both model interfaces. It records
// every input it received, so a test can assert on the exact prepared document
// and not only on the resulting disposition.
type Fake struct {
	Scripted       []ghtriage.Stage2Output
	ScriptedReview []bool
	ClassifyInputs []ghtriage.Stage2Input
	ReviewInputs   []ghtriage.ReviewInput
	Err            error

	classifyCalls int
	reviewCalls   int
}

func New() *Fake { return &Fake{} }

func (f *Fake) Classify(ctx context.Context, input ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	if err := ctx.Err(); err != nil {
		return ghtriage.Stage2Output{}, err
	}
	f.ClassifyInputs = append(f.ClassifyInputs, input)
	if f.Err != nil {
		return ghtriage.Stage2Output{}, f.Err
	}
	if f.classifyCalls >= len(f.Scripted) {
		return ghtriage.Stage2Output{}, errors.New("fakemodel: no scripted classifier response left")
	}
	out := f.Scripted[f.classifyCalls]
	f.classifyCalls++
	return out, nil
}

func (f *Fake) Review(ctx context.Context, input ghtriage.ReviewInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	f.ReviewInputs = append(f.ReviewInputs, input)
	if f.Err != nil {
		return false, f.Err
	}
	if f.reviewCalls >= len(f.ScriptedReview) {
		return false, errors.New("fakemodel: no scripted reviewer response left")
	}
	plausible := f.ScriptedReview[f.reviewCalls]
	f.reviewCalls++
	return plausible, nil
}

func (f *Fake) ClassifyCallCount() int { return f.classifyCalls }
func (f *Fake) ReviewCallCount() int   { return f.reviewCalls }
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/... -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
gofmt -l internal/ghtriage
git add internal/ghtriage
git commit -m "feat(ghtriage): add the model interfaces, CLI adapter and fake

The repository has no model client, so the production adapter follows the
existing executors and execs the configured CLI. The configuration is
validated in the constructor, which makes an unusable model a startup error
rather than a per-tick failure. A response must be exactly one JSON
object: a second object, trailing prose, an unknown field or a missing
rationale is rejected rather than partially interpreted, and the reviewer
verdict must carry plausible and nothing else."
```

---

### Task 4: The triage executor

**Files:**
- Create: `internal/ghtriage/executor.go`
- Test: `internal/ghtriage/executor_test.go`

**Interfaces:**
- Consumes: `Stage1`, `BuildStage2Input`, `Stage3`, `Decision`, `ParseStage2Output`, `ClassifierModel` from Tasks 1-3; `evidence.Store`.
- Produces: `ExecutorKind`, `TaskClass`, `RequiredCapability`, `DecisionMediaType`; `NewExecutor(*evidence.Store, ClassifierModel) *Executor`; `(*Executor).Start(ctx, executors.AttemptEnvelope) (executors.ExecutionResult, error)`.

- [ ] **Step 1: Write the failing executor test**

Create `internal/ghtriage/executor_test.go`:

```go
package ghtriage_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/fakemodel"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func putSnapshot(t *testing.T, store *evidence.Store, snap ghtriage.Snapshot) domain.ID {
	t.Helper()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: "application/json", Kind: "github.issue.snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	return object.ID
}

func envelopeFor(snapshotID domain.ID) executors.AttemptEnvelope {
	payload, _ := json.Marshal(map[string]any{
		"repository": "o/r", "issue": 42,
		"revision": "2026-09-28T10:00:00Z", "issueSnapshot": snapshotID,
	})
	return executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON:        payload,
		AcceptanceCriteria: []string{"triage decision recorded for 2026-09-28T10:00:00Z"},
	}
}

func newExecutorFixture(t *testing.T) (*ghtriage.Executor, *fakemodel.Fake, *evidence.Store) {
	t.Helper()
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	evidenceStore, err := evidence.New(store, t.TempDir(), clk)
	if err != nil {
		t.Fatal(err)
	}
	model := fakemodel.New()
	return ghtriage.NewExecutor(evidenceStore, model), model, evidenceStore
}

func TestExecutorResolvesADuplicateWithoutCallingTheModel(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"duplicate-of:41"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if model.ClassifyCallCount() != 0 {
		t.Fatalf("the model was consulted %d times on a decisive stage 1", model.ClassifyCallCount())
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want exactly one decision", len(result.Evidence))
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.FinalDisposition() != ghtriage.DispositionDuplicate {
		t.Fatalf("disposition = %q", decision.FinalDisposition())
	}
	if decision.Stage2 != nil || decision.Stage3 != nil {
		t.Fatal("a decisive stage 1 produced later stages")
	}
}

func TestExecutorRecordsAStage3DecisionWithTheExactPreparedInput(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{{
		IsActionable: true, Scope: ghtriage.ScopeSmall,
		SuggestedType: ghtriage.SuggestedTypeBug, Rationale: "enough detail to plan",
	}}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	var decision ghtriage.Decision
	if err := json.Unmarshal([]byte(result.Evidence[0].Content), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Stage3 == nil || decision.Stage3.Disposition != ghtriage.DispositionReadyToPlan {
		t.Fatalf("stage 3 = %+v", decision.Stage3)
	}
	if model.ClassifyCallCount() != 1 {
		t.Fatalf("classifier calls = %d, want 1", model.ClassifyCallCount())
	}
	if got := model.ClassifyInputs[0].Question; got != ghtriage.Stage2Question {
		t.Fatalf("question = %q, want the fixed literal", got)
	}
	if got := model.ClassifyInputs[0].Schema; got != ghtriage.Stage2InputSchema {
		t.Fatalf("input schema = %q", got)
	}
}

func TestExecutorFailsTheAttemptWhenBothResponsesAreInvalid(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{
		{Scope: "enormous", Rationale: "x"},
		{Scope: "enormous", Rationale: "x"},
	}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("two invalid responses did not fail the attempt")
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("a failed attempt fabricated %d evidence documents", len(result.Evidence))
	}
	if model.ClassifyCallCount() != 2 {
		t.Fatalf("classifier calls = %d, want exactly one retry", model.ClassifyCallCount())
	}
}

func TestExecutorSucceedsOnTheRetryWhenTheSecondResponseConforms(t *testing.T) {
	exec, model, evidenceStore := newExecutorFixture(t)
	model.Scripted = []ghtriage.Stage2Output{
		{Scope: "enormous", Rationale: "x"},
		{IsActionable: true, Scope: ghtriage.ScopeSmall, Rationale: "enough detail"},
	}
	snap := ghtriage.Snapshot{
		Repo: "o/r", Issue: 42, Title: "Crash", Body: "boom",
		Triage: "bug", Labels: []string{"bug"}, UpdatedAt: "2026-09-28T10:00:00Z",
	}
	envelope := envelopeFor(putSnapshot(t, evidenceStore, snap))

	result, err := exec.Start(context.Background(), envelope)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 {
		t.Fatalf("evidence = %d, want one decision", len(result.Evidence))
	}
	if len(model.ClassifyInputs) != 2 {
		t.Fatalf("classifier calls = %d, want 2", len(model.ClassifyInputs))
	}
	if model.ClassifyInputs[0] != model.ClassifyInputs[1] {
		t.Fatal("the retry changed the prepared input; it must be identical")
	}
}

func TestExecutorFailsWhenTheSnapshotEvidenceIsMissing(t *testing.T) {
	exec, _, _ := newExecutorFixture(t)
	envelope := envelopeFor(domain.NewID("absent"))
	if _, err := exec.Start(context.Background(), envelope); err == nil {
		t.Fatal("a missing snapshot evidence was accepted")
	}
}

func TestExecutorFailsOnAnIncompletePayload(t *testing.T) {
	exec, _, _ := newExecutorFixture(t)
	envelope := executors.AttemptEnvelope{
		TaskID: domain.NewID("task"), AttemptID: domain.NewID("attempt"),
		PayloadJSON: []byte(`{"repository":"o/r","issue":42,"revision":"r"}`),
	}
	_, err := exec.Start(context.Background(), envelope)
	if err == nil {
		t.Fatal("a payload without a snapshot evidence id was accepted")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("error %q does not name the missing snapshot", err)
	}
}

type erroringModel struct{ err error }

func (m erroringModel) Classify(context.Context, ghtriage.Stage2Input) (ghtriage.Stage2Output, error) {
	return ghtriage.Stage2Output{}, m.err
}

var _ = errors.New
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestExecutor -count=1`
Expected: FAIL, `undefined: ghtriage.NewExecutor`.

- [ ] **Step 3: Write the executor**

Create `internal/ghtriage/executor.go`:

```go
package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
)

const (
	ExecutorKind       = "github-issue-triage"
	TaskClass          = "github.issue.triage"
	RequiredCapability = "github.issue.read"
	DecisionMediaType  = "application/json"

	// stage2Attempts bounds the classifier to one retry inside a single lease.
	// An unbounded retry turns one poisoned issue into an infinite budget burn.
	stage2Attempts = 2
)

type triagePayload struct {
	Repository         string     `json:"repository"`
	Issue              int64      `json:"issue"`
	Revision           string     `json:"revision"`
	SnapshotEvidenceID domain.ID  `json:"issueSnapshot"`
}

type Executor struct {
	evidence *evidence.Store
	model    ClassifierModel
}

func NewExecutor(store *evidence.Store, model ClassifierModel) *Executor {
	return &Executor{evidence: store, model: model}
}

// Start runs the three deciding stages inside one lease and returns exactly
// one decision document. A stage 1 that resolves the issue never consults the
// model. A model response that does not validate is retried once with the same
// prepared input, and a second failure fails the attempt without fabricating a
// decision, because the worker treats any returning executor's evidence as a
// completed attempt.
func (e *Executor) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	if e == nil || e.evidence == nil || e.model == nil {
		return executors.ExecutionResult{}, errors.New("triage executor is not configured")
	}
	payload, err := decodePayload(envelope.PayloadJSON)
	if err != nil {
		return executors.ExecutionResult{}, err
	}
	snap, err := e.loadSnapshot(ctx, payload.SnapshotEvidenceID)
	if err != nil {
		return executors.ExecutionResult{}, err
	}

	decision := Decision{
		Schema:                  DecisionSchema,
		Repository:              payload.Repository,
		Issue:                   payload.Issue,
		Revision:                payload.Revision,
		SnapshotEvidenceID:      string(payload.SnapshotEvidenceID),
		TriageRulesVersion:      TriageRulesVersion,
		DispositionRulesVersion: DispositionRulesVersion,
		Stage1:                  Stage1(snap),
	}

	if !decision.Stage1.Resolved() {
		out, err := e.classify(ctx, BuildStage2Input(snap, decision.Stage1.Signals))
		if err != nil {
			return executors.ExecutionResult{}, err
		}
		stage3 := Stage3(out)
		decision.Stage2 = &out
		decision.Stage3 = &stage3
	}

	if err := decision.Validate(); err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("validate decision: %w", err)
	}
	raw, err := decision.Canonical()
	if err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("encode decision: %w", err)
	}
	return executors.ExecutionResult{
		Evidence: []executors.Evidence{{
			Kind:    executors.EvidenceKind(KindDecision),
			Content: string(raw),
		}},
		Stderr: fmt.Sprintf("triage %s#%d at %s: %s via %s\n",
			payload.Repository, payload.Issue, payload.Revision,
			decision.FinalDisposition(), decisionRule(decision)),
	}, nil
}

func (e *Executor) classify(ctx context.Context, input Stage2Input) (Stage2Output, error) {
	var lastErr error
	for attempt := 0; attempt < stage2Attempts; attempt++ {
		out, err := e.model.Classify(ctx, input)
		if err == nil {
			raw, marshalErr := json.Marshal(out)
			if marshalErr != nil {
				lastErr = marshalErr
				continue
			}
			parsed, parseErr := ParseStage2Output(raw)
			if parseErr == nil {
				return parsed, nil
			}
			lastErr = parseErr
			continue
		}
		lastErr = err
		if ctx.Err() != nil {
			return Stage2Output{}, ctx.Err()
		}
	}
	return Stage2Output{}, fmt.Errorf(
		"stage 2 produced no conforming response after %d attempts: %w", stage2Attempts, lastErr)
}

func (e *Executor) loadSnapshot(ctx context.Context, id domain.ID) (Snapshot, error) {
	_, raw, err := e.evidence.Get(ctx, id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load issue snapshot %s: %w", id, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode issue snapshot %s: %w", id, err)
	}
	if snap.Repo == "" || snap.Issue <= 0 {
		return Snapshot{}, fmt.Errorf("evidence %s is not a canonical issue snapshot", id)
	}
	return snap, nil
}

func decodePayload(raw json.RawMessage) (triagePayload, error) {
	var payload triagePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return triagePayload{}, fmt.Errorf("decode triage task payload: %w", err)
	}
	if strings.TrimSpace(payload.Repository) == "" || payload.Issue <= 0 {
		return triagePayload{}, errors.New("triage task payload lacks a repository or issue number")
	}
	if strings.TrimSpace(payload.Revision) == "" {
		return triagePayload{}, errors.New("triage task payload lacks a revision")
	}
	if strings.TrimSpace(string(payload.SnapshotEvidenceID)) == "" {
		return triagePayload{}, errors.New("triage task payload lacks the issue snapshot evidence id")
	}
	return payload, nil
}

func decisionRule(d Decision) string {
	if d.Stage1.Resolved() {
		return d.Stage1.Rule
	}
	if d.Stage3 != nil {
		return d.Stage3.Rule
	}
	return ""
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/ghtriage
git add internal/ghtriage
git commit -m "feat(ghtriage): add the triage executor

One lease produces exactly one decision document. A stage 1 that resolves
the issue never reaches the model, and a response that does not validate
is retried once with the identical prepared input before the attempt
fails. A failed attempt returns no evidence at all, because the worker
treats a returning executor's evidence as a completed attempt and a
fabricated decision would be indistinguishable from a real one."
```

---

### Task 5: Make the registered Tasks leasable by the right executor

This task carries two independent defects. Without it the already-registered Tasks stay `ELIGIBLE` forever, or are leased by the wrong executor.

**Files:**
- Create: `internal/scheduler/routing.go`
- Modify: `internal/scheduler/service.go` — `Service` struct (~line 40), `New` (~line 50), `ChooseExecutor` (~line 112)
- Test: `internal/scheduler/routing_test.go`
- Modify: `internal/runtime/box.go` — `Config` struct (~line 41), preference construction (~line 199)
- Modify: `cmd/summa42-box/main.go` — constants (~line 40), `workerCapacity` (~line 518), the `run-worker` composition

**Interfaces:**
- Consumes: `ghtriage.ExecutorKind`, `ghtriage.RequiredCapability` from Task 4.
- Produces: `scheduler.TaskClassRouting map[string]string` implementing `ExecutorPreference`; `scheduler.New(..., preferences ...ExecutorPreference)` honouring every preference in order; `Config.TaskClassRouting map[string]string` on the runtime box; `workerCapacity` advertising `github.issue.read` when `github-issue-triage` is registered.

- [ ] **Step 1: Write the failing routing test**

Create `internal/scheduler/routing_test.go`:

```go
package scheduler_test

import (
	"context"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/scheduler"
)

func TestTaskClassRoutingSelectsTheMappedExecutor(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"},
		[]string{"copilot", "github-issue-triage"})
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "github-issue-triage" {
		t.Fatalf("routing returned (%q, %v), want (github-issue-triage, true)", got, found)
	}
}

func TestTaskClassRoutingDeclinesAnUnmappedClass(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "shell"}, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("an unmapped task class was routed to %q", got)
	}
}

func TestTaskClassRoutingDeclinesWhenTheKindIsNotEligible(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "github-issue-triage"}

	got, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"copilot"})
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("an ineligible executor kind was selected: %q", got)
	}
}

func TestTaskClassRoutingDeclinesAnEmptyMapping(t *testing.T) {
	routing := scheduler.TaskClassRouting{"github.issue.triage": "  "}

	found, err := foundFor(routing)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a blank executor kind was selected")
	}
}

func foundFor(routing scheduler.TaskClassRouting) (bool, error) {
	_, found, err := routing.PreferredExecutor(
		context.Background(), domain.Task{TaskClass: "github.issue.triage"}, []string{"  "})
	return found, err
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler -run TestTaskClassRouting -count=1`
Expected: FAIL, `undefined: scheduler.TaskClassRouting`.

- [ ] **Step 3: Write the routing preference**

Create `internal/scheduler/routing.go`:

```go
package scheduler

import (
	"context"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// TaskClassRouting maps a task class to the executor kind that must handle it.
// It exists because ChooseExecutor does not inspect the task class: without an
// explicit mapping the alphabetical baseline would hand a triage Task to
// whichever registered kind happens to sort first.
type TaskClassRouting map[string]string

func (r TaskClassRouting) PreferredExecutor(ctx context.Context, task domain.Task, eligible []string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	kind, mapped := r[strings.TrimSpace(task.TaskClass)]
	if !mapped {
		return "", false, nil
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", false, nil
	}
	for _, candidate := range eligible {
		if strings.TrimSpace(candidate) == kind {
			return kind, true, nil
		}
	}
	return "", false, nil
}
```

- [ ] **Step 4: Make the preference slot an ordered chain**

In `internal/scheduler/service.go`, replace the `Service` struct and `New`:

```go
type Service struct {
	store         *state.Store
	clock         clock.Clock
	purpose       *purpose.Service
	execution     *execution.Service
	resources     *resources.Service
	leaseDuration time.Duration
	preferences   []ExecutorPreference
}

func New(store *state.Store, clk clock.Clock, purposes *purpose.Service, executionSvc *execution.Service, resourceSvc *resources.Service, leaseDuration time.Duration, preferences ...ExecutorPreference) *Service {
	return &Service{
		store: store, clock: clk, purpose: purposes, execution: executionSvc,
		resources: resourceSvc, leaseDuration: leaseDuration, preferences: preferences,
	}
}
```

In `ChooseExecutor`, replace the single-preference block — the lines from `if s.preference == nil {` through the eligibility check on the learned preference — with:

```go
	baseline := eligible[0]
	for _, preference := range s.preferences {
		if preference == nil {
			continue
		}
		preferred, found, err := preference.PreferredExecutor(ctx, task, append([]string(nil), eligible...))
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		preferred = strings.TrimSpace(preferred)
		if _, ok := set[preferred]; !ok {
			return "", fmt.Errorf("learned preference returned ineligible executor %q", preferred)
		}
		return preferred, nil
	}
	return baseline, nil
}
```

- [ ] **Step 5: Run the scheduler tests to verify they pass**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler/... -count=1`
Expected: PASS, including `TestNextClaimsGitHubIssueTriageWithEnforcedReadCapability` and `TestNextNeverClaimsGitHubIssueTriageWithoutReadCapability`.

- [ ] **Step 6: Wire the routing into the box**

In `internal/runtime/box.go`, add the field to `Config` directly after `ExecutorPreference`:

```go
	Executors           map[string]executors.Executor
	ExecutorPreference  scheduler.ExecutorPreference
	TaskClassRouting    map[string]string
```

Replace the preference construction in `internal/runtime/box.go` — the lines that assign `preference` and call `scheduler.New` — with:

```go
	preferences := make([]scheduler.ExecutorPreference, 0, 2)
	if len(cfg.TaskClassRouting) > 0 {
		preferences = append(preferences, scheduler.TaskClassRouting(cfg.TaskClassRouting))
	}
	if cfg.ExecutorPreference != nil {
		preferences = append(preferences, cfg.ExecutorPreference)
	} else {
		preferences = append(preferences, experienceSvc)
	}
	schedulerSvc = scheduler.New(store, cfg.Clock, purposes, executionSvc, resourceSvc, cfg.LeaseDuration, preferences...)
```

- [ ] **Step 7: Advertise the capability**

In `cmd/summa42-box/main.go`, add the constants next to the existing publish executor kind constants:

```go
	triageExecutorKind  = "github-issue-triage"
	triageReadCapability = "github.issue.read"
```

In `workerCapacity`, add this block after the existing `ado-publish` block and before the `if len(caps) == 0` check:

```go
	if _, registered := caps[triageExecutorKind]; registered {
		caps[triageReadCapability] = scheduler.CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
	}
```

- [ ] **Step 8: Register the executor in the `run-worker` composition**

In `cmd/summa42-box/main.go`, add a constructor for the triage executor and register it in the `run-worker` path. Add the imports `"github.com/SofiaFlux/summa42/internal/ghtriage"` and `"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"`.

Build the model configuration from the box flags, reading the binary name from `--model-binary` (default `codex`) and the timeout from `--model-timeout` (default `60s`), then validate it through `climodel.New` so an unusable model fails at startup:

```go
func triageModelConfig(flags *flag.FlagSet) (climodel.Config, error) {
	binary := flags.Lookup("model-binary").Value.String()
	timeout := flags.Lookup("model-timeout").Value.String()
	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return climodel.Config{}, fmt.Errorf("parse --model-timeout: %w", err)
	}
	return climodel.Config{Binary: binary, Timeout: duration}, nil
}
```

Register the two new flags in the same block where the other `run-worker` flags are declared, so both `run-gh-triage-driver` and `run-gh-triage-review` can read them.

In the `run-worker` composition, after the evidence store is constructed and before the scheduler is created, add:

```go
	classifier, err := climodel.New(modelConfig)
	if err != nil {
		return nil, err
	}
	if cfg.Executors == nil {
		cfg.Executors = map[string]executors.Executor{}
	}
	cfg.Executors[triageExecutorKind] = ghtriage.NewExecutor(evidenceStore, classifier)
	cfg.TaskClassRouting = map[string]string{ghtriage.TaskClass: triageExecutorKind}
```

The driver and review subcommands read the same `modelConfig` so all three processes agree on the model, and the reviewer's `climodel.Adapter` is built from it directly.

- [ ] **Step 9: Run the build and the affected tests**

Run: `GOCACHE=/tmp/summa42-full-go-cache go build ./cmd/summa42 ./cmd/summa42-box && GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler/... ./internal/runtime/... -count=1`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/scheduler internal/runtime cmd/summa42-box
git add internal/scheduler internal/runtime cmd/summa42-box
git commit -m "feat: make triage Tasks leasable and route them to the triage executor

Two independent defects kept the registered triage Tasks inert. The
advertised capability set is keyed by executor kind alone, so registering
a triage executor advertised that kind and not github.issue.read, which
is what the Tasks require, leaving them ELIGIBLE and unclaimed forever.
workerCapacity now carries an explicit special case for them, mirroring
the one that already exists for ado-publish.

ChooseExecutor never inspects the task class and falls back to the
alphabetically first registered kind, so a real run-worker would have
handed the triage Task to copilot. The preference slot is now an ordered
chain with a task-class routing map consulted first, declining cleanly
for every class it does not map so the existing experience preference
still applies to everything else."
```

---

### Task 6: A state-guarded challenge

Supersession must stop a pending triage task and must never turn an already-accepted one back into a challenged one. `ChallengeTask` today updates the task state with no predicate.

**Files:**
- Modify: `internal/execution/service.go` — add a method after `ChallengeTask` (~line 440)
- Test: `internal/execution/challenge_guarded_test.go`

**Interfaces:**
- Consumes: `domain.TaskState`, `domain.ChallengeScope`, `domain.LeaseRevoked`, `domain.AttemptCancelled`, `domain.TaskChallenged`, `formatTime`, `appendEvent`.
- Produces: `func (s *Service) ChallengeTaskIfInStates(ctx, taskID domain.ID, states []domain.TaskState, scope domain.ChallengeScope, reason string, evidenceIDs []domain.ID) (bool, error)`. Returns `changed == false, err == nil` when the task is not in one of the requested states.

- [ ] **Step 1: Write the failing test**

Create `internal/execution/challenge_guarded_test.go`:

```go
package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func newTriageTask(t *testing.T, store interface {
	DB() *sql.DB
}, svc *execution.Service, ctx context.Context, key string) domain.Task {
	t.Helper()
	envelope := domain.NewID("envelope")
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, ?, ?)`,
		envelope, 100, "2026-09-28T10:00:00Z"); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CreateTask(ctx, execution.TaskRequest{
		Purpose:              domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner"},
		TaskClass:            "github.issue.triage",
		Objective:            "triage",
		RequiredCapabilities: []string{"github.issue.read"},
		RequiredEnforcement:  domain.EnforcementEnforced,
		AuthorityCeiling:     []string{"github.issue.read"},
		ResourceEnvelopeID:   envelope,
		IdempotencyKey:       key,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestChallengeTaskIfInStatesRefusesAnAcceptedTask(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-accepted")

	if _, err := store.DB().ExecContext(ctx,
		`UPDATE tasks SET state = ? WHERE task_id = ?`, domain.TaskSucceeded, task.ID); err != nil {
		t.Fatal(err)
	}

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("a SUCCEEDED task was challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskSucceeded {
		t.Fatalf("state = %q, want SUCCEEDED", stored.State)
	}
	var challenges int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM task_challenges WHERE task_id = ?`, task.ID).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 {
		t.Fatalf("challenge rows = %d, want 0", challenges)
	}
}

func TestChallengeTaskIfInStatesChallengesAnEligibleTask(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-eligible")

	changed, err := svc.ChallengeTaskIfInStates(ctx, task.ID,
		[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
		domain.ChallengeTask, "superseded-by:2026-09-28T11:00:00Z",
		[]domain.ID{domain.NewID("evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an ELIGIBLE task was not challenged")
	}
	stored, err := svc.Task(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.TaskChallenged {
		t.Fatalf("state = %q, want CHALLENGED", stored.State)
	}
}

func TestChallengeTaskIfInStatesRejectsAnEmptyStateList(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := execution.New(store, clk, purpose.New(store, clk))
	task := newTriageTask(t, store, svc, ctx, "work-no-states")

	if _, err := svc.ChallengeTaskIfInStates(ctx, task.ID, nil,
		domain.ChallengeTask, "superseded", nil); err == nil {
		t.Fatal("an empty state list was accepted")
	}
}
```

Add `"database/sql"` to the import block of that file, which the `newTriageTask` helper signature needs.

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/execution -run TestChallengeTaskIfInStates -count=1`
Expected: FAIL, `undefined: svc.ChallengeTaskIfInStates`.

- [ ] **Step 3: Write the guarded challenge**

Add to `internal/execution/service.go`, directly after `ChallengeTask`:

```go
// ChallengeTaskIfInStates challenges a task only when it is currently in one of
// the supplied states. The caller must re-read the task when changed is false,
// because the state may have moved since it was read. An accepted task can
// never be returned to CHALLENGED, which is the property the unguarded
// ChallengeTask lacks.
func (s *Service) ChallengeTaskIfInStates(ctx context.Context, taskID domain.ID, states []domain.TaskState, scope domain.ChallengeScope, reason string, evidenceIDs []domain.ID) (bool, error) {
	if err := s.configured(); err != nil {
		return false, err
	}
	taskID = domain.ID(strings.TrimSpace(string(taskID)))
	reason = strings.TrimSpace(reason)
	if taskID == "" || reason == "" || !validChallengeScope(scope) {
		return false, errors.New("task, valid challenge scope, and reason are required")
	}
	allowed := make([]string, 0, len(states))
	for _, state := range states {
		if trimmed := strings.TrimSpace(string(state)); trimmed != "" {
			allowed = append(allowed, trimmed)
		}
	}
	if len(allowed) == 0 {
		return false, errors.New("at least one task state is required")
	}
	evidenceJSON, err := json.Marshal(evidenceIDs)
	if err != nil {
		return false, err
	}
	now := s.clock.Now().UTC()

	return s.store.WithTx(ctx, func(tx *sql.Tx) (bool, error) {
		var currentAttempt sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT current_attempt_id FROM tasks WHERE task_id = ?`, taskID,
		).Scan(&currentAttempt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("task %q not found", taskID)
			}
			return false, err
		}

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(allowed)), ",")
		args := []any{domain.TaskChallenged, now, taskID}
		for _, state := range allowed {
			args = append(args, state)
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE tasks SET state = ?, updated_at = ? WHERE task_id = ? AND state IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return false, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if changed != 1 {
			return false, nil
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO task_challenges(challenge_id, task_id, scope, reason, evidence_ids_json, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			domain.NewID("challenge"), taskID, scope, reason, string(evidenceJSON), formatTime(now),
		); err != nil {
			return false, err
		}
		if currentAttempt.Valid {
			if _, err := tx.ExecContext(ctx,
				`UPDATE attempts SET lease_state = ?, state = ?, completed_at = COALESCE(completed_at, ?) WHERE attempt_id = ? AND lease_state = ?`,
				domain.LeaseRevoked, domain.AttemptCancelled, formatTime(now), currentAttempt.String, domain.LeaseActive,
			); err != nil {
				return false, err
			}
		}
		if err := appendEvent(ctx, tx, taskID, domain.ID(currentAttempt.String), "TASK_CHALLENGED", now); err != nil {
			return false, err
		}
		return true, nil
	})
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/execution/... -count=1`
Expected: PASS.

- [ ] **Step 5: Run the race detector on this package**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test -race ./internal/execution/... -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
gofmt -l internal/execution
git add internal/execution
git commit -m "feat(execution): add a state-guarded challenge

The existing ChallengeTask updates the task state with no state predicate,
so it would happily turn a SUCCEEDED task back into a CHALLENGED one.
Supersession needs to stop a task that has not been accepted yet and must
leave an accepted one alone, so the guard moves into the statement: the
caller gets changed=false and re-reads the task rather than writing on a
stale read, and no challenge row is written when nothing changed."
```

---

### Task 7: The driver

The driver is the largest task. It is split into two: the case read path it needs, then the driver itself.

**Files:**
- Modify: `internal/workflowcase/service.go` — add `ListByObject` and `ListBySource` after `ListAssessments` (~line 215)
- Test: `internal/workflowcase/listbyobject_test.go`
- Create: `internal/ghtriage/driver.go`
- Test: `internal/ghtriage/driver_test.go`
- Modify: `cmd/summa42-box/main.go` — add `run-gh-triage-driver`

**Interfaces:**
- Consumes: `Decision`, `KindDecision`, `KindFailure`, `KindAccepted`, `DecisionMediaType` from Tasks 1 and 4; `AcceptTask`, `CompleteAttempt` from `internal/verification`; `ChallengeTaskIfInStates` from Task 6; `Provenance` from `internal/runmanifest`.
- Produces: `ListByObject(ctx, missionID domain.ID, source, objectID string) ([]Case, error)`; `ListBySource(ctx, missionID domain.ID, source string) ([]Case, error)`; `NewDriver(...) *Driver`; `(*Driver).Tick(ctx, missionID) (DriverResult, error)`; `DriverResult`; `DriverVerifierID`; `DriverVerifierType`; `SourceGitHub`.

- [ ] **Step 1: Write the failing read-path test**

Create `internal/workflowcase/listbyobject_test.go`:

```go
package workflowcase_test

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func TestListByObjectReturnsEveryStateOfOneIssue(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	insert := func(mission, source, object, revision string) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx,
			`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id,
				observation_evidence_id, state, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'ACTIVE', ?, ?)`,
			domain.NewID("case"), mission, source, object, revision, domain.NewID("evidence"), now, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	insert("mission-1", "github", "o/r#42", "2026-09-28T09:00:00Z")
	insert("mission-1", "github", "o/r#42", "2026-09-28T10:00:00Z")
	insert("mission-1", "github", "o/r#99", "2026-09-28T10:00:00Z")

	cases, err := svc.ListByObject(ctx, "mission-1", "github", "o/r#42")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 2 {
		t.Fatalf("cases = %d, want both revisions of one object", len(cases))
	}
}

func TestListByObjectRejectsIncompleteArguments(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	if _, err := svc.ListByObject(ctx, "", "github", "o/r#42"); err == nil {
		t.Fatal("an empty mission was accepted")
	}
	if _, err := svc.ListByObject(ctx, "mission-1", "", "o/r#42"); err == nil {
		t.Fatal("an empty source was accepted")
	}
	if _, err := svc.ListByObject(ctx, "mission-1", "github", ""); err == nil {
		t.Fatal("an empty object id was accepted")
	}
}

func TestListBySourceSpansEveryState(t *testing.T) {
	ctx := context.Background()
	store := testutil.OpenStore(t)
	clk := testutil.NewClock(time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC))
	svc := workflowcase.New(store, clk, purpose.New(store, clk))

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO workflow_cases(case_id, mission_id, source, object_id, revision_id,
			observation_evidence_id, state, created_at, updated_at)
		 VALUES (?, 'mission-1', 'github', 'o/r#42', 'r1', ?, 'BLOCKED', ?, ?)`,
		domain.NewID("case"), domain.NewID("evidence"), now, now,
	); err != nil {
		t.Fatal(err)
	}

	cases, err := svc.ListBySource(ctx, "mission-1", "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("cases = %d, want the BLOCKED case too", len(cases))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/workflowcase -run 'TestListByObject|TestListBySource' -count=1`
Expected: FAIL, `undefined: svc.ListByObject`.

- [ ] **Step 3: Write the read paths**

Add to `internal/workflowcase/service.go`, after `ListAssessments`:

```go
// ListByObject returns every case of one object across all states. Supersession
// cannot be built on ListActive: once an assessment clears current_work_id the
// case is BLOCKED, so the accepted decision that identifies the newest revision
// disappears from an active-only scan and the driver forgets newer work after a
// restart.
func (s *Service) ListByObject(ctx context.Context, missionID domain.ID, source, objectID string) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	source = strings.TrimSpace(source)
	objectID = strings.TrimSpace(objectID)
	if missionID == "" || source == "" || objectID == "" {
		return nil, errors.New("mission, source, and object ID are required")
	}
	return s.listWhere(ctx,
		`mission_id = ? AND source = ? AND object_id = ?`, missionID, source, objectID)
}

// ListBySource returns every case of one source across all states, which the
// reviewer needs because a superseded case it must re-read is BLOCKED.
func (s *Service) ListBySource(ctx context.Context, missionID domain.ID, source string) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	source = strings.TrimSpace(source)
	if missionID == "" || source == "" {
		return nil, errors.New("mission and source are required")
	}
	return s.listWhere(ctx, `mission_id = ? AND source = ?`, missionID, source)
}

func (s *Service) listWhere(ctx context.Context, predicate string, args ...any) ([]Case, error) {
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT `+caseColumns+` FROM workflow_cases WHERE `+predicate+` ORDER BY revision_id, case_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow cases: %w", err)
	}
	defer rows.Close()
	cases := make([]Case, 0)
	for rows.Next() {
		c, _, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow case: %w", err)
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow cases: %w", err)
	}
	return cases, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/workflowcase -count=1`
Expected: PASS.

- [ ] **Step 5: Commit the read paths**

```bash
gofmt -l internal/workflowcase
git add internal/workflowcase
git commit -m "feat(workflowcase): read cases by object and by source across all states

Neither scan can be built on ListActive. Once an assessment clears
current_work_id the case is BLOCKED, so the accepted decision that
identifies the newest revision disappears from an active-only scan, and
the driver would forget newer work after a restart. Supersession needs the
blocked history and the reviewer needs superseded cases."
```

- [ ] **Step 6: Write the failing driver test**

Create `internal/ghtriage/driver_test.go`. The fixture registers a real case and a real `github.issue.triage` Task through `workflowcase.EnsureAndMaterialize` with a real snapshot evidence object, then holds the store, every service, the clock, the mission, the case and the task. Build it the way `internal/ghissue/observe_test.go` builds its own fixture.

The fixture must expose these helpers, each implemented against the real services:

- `newDriverFixture(t) *driverFixture`
- `completeTaskWithDecision(t, revision string, decision Decision)` — stores the snapshot, leases one attempt, writes the decision evidence through `evidence.Store.Put`, then calls `verificationSvc.CompleteAttempt` with that evidence ID so the task reaches `AWAITING_VERIFICATION`
- `failTaskTwice(t, revision string)` — drives `executionSvc.FailAttempt` twice with the same signature so the task reaches `TaskBlocked`
- `registerRevision(t, revision string)` — calls `EnsureAndMaterialize` again for the same issue at another revision
- `caseState(t, revision)`, `taskState(t, revision)`
- `latestAssessmentReason(t, revision) string`

The tests:

```go
func TestDriverAcceptsThenBlocksNotActionable(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, "2026-09-28T10:00:00Z", notActionableDecision("2026-09-28T10:00:00Z"))

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.Assessed != 1 {
		t.Fatalf("result = %+v, want one acceptance and one assessment", result)
	}
	if got := f.caseState(t, "2026-09-28T10:00:00Z"); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
	if got := f.taskState(t, "2026-09-28T10:00:00Z"); got != "SUCCEEDED" {
		t.Fatalf("task state = %q, want SUCCEEDED", got)
	}
	if got := f.latestAssessmentReason(t, "2026-09-28T10:00:00Z"); got != "not-actionable" {
		t.Fatalf("assessment reason = %q, want not-actionable", got)
	}
}

func TestDriverAcceptsAndLeavesReadyToPlanActive(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, "2026-09-28T10:00:00Z", readyToPlanDecision("2026-09-28T10:00:00Z"))

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, "2026-09-28T10:00:00Z"); got != "ACTIVE" {
		t.Fatalf("case state = %q, want ACTIVE", got)
	}
	if got := f.taskState(t, "2026-09-28T10:00:00Z"); got != "SUCCEEDED" {
		t.Fatalf("task state = %q, want SUCCEEDED", got)
	}
}

func TestDriverIsIdempotentAcrossTicks(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, "2026-09-28T10:00:00Z", notActionableDecision("2026-09-28T10:00:00Z"))

	first, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Accepted != 1 || first.Assessed != 1 {
		t.Fatalf("first tick = %+v", first)
	}
	if second.Accepted != 0 || second.Assessed != 0 {
		t.Fatalf("second tick = %+v, want no repeated work", second)
	}
}

func TestDriverBlocksAnExhaustedTaskWithFailureEvidence(t *testing.T) {
	f := newDriverFixture(t)
	f.failTaskTwice(t, "2026-09-28T10:00:00Z")

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Blocked != 1 {
		t.Fatalf("result = %+v, want one blocked case", result)
	}
	if got := f.caseState(t, "2026-09-28T10:00:00Z"); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
	if got := f.latestAssessmentReason(t, "2026-09-28T10:00:00Z"); got != "triage-failed" {
		t.Fatalf("assessment reason = %q, want triage-failed", got)
	}
	if !f.hasEvidenceKind(t, ghtriage.KindFailure) {
		t.Fatal("no failure evidence was written")
	}
}

func TestDriverSupersedesAnOlderRevisionAndChallengesItsPendingTask(t *testing.T) {
	f := newDriverFixture(t)
	f.registerRevision(t, "2026-09-28T09:00:00Z")
	f.completeTaskWithDecision(t, "2026-09-28T10:00:00Z", readyToPlanDecision("2026-09-28T10:00:00Z"))

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Superseded != 1 {
		t.Fatalf("result = %+v, want one superseded revision", result)
	}
	if got := f.taskState(t, "2026-09-28T09:00:00Z"); got != "CHALLENGED" {
		t.Fatalf("older task state = %q, want CHALLENGED", got)
	}
	if got := f.caseState(t, "2026-09-28T09:00:00Z"); got != "BLOCKED" {
		t.Fatalf("older case state = %q, want BLOCKED", got)
	}
	if got := f.caseState(t, "2026-09-28T10:00:00Z"); got != "ACTIVE" {
		t.Fatalf("newer case state = %q, want ACTIVE", got)
	}
	if got := f.latestAssessmentReason(t, "2026-09-28T09:00:00Z"); got != "superseded-by:2026-09-28T10:00:00Z" {
		t.Fatalf("assessment reason = %q", got)
	}
}

func TestDriverComparesRevisionsAsTimesNotStrings(t *testing.T) {
	f := newDriverFixture(t)
	// 2026-09-28T09:00:00Z sorts after 2026-09-28T10:00:00Z as a string only
	// if the month digit differed; a lexicographic comparison would pick the
	// wrong newest revision here.
	f.registerRevision(t, "2026-09-29T09:00:00Z")
	f.registerRevision(t, "2026-10-01T09:00:00Z")
	f.completeTaskWithDecision(t, "2026-10-01T09:00:00Z", readyToPlanDecision("2026-10-01T09:00:00Z"))

	if _, err := f.driver.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.caseState(t, "2026-10-01T09:00:00Z"); got != "ACTIVE" {
		t.Fatalf("newest case state = %q, want ACTIVE", got)
	}
	if got := f.caseState(t, "2026-09-29T09:00:00Z"); got != "BLOCKED" {
		t.Fatalf("older case state = %q, want BLOCKED", got)
	}
}

func TestDriverRecoversWhenTheTaskIsAcceptedButTheCaseIsNot(t *testing.T) {
	f := newDriverFixture(t)
	f.completeTaskWithDecision(t, "2026-09-28T10:00:00Z", notActionableDecision("2026-09-28T10:00:00Z"))
	f.simulateRestartAfterAcceptance(t, "2026-09-28T10:00:00Z")

	result, err := f.driver.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Assessed != 1 {
		t.Fatalf("result = %+v, want the assessment completed on the later tick", result)
	}
	if got := f.caseState(t, "2026-09-28T10:00:00Z"); got != "BLOCKED" {
		t.Fatalf("case state = %q, want BLOCKED", got)
	}
}

func notActionableDecision(revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: 42, Revision: revision,
		SnapshotEvidenceID: "evidence-snapshot", TriageRulesVersion: ghtriage.TriageRulesVersion,
		DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: false, Scope: ghtriage.ScopeSmall, Rationale: "no defect described"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionNotActionable, Rule: "not-actionable"},
	}
}

func readyToPlanDecision(revision string) ghtriage.Decision {
	return ghtriage.Decision{
		Schema: ghtriage.DecisionSchema, Repository: "o/r", Issue: 42, Revision: revision,
		SnapshotEvidenceID: "evidence-snapshot", TriageRulesVersion: ghtriage.TriageRulesVersion,
		DispositionRulesVersion: ghtriage.DispositionRulesVersion,
		Stage1: ghtriage.Stage1Result{Triage: ghtriage.TriageBug, Signals: []string{"has-repro"}},
		Stage2: &ghtriage.Stage2Output{IsActionable: true, Scope: ghtriage.ScopeSmall, Rationale: "enough detail"},
		Stage3: &ghtriage.Stage3Result{Disposition: ghtriage.DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"},
	}
}
```

`simulateRestartAfterAcceptance` accepts the task through `verificationSvc.AcceptTask` itself and stops, leaving the case `ACTIVE`. That is exactly the crash window the driver must recover from, and it is the reason the driver keeps its own `github.issue.triage.accepted` evidence: the fixture can then delete nothing and rely on that record.

- [ ] **Step 7: Run the driver test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestDriver -count=1`
Expected: FAIL, `undefined: ghtriage.NewDriver`.

- [ ] **Step 8: Write the driver**

Create `internal/ghtriage/driver.go`:

```go
package ghtriage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const (
	DriverVerifierID   = domain.ID("gh-triage-driver")
	DriverVerifierType = "DETERMINISTIC"
	SourceGitHub       = "github"

	ReasonTriageFailed   = "triage-failed"
	supersededReasonHead = "superseded-by:"
)

type DriverResult struct {
	Accepted   int
	Assessed   int
	Blocked    int
	Superseded int
	Failures   []string
}

type Driver struct {
	cases        *workflowcase.Service
	execution    *execution.Service
	verification *verification.Service
	manifests    *runmanifest.Service
	evidence     *evidence.Store
	clock        clock.Clock
}

func NewDriver(cases *workflowcase.Service, exec *execution.Service, ver *verification.Service, manifests *runmanifest.Service, store *evidence.Store, clk clock.Clock) *Driver {
	return &Driver{
		cases: cases, execution: exec, verification: ver,
		manifests: manifests, evidence: store, clock: clk,
	}
}

type acceptedRevision struct {
	c           workflowcase.Case
	revision    time.Time
	disposition Disposition
	evidenceID  domain.ID
}

// Tick advances every GitHub case of the mission. It runs after the lease has
// completed, never inside the executor, so the case never moves before the
// successful attempt is recorded.
func (d *Driver) Tick(ctx context.Context, missionID domain.ID) (DriverResult, error) {
	if d == nil || d.cases == nil || d.execution == nil || d.verification == nil {
		return DriverResult{}, errors.New("triage driver is not configured")
	}
	var result DriverResult
	active, err := d.cases.ListActive(ctx, missionID)
	if err != nil {
		return result, err
	}
	objects := make([]string, 0, len(active))
	seen := make(map[string]struct{}, len(active))
	for _, c := range active {
		if c.Source != SourceGitHub {
			continue
		}
		if _, known := seen[c.ObjectID]; known {
			continue
		}
		seen[c.ObjectID] = struct{}{}
		objects = append(objects, c.ObjectID)
	}
	sort.Strings(objects)

	for _, object := range objects {
		if err := d.tickObject(ctx, missionID, object, &result); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("%s: %v", object, err))
		}
	}
	return result, nil
}

func (d *Driver) tickObject(ctx context.Context, missionID domain.ID, object string, result *DriverResult) error {
	cases, err := d.cases.ListByObject(ctx, missionID, SourceGitHub, object)
	if err != nil {
		return err
	}
	accepted, err := d.advanceAccepted(ctx, cases, result)
	if err != nil {
		return err
	}
	if len(accepted) == 0 {
		return nil
	}
	newest := accepted[0]
	for _, older := range accepted[1:] {
		if !older.revision.Before(newest.revision) {
			continue
		}
		if err := d.supersede(ctx, older, newest, result); err != nil {
			return err
		}
	}
	return nil
}

// advanceAccepted accepts the task of every completed revision and applies its
// disposition. Accepted revisions are returned newest first.
func (d *Driver) advanceAccepted(ctx context.Context, cases []workflowcase.Case, result *DriverResult) ([]acceptedRevision, error) {
	accepted := make([]acceptedRevision, 0, len(cases))
	for _, c := range cases {
		if c.CurrentWorkID == "" || c.State != workflowcase.Active {
			continue
		}
		task, found, err := d.execution.FindByIdempotencyKey(ctx, string(c.CurrentWorkID))
		if err != nil {
			return accepted, err
		}
		if !found {
			continue
		}
		switch task.State {
		case domain.TaskAwaitingVerification:
			decision, evidenceID, err := d.acceptDecision(ctx, c, task, result)
			if err != nil {
				return accepted, err
			}
			if decision == nil {
				continue
			}
			current, err := d.cases.Get(ctx, c.ID)
			if err != nil {
				return accepted, err
			}
			if err := d.applyDisposition(ctx, current, task.ID, *decision, evidenceID, result); err != nil {
				return accepted, err
			}
			if err := d.recordAccepted(ctx, current, *decision, evidenceID, result); err != nil {
				return accepted, err
			}
			accepted = append(accepted, acceptedRevision{
				c: current, revision: parseRevision(current.RevisionID),
				disposition: decision.FinalDisposition(), evidenceID: evidenceID,
			})
		case domain.TaskSucceeded:
			decision, evidenceID, err := d.recoverAccepted(ctx, c, task)
			if err != nil {
				return accepted, err
			}
			if decision == nil {
				continue
			}
			if err := d.applyDisposition(ctx, c, task.ID, *decision, evidenceID, result); err != nil {
				return accepted, err
			}
			accepted = append(accepted, acceptedRevision{
				c: c, revision: parseRevision(c.RevisionID),
				disposition: decision.FinalDisposition(), evidenceID: evidenceID,
			})
		case domain.TaskBlocked:
			if err := d.blockFailedTask(ctx, c, task, result); err != nil {
				return accepted, err
			}
		}
	}
	sort.SliceStable(accepted, func(i, j int) bool {
		return accepted[i].revision.After(accepted[j].revision)
	})
	return accepted, nil
}

// acceptDecision validates the single decision attached to the completed
// attempt, writes the restart index, and accepts the task. Evidence not
// attached to a completed attempt is not a decision, which is why this reads
// provenance rather than the evidence store directly.
func (d *Driver) acceptDecision(ctx context.Context, c workflowcase.Case, task domain.Task, result *DriverResult) (*Decision, domain.ID, error) {
	if task.CurrentAttemptID == "" {
		return nil, domain.ID(""), nil
	}
	provenance, err := d.manifests.Provenance(ctx, task.CurrentAttemptID)
	if err != nil {
		return nil, domain.ID(""), fmt.Errorf("read triage attempt provenance: %w", err)
	}
	decision, evidenceID, err := d.readDecision(ctx, provenance.OutputEvidence, c)
	if err != nil || decision == nil {
		return nil, domain.ID(""), err
	}
	if err := d.recordAccepted(ctx, c, *decision, evidenceID, result); err != nil {
		return nil, domain.ID(""), err
	}
	if _, err := d.verification.AcceptTask(ctx, task.ID, verification.AcceptanceRequest{
		VerifierID:   DriverVerifierID,
		VerifierType: DriverVerifierType,
		CriteriaMet:  true,
		EvidenceIDs:  []domain.ID{evidenceID},
	}); err != nil {
		return nil, domain.ID(""), fmt.Errorf("accept triage task %s: %w", task.ID, err)
	}
	result.Accepted++
	return decision, evidenceID, nil
}

// recoverAccepted replays a task the driver already accepted on an earlier
// tick. internal/verification has no read accessor for an acceptance record, so
// the driver's own accepted index is the replay path.
func (d *Driver) recoverAccepted(ctx context.Context, c workflowcase.Case, task domain.Task) (*Decision, domain.ID, error) {
	raw, found, err := d.findAccepted(ctx, c, task)
	if err != nil || !found {
		return nil, domain.ID(""), err
	}
	var record acceptedIndex
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted triage index: %w", err)
	}
	if record.Revision != c.RevisionID {
		return nil, domain.ID(""), fmt.Errorf(
			"accepted index revision %q does not match case revision %q", record.Revision, c.RevisionID)
	}
	object, decisionRaw, err := d.evidence.Get(ctx, record.DecisionEvidenceID)
	if err != nil {
		return nil, domain.ID(""), err
	}
	if object.Kind != KindDecision {
		return nil, domain.ID(""), fmt.Errorf("accepted index points at evidence of kind %q", object.Kind)
	}
	var decision Decision
	if err := json.Unmarshal(decisionRaw, &decision); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted decision: %w", err)
	}
	return &decision, record.DecisionEvidenceID, nil
}

type acceptedIndex struct {
	Schema            string    `json:"schema"`
	CaseID            domain.ID `json:"case_id"`
	TaskID            domain.ID `json:"task_id"`
	Revision          string    `json:"revision"`
	DecisionEvidenceID domain.ID `json:"decision_evidence_id"`
}

// recordAccepted writes the driver's restart index before AcceptTask, so a
// crash between the two leaves a recoverable record. Content hashing dedupes
// it across ticks.
func (d *Driver) recordAccepted(ctx context.Context, c workflowcase.Case, decision Decision, evidenceID domain.ID, result *DriverResult) error {
	record := acceptedIndex{
		Schema:            "github.issue.triage.accepted.v1",
		CaseID:            c.ID,
		TaskID:            c.CurrentWorkID,
		Revision:          c.RevisionID,
		DecisionEvidenceID: evidenceID,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, _, err = d.putOnce(ctx, raw, KindAccepted)
	return err
}

// findAccepted locates the driver's own accepted index for a case. The evidence
// store has no case column and FindByContentHash needs a hash that is unknown
// before the record exists, so the lookup is a bounded scan of the kind.
func (d *Driver) findAccepted(ctx context.Context, c workflowcase.Case, task domain.Task) ([]byte, bool, error) {
	if d.evidence == nil {
		return nil, false, errors.New("evidence store is not configured")
	}
	var record acceptedIndex
	_, raw, found, err := d.evidence.FindLatestByKind(ctx, KindAccepted, &record, acceptedIndexScanLimit)
	if err != nil || !found {
		return nil, false, err
	}
	if record.CaseID != c.ID || record.TaskID != task.ID {
		return nil, false, nil
	}
	return raw, true, nil
}

func (d *Driver) readDecision(ctx context.Context, refs []runmanifest.EvidenceRef, c workflowcase.Case) (*Decision, domain.ID, error) {
	var found *Decision
	var foundID domain.ID
	for _, ref := range refs {
		object, raw, err := d.evidence.Get(ctx, ref.ID)
		if err != nil {
			return nil, domain.ID(""), err
		}
		if object.Kind != KindDecision {
			continue
		}
		if found != nil {
			return nil, domain.ID(""), fmt.Errorf("attempt carries more than one %s", KindDecision)
		}
		var decision Decision
		if err := json.Unmarshal(raw, &decision); err != nil {
			return nil, domain.ID(""), fmt.Errorf("decode decision %s: %w", ref.ID, err)
		}
		found = &decision
		foundID = ref.ID
	}
	if found == nil {
		return nil, domain.ID(""), nil
	}
	if err := found.Validate(); err != nil {
		return nil, domain.ID(""), fmt.Errorf("stored decision is invalid: %w", err)
	}
	if found.Revision != c.RevisionID {
		return nil, domain.ID(""), fmt.Errorf(
			"decision revision %q does not match case revision %q", found.Revision, c.RevisionID)
	}
	if found.SnapshotEvidenceID == "" {
		return nil, domain.ID(""), errors.New("decision carries no snapshot evidence id")
	}
	return found, foundID, nil
}

func (d *Driver) applyDisposition(ctx context.Context, c workflowcase.Case, taskID domain.ID, decision Decision, evidenceID domain.ID, result *DriverResult) error {
	disposition := decision.FinalDisposition()
	if disposition == DispositionReadyToPlan {
		return nil
	}
	if err := d.block(ctx, c, taskID, string(disposition), evidenceID); err != nil {
		return err
	}
	result.Assessed++
	return nil
}

// block moves an ACTIVE case to BLOCKED through Assess, which is idempotent by
// exact-request replay and already records the reason in the audit row. No new
// column on workflow_cases is required.
func (d *Driver) block(ctx context.Context, c workflowcase.Case, taskID domain.ID, reason string, evidenceID domain.ID) error {
	if c.State != workflowcase.Active || c.CurrentWorkID != taskID {
		return nil
	}
	_, err := d.cases.Assess(ctx, workflowcase.AssessmentRequest{
		CaseID: c.ID,
		WorkID: taskID,
		Assessment: workflow.Assessment{
			Verdict:     workflow.Unknown,
			Reason:      reason,
			EvidenceIDs: []string{string(evidenceID)},
		},
		RemainingBudget: c.RemainingBudget,
	})
	return err
}

// blockFailedTask handles a task that exhausted its retries and therefore has
// no decision. workflow.Decide requires non-empty evidence, so the failure
// document is what makes this transition possible at all, and it stops an
// untriaged case from looking like work to the future planner.
func (d *Driver) blockFailedTask(ctx context.Context, c workflowcase.Case, task domain.Task, result *DriverResult) error {
	raw, err := json.Marshal(map[string]any{
		"schema":  "github.issue.triage.failure.v1",
		"case_id": string(c.ID),
		"task_id": string(task.ID),
		"attempt": string(task.CurrentAttemptID),
		"state":   string(task.State),
		"reason":  ReasonTriageFailed,
	})
	if err != nil {
		return err
	}
	_, evidenceID, err := d.putOnce(ctx, raw, KindFailure)
	if err != nil {
		return err
	}
	result.Failures = append(result.Failures, fmt.Sprintf(
		"triage task %s exhausted its retries and was blocked", task.ID))
	if err := d.block(ctx, c, task.ID, ReasonTriageFailed, evidenceID); err != nil {
		return err
	}
	result.Blocked++
	return nil
}

// supersede stops an older revision's pending work and blocks its case. The
// challenge runs before the assessment, so a crash between them leaves an
// inert task and an ACTIVE case the next tick can finish.
func (d *Driver) supersede(ctx context.Context, older, newest acceptedRevision, result *DriverResult) error {
	if older.c.State != workflowcase.Active {
		return nil
	}
	reason := supersededReasonHead + newest.c.RevisionID
	taskID := older.c.CurrentWorkID
	task, found, err := d.execution.FindByIdempotencyKey(ctx, string(taskID))
	if err != nil {
		return err
	}
	if found {
		switch task.State {
		case domain.TaskEligible, domain.TaskExecuting:
			if _, err := d.execution.ChallengeTaskIfInStates(ctx, task.ID,
				[]domain.TaskState{domain.TaskEligible, domain.TaskExecuting},
				domain.ChallengeTask, reason, []domain.ID{newest.evidenceID}); err != nil {
				return err
			}
			// The task may have moved under a racing lease, so follow the row
			// that is now current instead of assuming the challenge applied.
			if reloaded, stillFound, err := d.execution.FindByIdempotencyKey(ctx, string(taskID)); err != nil {
				return err
			} else if stillFound {
				task = reloaded
			}
		case domain.TaskAwaitingVerification:
			decision, evidenceID, err := d.acceptDecision(ctx, older.c, task, result)
			if err != nil {
				return err
			}
			if decision != nil {
				if err := d.applyDisposition(ctx, older.c, task.ID, *decision, evidenceID, result); err != nil {
					return err
				}
			}
		}
	}
	current, err := d.cases.Get(ctx, older.c.ID)
	if err != nil {
		return err
	}
	if current.State != workflowcase.Active {
		return nil
	}
	if err := d.block(ctx, current, current.CurrentWorkID, reason, newest.evidenceID); err != nil {
		return err
	}
	result.Superseded++
	return nil
}

// putOnce stores a document and returns its evidence ID, reusing an existing
// object when the identical bytes are already stored. The lookup is by content
// hash of these bytes, which are a deterministic function of their inputs.
func (d *Driver) putOnce(ctx context.Context, raw []byte, kind string) (evidence.EvidenceObject, domain.ID, error) {
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if existing, found, err := d.evidence.FindByContentHash(ctx, hash, kind); err != nil {
		return evidence.EvidenceObject{}, domain.ID(""), err
	} else if found {
		return existing, existing.ID, nil
	}
	object, err := d.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: DecisionMediaType,
		Kind:      kind,
	})
	if err != nil {
		return evidence.EvidenceObject{}, domain.ID(""), err
	}
	return object, object.ID, nil
}

func parseRevision(revision string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(revision))
	if err != nil {
		return time.Time{}
	}
	return parsed
}
```

The driver needs to find its own accepted index by case, and neither existing store method can do that: `FindByContentHash` requires a hash that is unknown before the record exists, and `evidence_objects` has no case column. Add a bounded scan to `internal/evidence/store.go`:

```go
// FindLatestByKind returns the most recently created evidence object of a kind
// whose bytes decode into target, together with those bytes. The evidence store
// has no case or revision column, so a caller that must find a document by a
// business key decodes the most recent candidates and checks the key itself.
// The bound keeps a tick's cost predictable on a long-lived store.
func (s *Store) FindLatestByKind(ctx context.Context, kind string, target any, limit int) (EvidenceObject, []byte, bool, error) {
	if s == nil || s.state == nil {
		return EvidenceObject{}, nil, false, errors.New("evidence store is not configured")
	}
	if strings.TrimSpace(kind) == "" {
		return EvidenceObject{}, nil, false, errors.New("evidence kind is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.state.DB().QueryContext(ctx,
		`SELECT evidence_id, content_hash, media_type, kind, size_bytes, created_at
		 FROM evidence_objects WHERE kind = ?
		 ORDER BY created_at DESC, evidence_id DESC LIMIT ?`, kind, limit)
	if err != nil {
		return EvidenceObject{}, nil, false, fmt.Errorf("list evidence of kind %s: %w", kind, err)
	}
	defer rows.Close()
	candidates := make([]EvidenceObject, 0)
	for rows.Next() {
		var object EvidenceObject
		var createdAt string
		if err := rows.Scan(&object.ID, &object.ContentHash, &object.MediaType,
			&object.Kind, &object.SizeBytes, &createdAt); err != nil {
			return EvidenceObject{}, nil, false, fmt.Errorf("scan evidence of kind %s: %w", kind, err)
		}
		object.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return EvidenceObject{}, nil, false, fmt.Errorf("parse evidence created_at %q: %w", createdAt, err)
		}
		candidates = append(candidates, object)
	}
	if err := rows.Err(); err != nil {
		return EvidenceObject{}, nil, false, err
	}
	for _, candidate := range candidates {
		_, raw, err := s.Get(ctx, candidate.ID)
		if err != nil {
			return EvidenceObject{}, nil, false, err
		}
		if err := json.Unmarshal(raw, target); err != nil {
			continue
		}
		return candidate, raw, true, nil
	}
	return EvidenceObject{}, nil, false, nil
}
```

Add `encoding/json`, `strings` and `time` to the imports of that file if they are missing, and add the constant next to the other driver constants:

```go
	// acceptedIndexScanLimit bounds the candidate scan, so a long-lived store
	// cannot make a driver tick unbounded.
	acceptedIndexScanLimit = 100
```

The scan returns the newest decodable object, and the caller compares `record.CaseID` and `record.TaskID`, so a record belonging to another case is rejected rather than used. If a tick ever finds a newer accepted index for a different case ahead of its own, raise the limit; the default of 100 is far above the number of cases one mission holds.

- [ ] **Step 9: Run the driver test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestDriver -count=1`
Expected: PASS.

- [ ] **Step 10: Add the `run-gh-triage-driver` subcommand**

In `cmd/summa42-box/main.go`, register `run-gh-triage-driver` alongside the other `run-*` subcommands and implement it to open the box, construct the driver from the box's `workflowcase`, `execution`, `verification`, `runmanifest`, evidence store and clock services, call `Tick` once for the configured mission, and print the `DriverResult` as JSON to stdout. Model the command on the existing ADO `run-driver` subcommand. It takes no model configuration, because the driver never calls the model.

- [ ] **Step 11: Run the build and the affected tests**

Run: `GOCACHE=/tmp/summa42-full-go-cache go build ./cmd/summa42-box && GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/... ./internal/workflowcase/... ./internal/execution/... ./internal/evidence/... -count=1`
Expected: PASS.

- [ ] **Step 12: Run the race detector on the new packages**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test -race ./internal/ghtriage/... ./internal/evidence/... -count=1`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
gofmt -l internal/ghtriage internal/evidence internal/workflowcase cmd/summa42-box
git add internal/ghtriage internal/evidence internal/workflowcase cmd/summa42-box
git commit -m "feat(ghtriage): add the driver that applies the decision to the case

The driver runs after the lease completes, never inside the executor, so
the case never moves before the successful attempt is recorded. It
validates the single decision attached to a completed attempt, accepts the
task, and only then assesses the case, so a crash between acceptance and
assessment leaves a finished task and an ACTIVE case the next tick can
complete rather than a lost decision.

internal/verification has no read accessor for an acceptance record, so
the driver writes its own accepted index before accepting. That is the
restart replay path, and it is why a SUCCEEDED task whose case is still
ACTIVE is not silently skipped.

A task that exhausted its retries has no decision, so the driver writes a
canonical failure document and uses it as the assessment evidence; the
case is blocked as triage-failed instead of being left ACTIVE and looking
like work. Supersession compares parsed revision times rather than strings,
challenges the older pending task before blocking its case, and re-reads
the task afterwards so a racing lease is followed rather than overwritten."
```

---

### Task 8: The reviewer

Built last. It depends on the decision record, the driver, and the resolved case state, and it carries no state of its own, so nothing earlier depends on it.

**Files:**
- Create: `internal/state/sqlite/migrations/00016_github_issue_triage_reviews.sql`
- Create: `internal/ghtriage/reviewindex.go`
- Create: `internal/ghtriage/reviewer.go`
- Test: `internal/ghtriage/reviewer_test.go`
- Modify: `cmd/summa42-box/main.go` — add `run-gh-triage-review`

**Interfaces:**
- Consumes: `Decision`, `Stage1`, `Stage3`, `ReviewerVersion`, `ReviewSchema`, `KindReview`, `ReviewerModel`, `ReviewInput`, `ReviewerQuestion` from Tasks 1-3; `ListBySource` from Task 7.
- Produces: table `github_issue_triage_reviews`; `NewReviewIndex(*state.Store, clock.Clock) ReviewIndexStore`; `Reviewer`; `NewReviewer(...) *Reviewer`; `(*Reviewer).Tick(ctx, missionID) (ReviewResult, error)`; `ReviewResult`; `Structural`; `ReviewVerdict`; the `run-gh-triage-review` subcommand.

- [ ] **Step 1: Write the migration**

Create `internal/state/sqlite/migrations/00016_github_issue_triage_reviews.sql`:

```sql
-- +goose Up
CREATE TABLE github_issue_triage_reviews (
    decision_evidence_id TEXT NOT NULL REFERENCES evidence_objects(evidence_id),
    reviewer_version     TEXT NOT NULL,
    state_fingerprint    TEXT NOT NULL,
    verdict_evidence_id  TEXT NOT NULL REFERENCES evidence_objects(evidence_id),
    created_at           TEXT NOT NULL,
    PRIMARY KEY (decision_evidence_id, reviewer_version, state_fingerprint)
);

CREATE INDEX github_issue_triage_reviews_verdict
    ON github_issue_triage_reviews(verdict_evidence_id);

-- +goose Down
DROP INDEX github_issue_triage_reviews_verdict;
DROP TABLE github_issue_triage_reviews;
```

The primary key is the scheduling key, not a content hash: the model's boolean result is unknown before the call, so content hashing cannot suppress a repeated call on an unchanged decision. A later case transition changes the fingerprint and therefore gets a new structural review, and a deliberate re-review of an unchanged case uses a new reviewer version.

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/state/sqlite/... -count=1`
Expected: PASS, including the migration roundtrip test.

- [ ] **Step 2: Write the failing reviewer test**

Create `internal/ghtriage/reviewer_test.go`. The fixture registers a case and a task with a real accepted decision the way the driver fixture does, and exposes `model`, `reviewer`, `missionID`, `verdictCount(t)`, `lastVerdict(t)` and `corruptStage3Rule(t, rule)`. Implement `verdictCount` by counting evidence of kind `github.issue.triage.review` through the store, and `corruptStage3Rule` by loading the stored decision evidence, changing `Stage3.Rule`, storing a new object, and repointing the driver's accepted index at it.

```go
func TestReviewerMakesNoSecondModelCallWhileTheCaseStateIsUnchanged(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true, true}

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	first := f.model.ReviewCallCount()
	if first != 1 {
		t.Fatalf("model calls on the first tick = %d, want 1", first)
	}
	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.model.ReviewCallCount(); got != first {
		t.Fatalf("an unchanged case triggered %d extra model calls", got-first)
	}
	if got := f.verdictCount(t); got != 1 {
		t.Fatalf("verdict documents = %d, want exactly 1", got)
	}
}

func TestReviewerReviewsAgainAfterSupersessionChangesTheFingerprint(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true, true}

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	f.supersedeCase(t)
	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.model.ReviewCallCount(); got != 2 {
		t.Fatalf("model calls = %d, want a second call after the fingerprint changed", got)
	}
	if got := f.verdictCount(t); got != 2 {
		t.Fatalf("verdict documents = %d, want 2", got)
	}
}

func TestReviewerWritesNoVerdictAndNoIndexRowWhenTheModelFails(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.Err = errors.New("model unavailable")

	result, err := f.reviewer.Tick(context.Background(), f.missionID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want one failure", result)
	}
	if got := f.verdictCount(t); got != 0 {
		t.Fatalf("verdict documents = %d, want 0", got)
	}
	if got := f.reviewLinkCount(t); got != 0 {
		t.Fatalf("review index rows = %d, want 0 so the next tick retries", got)
	}
}

func TestReviewerReportsAnInjectedStage3Mismatch(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true}
	f.corruptStage3Rule(t, "a-rule-that-does-not-match")

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := f.lastVerdict(t)
	if verdict.Structural.RuleMatchesRecomputation != "mismatch" {
		t.Fatalf("recomputation = %q, want mismatch", verdict.Structural.RuleMatchesRecomputation)
	}
}

func TestReviewerReportsNotApplicableForAFutureRulesVersion(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true}
	f.rewriteDecision(t, func(d *ghtriage.Decision) {
		d.DispositionRulesVersion = "ghtriage.dispositions.v99"
		d.Stage3 = &ghtriage.Stage3Result{Disposition: ghtriage.DispositionReadyToPlan, Rule: "a-future-rule"}
	})

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if got := f.lastVerdict(t).Structural.RuleMatchesRecomputation; got != "not-applicable" {
		t.Fatalf("recomputation = %q, want not-applicable", got)
	}
}

func TestReviewerAcceptsASupersededCaseAsAnAllowedTerminalState(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true}
	f.supersedeCase(t)

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := f.lastVerdict(t)
	if !verdict.Structural.StateMatchesDisposition {
		t.Fatalf("a correctly superseded case was reported as a violation: %+v", verdict.Structural)
	}
}

func TestReviewerReportsAClassificationThatDisagreesWithIntake(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true}
	f.rewriteDecision(t, func(d *ghtriage.Decision) { d.Stage1.Triage = ghtriage.TriageFeature })

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	if f.lastVerdict(t).Structural.ClassificationAgreesWithIntake {
		t.Fatal("a stage 1 triage that disagrees with the snapshot was accepted")
	}
}

func TestReviewerRecordsTheDecisionAndTheStateItObserved(t *testing.T) {
	f := newReviewerFixture(t)
	f.model.ScriptedReview = []bool{true}

	if _, err := f.reviewer.Tick(context.Background(), f.missionID); err != nil {
		t.Fatal(err)
	}
	verdict := f.lastVerdict(t)
	if verdict.DecisionEvidenceID == "" {
		t.Fatal("the verdict does not name the decision it reviewed")
	}
	if verdict.ReviewerVersion != ghtriage.ReviewerVersion {
		t.Fatalf("reviewer version = %q", verdict.ReviewerVersion)
	}
	if verdict.CaseState == "" {
		t.Fatal("the verdict does not record the case state it observed")
	}
}
```

- [ ] **Step 3: Run the reviewer test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestReviewer -count=1`
Expected: FAIL, `undefined: ghtriage.NewReviewer`.

- [ ] **Step 4: Write the review index**

Create `internal/ghtriage/reviewindex.go`:

```go
package ghtriage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// ReviewIndexStore schedules reviews by (decision, reviewer version, observed
// case state). Two concurrent ticks may both pay for a model call, but only one
// verdict is linked and reported.
type ReviewIndexStore interface {
	ReviewLinked(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string) (bool, error)
	LinkReview(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string, verdictID domain.ID) (bool, error)
}

type reviewIndex struct {
	store *state.Store
	clock clock.Clock
}

func NewReviewIndex(store *state.Store, clk clock.Clock) ReviewIndexStore {
	return &reviewIndex{store: store, clock: clk}
}

func (r *reviewIndex) ReviewLinked(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("review index is not configured")
	}
	var one int
	err := r.store.DB().QueryRowContext(ctx,
		`SELECT 1 FROM github_issue_triage_reviews
		 WHERE decision_evidence_id = ? AND reviewer_version = ? AND state_fingerprint = ?`,
		decisionID, reviewerVersion, fingerprint).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up triage review link: %w", err)
	}
	return true, nil
}

func (r *reviewIndex) LinkReview(ctx context.Context, decisionID domain.ID, reviewerVersion, fingerprint string, verdictID domain.ID) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("review index is not configured")
	}
	now := r.clock.Now().UTC().Format(time.RFC3339Nano)
	result, err := r.store.DB().ExecContext(ctx,
		`INSERT INTO github_issue_triage_reviews
		 (decision_evidence_id, reviewer_version, state_fingerprint, verdict_evidence_id, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(decision_evidence_id, reviewer_version, state_fingerprint) DO NOTHING`,
		decisionID, reviewerVersion, fingerprint, verdictID, now)
	if err != nil {
		return false, fmt.Errorf("link triage review: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}
```

- [ ] **Step 5: Write the reviewer**

Create `internal/ghtriage/reviewer.go`:

```go
package ghtriage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

type Structural struct {
	Stage2OnlyIfUnresolved         bool   `json:"stage2_only_if_unresolved"`
	SchemaConformant               bool   `json:"schema_conformant"`
	RuleMatchesRecomputation       string `json:"rule_matches_recomputation"`
	StateMatchesDisposition        bool   `json:"state_matches_disposition"`
	ClassificationAgreesWithIntake bool   `json:"classification_agrees_with_intake"`
}

type ReviewVerdict struct {
	Schema             string     `json:"schema"`
	ReviewerVersion    string     `json:"reviewer_version"`
	DecisionEvidenceID string     `json:"decision_evidence_id"`
	Repository         string     `json:"repository"`
	Issue              int64      `json:"issue"`
	Revision           string     `json:"revision"`
	CaseState          string     `json:"case_state"`
	LatestAssessmentID *string    `json:"latest_assessment_id"`
	Structural         Structural `json:"structural"`
	Plausible          bool       `json:"plausible"`
}

type ReviewResult struct {
	Reviewed int
	Skipped  int
	Failed   int
}

type Reviewer struct {
	cases    *workflowcase.Service
	evidence *evidence.Store
	index    ReviewIndexStore
	model    ReviewerModel
	clock    clock.Clock
}

func NewReviewer(cases *workflowcase.Service, store *evidence.Store, index ReviewIndexStore, model ReviewerModel, clk clock.Clock) *Reviewer {
	return &Reviewer{cases: cases, evidence: store, index: index, model: model, clock: clk}
}

// Tick reviews every decision whose case state it has not already observed. It
// changes nothing, gates nothing and blocks nothing.
func (r *Reviewer) Tick(ctx context.Context, missionID domain.ID) (ReviewResult, error) {
	if r == nil || r.cases == nil || r.evidence == nil || r.index == nil || r.model == nil {
		return ReviewResult{}, errors.New("triage reviewer is not configured")
	}
	var result ReviewResult
	cases, err := r.cases.ListBySource(ctx, missionID, SourceGitHub)
	if err != nil {
		return result, err
	}
	for _, c := range cases {
		outcome, err := r.reviewCase(ctx, c)
		if err != nil {
			result.Failed++
			fmt.Fprintf(os.Stderr, "triage review of case %s failed: %v\n", c.ID, err)
			continue
		}
		if outcome {
			result.Reviewed++
			continue
		}
		result.Skipped++
	}
	return result, nil
}

func (r *Reviewer) reviewCase(ctx context.Context, c workflowcase.Case) (bool, error) {
	records, err := r.cases.ListAssessments(ctx, c.ID)
	if err != nil {
		return false, err
	}
	fingerprint, latest := stateFingerprint(c, records)
	reason := latestReason(records, latest)

	// A task that exhausted its retries has no decision to review, and the
	// driver already reported it. Recording "review unavailable" as a verdict
	// would mark it reviewed without performing any check.
	if reason == ReasonTriageFailed {
		return false, nil
	}
	decision, decisionID, err := r.findDecision(ctx, c, records)
	if err != nil {
		return false, err
	}
	if decision == nil {
		return false, nil
	}
	linked, err := r.index.ReviewLinked(ctx, decisionID, ReviewerVersion, fingerprint)
	if err != nil {
		return false, err
	}
	if linked {
		return false, nil
	}

	snap, err := r.snapshot(ctx, *decision)
	if err != nil {
		return false, err
	}
	plausible, err := r.model.Review(ctx, ReviewInput{
		Schema:   ReviewSchema,
		Title:    snap.Title,
		Body:     snap.Body,
		Question: ReviewerQuestion,
		Decision: *decision,
	})
	if err != nil {
		// No verdict document and no index row, so the next tick retries.
		return false, fmt.Errorf("reviewer model call: %w", err)
	}

	verdict := ReviewVerdict{
		Schema:             ReviewSchema,
		ReviewerVersion:    ReviewerVersion,
		DecisionEvidenceID: string(decisionID),
		Repository:         decision.Repository,
		Issue:              decision.Issue,
		Revision:           decision.Revision,
		CaseState:          string(c.State),
		LatestAssessmentID: latest,
		Structural:         r.structural(ctx, c, records, latest, *decision),
		Plausible:          plausible,
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return false, err
	}
	object, err := r.evidence.Put(ctx, strings.NewReader(string(raw)), evidence.Metadata{
		MediaType: DecisionMediaType,
		Kind:      KindReview,
	})
	if err != nil {
		return false, err
	}
	if _, err := r.index.LinkReview(ctx, decisionID, ReviewerVersion, fingerprint, object.ID); err != nil {
		return false, err
	}
	fmt.Fprintf(os.Stderr,
		"triage review %s#%d at %s: plausible=%v structural=%+v verdict=%s\n",
		decision.Repository, decision.Issue, decision.Revision, plausible, verdict.Structural, object.ID)
	return true, nil
}

// structural re-derives what can be re-derived from stored inputs. It never
// re-runs the classifier.
func (r *Reviewer) structural(ctx context.Context, c workflowcase.Case, records []workflowcase.AssessmentRecord, latest *string, decision Decision) Structural {
	return Structural{
		Stage2OnlyIfUnresolved:         decision.Stage1.Resolved() == (decision.Stage2 == nil),
		SchemaConformant:               decision.Validate() == nil,
		RuleMatchesRecomputation:       r.recompute(ctx, decision),
		StateMatchesDisposition:        r.stateMatches(c, records, latest, decision),
		ClassificationAgreesWithIntake: r.classificationAgrees(ctx, decision),
	}
}

// recompute re-derives the outcome. A record written by a newer rules version
// is reported as not-applicable, not as a mismatch: the reviewer simply does
// not implement those rules, and calling history violating would be wrong.
func (r *Reviewer) recompute(ctx context.Context, decision Decision) string {
	if decision.Stage1.Resolved() {
		if decision.TriageRulesVersion != TriageRulesVersion {
			return "not-applicable"
		}
		snap, err := r.snapshot(ctx, decision)
		if err != nil {
			return "mismatch"
		}
		recomputed := Stage1(snap)
		if recomputed.Disposition == decision.Stage1.Disposition && recomputed.Rule == decision.Stage1.Rule {
			return "match"
		}
		return "mismatch"
	}
	if decision.DispositionRulesVersion != DispositionRulesVersion {
		return "not-applicable"
	}
	if decision.Stage2 == nil || decision.Stage3 == nil {
		return "mismatch"
	}
	recomputed := Stage3(*decision.Stage2)
	if recomputed.Disposition == decision.Stage3.Disposition && recomputed.Rule == decision.Stage3.Rule {
		return "match"
	}
	return "mismatch"
}

// stateMatches treats a superseded case as an allowed terminal state, so a
// correctly superseded revision is never reported as a violation.
func (r *Reviewer) stateMatches(c workflowcase.Case, records []workflowcase.AssessmentRecord, latest *string, decision Decision) bool {
	reason := latestReason(records, latest)
	if strings.HasPrefix(reason, supersededReasonHead) {
		return c.State == workflowcase.Blocked
	}
	switch decision.FinalDisposition() {
	case DispositionReadyToPlan:
		return c.State == workflowcase.Active && reason == ""
	default:
		return c.State == workflowcase.Blocked && reason == string(decision.FinalDisposition())
	}
}

// classificationAgrees recomputes ClassifyTriage and the exact stage 1 widening
// rule from the snapshot, rather than accepting any difference merely because
// the recorded value is question or duplicate.
func (r *Reviewer) classificationAgrees(ctx context.Context, decision Decision) bool {
	snap, err := r.snapshot(ctx, decision)
	if err != nil {
		return false
	}
	return Stage1(snap).Triage == decision.Stage1.Triage
}

func (r *Reviewer) snapshot(ctx context.Context, decision Decision) (Snapshot, error) {
	_, raw, err := r.evidence.Get(ctx, domain.ID(decision.SnapshotEvidenceID))
	if err != nil {
		return Snapshot{}, fmt.Errorf("load snapshot %s: %w", decision.SnapshotEvidenceID, err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decode snapshot %s: %w", decision.SnapshotEvidenceID, err)
	}
	return snap, nil
}

// findDecision locates the decision of a case: the assessment of its own task
// first, and the driver's accepted index otherwise, which covers the window
// between AcceptTask and Assess.
func (r *Reviewer) findDecision(ctx context.Context, c workflowcase.Case, records []workflowcase.AssessmentRecord) (*Decision, domain.ID, error) {
	for i := len(records) - 1; i >= 0; i-- {
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(records[i].ResultJSON), &result); err != nil {
			continue
		}
		for _, evidenceID := range result.Decision.EvidenceIDs {
			decision, id, ok, err := r.loadDecision(ctx, evidenceID)
			if err != nil {
				return nil, domain.ID(""), err
			}
			if ok {
				return decision, id, nil
			}
		}
	}
	return r.findIndexedDecision(ctx, c)
}

func (r *Reviewer) findIndexedDecision(ctx context.Context, c workflowcase.Case) (*Decision, domain.ID, error) {
	raw, found, err := findAcceptedIndex(ctx, r.evidence, c)
	if err != nil || !found {
		return nil, domain.ID(""), err
	}
	var record acceptedIndex
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, domain.ID(""), fmt.Errorf("decode accepted index: %w", err)
	}
	decision, id, ok, err := r.loadDecision(ctx, record.DecisionEvidenceID)
	if err != nil || !ok {
		return nil, domain.ID(""), err
	}
	return decision, id, nil
}

func (r *Reviewer) loadDecision(ctx context.Context, evidenceID string) (*Decision, domain.ID, bool, error) {
	object, raw, err := r.evidence.Get(ctx, domain.ID(evidenceID))
	if err != nil {
		return nil, domain.ID(""), false, err
	}
	if object.Kind != KindDecision {
		return nil, domain.ID(""), false, nil
	}
	var decision Decision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return nil, domain.ID(""), false, fmt.Errorf("decode decision %s: %w", evidenceID, err)
	}
	return &decision, domain.ID(evidenceID), true, nil
}

// stateFingerprint is the canonical pair of the case state and its latest
// assessment, so a later transition schedules a new structural review.
func stateFingerprint(c workflowcase.Case, records []workflowcase.AssessmentRecord) (string, *string) {
	if len(records) == 0 {
		return string(c.State) + "|none", nil
	}
	latest := records[len(records)-1].ID
	return string(c.State) + "|" + latest, &latest
}

func latestReason(records []workflowcase.AssessmentRecord, latest *string) string {
	if latest == nil {
		return ""
	}
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].ID != *latest {
			continue
		}
		var result workflowcase.AssessmentResult
		if err := json.Unmarshal([]byte(records[i].ResultJSON), &result); err != nil {
			return ""
		}
		return result.Decision.Reason
	}
	return ""
}
```

`ListAssessments` orders by `created_at, assessment_id` (`internal/workflowcase/service.go:203`), so the last element of the slice is the latest assessment. `stateFingerprint` and `latestReason` both rely on that and must not re-sort.

- [ ] **Step 6: Add the reviewer's `findAcceptedIndex` helper**

`evidence.Store.FindLatestByKind` and the `acceptedIndexScanLimit` constant were already added in Task 7, Step 8. Add only the reviewer's helper to `internal/ghtriage/reviewer.go`:

```go
// findAcceptedIndex returns the driver's accepted index for a case, using the
// bounded candidate scan because the evidence store carries no case column.
func findAcceptedIndex(ctx context.Context, store *evidence.Store, c workflowcase.Case) ([]byte, bool, error) {
	if store == nil {
		return nil, false, errors.New("evidence store is not configured")
	}
	var record acceptedIndex
	_, raw, found, err := store.FindLatestByKind(ctx, KindAccepted, &record, acceptedIndexScanLimit)
	if err != nil || !found {
		return nil, false, err
	}
	if record.CaseID != c.ID {
		return nil, false, nil
	}
	return raw, true, nil
}
```

The scan returns the newest decodable object and the caller compares `record.CaseID`, so an index belonging to another case is rejected rather than used.

- [ ] **Step 7: Run the reviewer test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestReviewer -count=1`
Expected: PASS.

- [ ] **Step 8: Add the `run-gh-triage-review` subcommand**

In `cmd/summa42-box/main.go`, register `run-gh-triage-review` and implement it to open the box, construct the review index from the box's `state.Store` and clock, construct the reviewer from the box's `workflowcase` service, evidence store, the index and the same model adapter the worker uses, call `Tick` once for the configured mission, and print the `ReviewResult` as JSON to stdout. Model the command on `run-gh-triage-driver`.

- [ ] **Step 9: Run the build and the affected tests**

Run: `GOCACHE=/tmp/summa42-full-go-cache go build ./cmd/summa42 ./cmd/summa42-box && GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage/... ./internal/evidence/... ./internal/state/... -count=1`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
gofmt -l internal/ghtriage internal/evidence internal/state
git add internal/ghtriage internal/evidence internal/state cmd/summa42-box
git commit -m "feat(ghtriage): add the stage 4 reviewer

The reviewer must not call the model on every tick for an unchanged
decision, and content hashing cannot schedule that, because the model's
boolean result is unknown before the call. A small table keyed by
(decision, reviewer version, observed case state) schedules it: a later
transition changes the fingerprint and earns a new structural review, and
a deliberate re-review of an unchanged case uses a new reviewer version.
Two concurrent ticks may both pay for the call, but only one verdict is
linked.

A failed model call writes neither a verdict nor an index row, so the
retry is not suppressed. The reviewer records the state and assessment it
observed, and treats a superseded case as an allowed terminal state so a
correct supersession is never reported as a violation. A record written by
a newer rules version is reported as not-applicable rather than as a
mismatch, because the reviewer does not implement those rules."
```

---

## Validation

After the last task, run the full matrix from `CONTRIBUTING.md` before considering the work done:

```bash
GOCACHE=/tmp/summa42-full-go-cache go test ./... -count=1
GOCACHE=/tmp/summa42-full-go-cache go test -race ./... -count=1
GOCACHE=/tmp/summa42-full-go-cache go vet ./...
GOCACHE=/tmp/summa42-full-go-cache go build ./cmd/summa42 ./cmd/summa42-box
```

Then run the two targeted gates this slice touches:

```bash
GOCACHE=/tmp/summa42-full-go-cache go test -run TestCloseConcurrentReplay ./internal/workflowcase -count=600 -timeout 30m
GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler/... -count=1
```

Verify formatting only for the files this plan touched:

```bash
gofmt -l internal/ghtriage internal/scheduler internal/execution internal/workflowcase internal/evidence internal/runtime cmd/summa42-box
```

Expected: no output. The repository is not globally gofmt-clean, so a whole-repo `gofmt -l internal cmd` will report pre-existing files and is not a gate.

Finally, run the SQLite persistence spike and the OCI capability acceptance profile exactly as `CONTRIBUTING.md` specifies. `AGENTS.md` stays untracked throughout.

