# CI Observation and Protected Merge (Milestone A2) Implementation Plan

> **Status: Reverted 2026-10-21, see review-remediation plan.**

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement Etap 6 of the self-development roadmap: exact-SHA GitHub CI observation, PR review validation, and protected merge operation with idempotent read-only reconciliation.

**Architecture:** Extend `internal/ghpublish` with CI and Review observers and a protected `MergeProvider` implementing `operations.Provider`. Introduce `Merger` in `internal/ghtriage` to enforce the full verification chain (triage $\to$ plan $\to$ review $\to$ implementation $\to$ code-review $\to$ publication $\to$ CI observation $\to$ merge) and wire `run-gh-merge` into `cmd/summa42-box`.

**Tech Stack:** Go 1.27, standard `net/http` client with bounded JSON readers, SQLite WAL persistence, OPA policies, and existing Summa42 guarded operation lifecycle.

## Global Constraints

- Go 1.27 environment. Run all Go commands with `GOCACHE=/tmp/summa42-full-go-cache`.
- Follow strict single-parent and exact commit SHA validation. Never merge if `head_sha` drifted.
- All external HTTP calls must use HTTPS (except loopback in tests), 20s timeouts, bounded 1 MiB response readers, and disabled redirects.
- Inconclusive merge responses must be reconciled via read-only PR state inspection (`LookupOutcome`), never retried blindly.
- No ambient exfiltration: GitHub tokens must only be accessed via private credential files and never passed to subshells or logged.

---

### Task 1: GitHub CI & PR Review Observers in `internal/ghpublish`

**Files:**
- Create: `internal/ghpublish/observer.go`
- Test: `internal/ghpublish/observer_test.go`

**Interfaces:**
- `type CIState string` (`CIStateSuccess`, `CIStatePending`, `CIStateFailed`)
- `type CheckRunStatus struct { Name, Status, Conclusion, HeadSHA string }`
- `type CIObservation struct { HeadSHA string; State CIState; Runs []CheckRunStatus }`
- `type ReviewState string` (`ReviewApproved`, `ReviewChangesRequested`, `ReviewCommented`, `ReviewPending`)
- `type ReviewStatus struct { User string; State ReviewState; CommitSHA string }`
- `type ReviewObservation struct { HeadSHA string; Approved bool; ChangesRequested bool; Reviews []ReviewStatus }`
- Methods on `client`:
  - `ObserveCI(ctx context.Context, headSHA string, requiredChecks []string) (CIObservation, error)`
  - `ObserveReviews(ctx context.Context, pullNumber int, headSHA string) (ReviewObservation, error)`

- [ ] **Step 1: Write the failing tests**

Write `internal/ghpublish/observer_test.go` testing:
1. `TestObserveCI_Success`: Check runs all completed with `conclusion: success` returns `CIStateSuccess`.
2. `TestObserveCI_Pending`: Check runs with `status: in_progress` or `queued` returns `CIStatePending`.
3. `TestObserveCI_Failure`: Check run with `conclusion: failure` returns `CIStateFailed`.
4. `TestObserveCI_MissingRequiredCheck`: Required check not present in check-runs returns `CIStatePending`.
5. `TestObserveReviews_Approved`: Review with state `APPROVED` on matching `head_sha` returns `Approved: true`.
6. `TestObserveReviews_ChangesRequested`: Review with `CHANGES_REQUESTED` returns `ChangesRequested: true`.
7. `TestObserveReviews_StaleReviewOnOldHead`: Review on different commit SHA does not count as approved for `head_sha`.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghpublish -run TestObserve -count=1`
Expected: FAIL (compilation error, types not yet defined).

- [ ] **Step 3: Implement `internal/ghpublish/observer.go`**

Implement `ObserveCI` and `ObserveReviews` using `client.request`.
Parse GitHub check-runs from `/commits/{ref}/check-runs` and PR reviews from `/pulls/{number}/reviews`.

- [ ] **Step 4: Run test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghpublish -run TestObserve -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ghpublish/observer.go internal/ghpublish/observer_test.go
git commit -m "feat(ghpublish): add exact-SHA CI and PR review observers"
```

---

### Task 2: Protected Merge Operation Provider in `internal/ghpublish`

**Files:**
- Create: `internal/ghpublish/merge.go`
- Test: `internal/ghpublish/merge_test.go`

**Interfaces:**
- `const MergeProviderName = "github-candidate-merge"`
- `type MergeIntent struct { Repository string; PullNumber int; HeadSHA string; BaseBranch string; MergeMethod string; CommitTitle string; CommitMessage string }`
- `type MergeProvider struct { c *client }`
- Implements `operations.Provider`:
  - `Name() string` -> `github-candidate-merge`
  - `Capability() string` -> `github.pr.merge`
  - `EnforcementLevel() domain.EnforcementLevel` -> `domain.EnforcementEnforced`
  - `AdapterVersion() string` -> `"1"`
  - `AdapterVersionSemanticallyRelevant() bool` -> `true`
  - `CanonicalIntent(operations.IntentDescriptor) ([]byte, error)`
  - `CostProfile(operations.IntentDescriptor) (operations.CostProfile, error)`
  - `Dispatch(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error)`
  - `LookupOutcome(context.Context, operations.ProviderDispatchRequest) (operations.ProviderOutcome, error)`

- [ ] **Step 1: Write the failing tests**

Write `internal/ghpublish/merge_test.go` covering:
1. `TestMergeProvider_Success`: Dispatches `PUT /repos/{owner}/{repo}/pulls/{number}/merge` with `sha: HeadSHA`, returns confirmed outcome with merge commit SHA.
2. `TestMergeProvider_HeadMoved`: Remote head does not match `HeadSHA` $\to$ fails closed without merging.
3. `TestMergeProvider_ReconcileTimeout`: When PUT merge times out or returns 500, but subsequent GET shows PR is `merged == true`, returns confirmed outcome with existing merge commit.
4. `TestMergeProvider_InvalidIntent`: Malformed PR number, non-commit SHA, or unsupported merge method fails validation.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghpublish -run TestMergeProvider -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/ghpublish/merge.go`**

Implement `MergeProvider` with strict `HeadSHA` binding and idempotent reconciliation.

- [ ] **Step 4: Run test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghpublish -run TestMergeProvider -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ghpublish/merge.go internal/ghpublish/merge_test.go
git commit -m "feat(ghpublish): implement protected PR merge provider with reconciliation"
```

---

### Task 3: Merge Coordinator & Evidence in `internal/ghtriage`

**Files:**
- Create: `internal/ghtriage/merge.go`
- Test: `internal/ghtriage/merge_test.go`

**Interfaces:**
- `const KindMerge = "github.issue.merge"`
- `type MergeRecord struct { Schema string; CaseID, WorkID, TaskID, AttemptID domain.ID; Fence int64; PublicationEvidenceID domain.ID; Repository, BaseBranch, HeadBranch, CandidateSHA, MergeCommitSHA, PRReference string; MergedAt time.Time }`
- `type MergeResult struct { Held bool; ApprovalID, OperationID domain.ID; Busy bool; Pending bool; Reused bool; EvidenceID domain.ID; Record *MergeRecord; CIEvidenceID domain.ID }`
- `type Merger struct { cases *workflowcase.Service; exec *execution.Service; verify *verification.Service; evidence *evidence.Store; operations *operations.Service; ghClient *ghpublish.Config }`
- `func (m *Merger) Merge(ctx context.Context, caseID domain.ID, sourceRepo, baseBranch, mergeMethod string, requiredChecks []string) (MergeResult, error)`

- [ ] **Step 1: Write the failing tests**

Write `internal/ghtriage/merge_test.go` covering:
1. `TestMerger_Success`: Successful execution through publication verification, CI observation, guarded merge dispatch, and evidence creation.
2. `TestMerger_PendingCI`: When CI observation is pending, returns `Pending: true` without dispatching merge.
3. `TestMerger_FailedCI`: When CI observation fails, returns error and blocks merge.
4. `TestMerger_UnresolvedReview`: When changes are requested or review is missing, blocks merge.
5. `TestMerger_GuardedRevocation`: Upstream case revocation between prepare and dispatch halts merge.
6. `TestMerger_ReplayConfirmed`: Repeated call returns cached merge evidence and record.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestMerger -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `internal/ghtriage/merge.go`**

Implement `Merger` using existing patterns from `publication.go` and `codereview.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghtriage -run TestMerger -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ghtriage/merge.go internal/ghtriage/merge_test.go
git commit -m "feat(ghtriage): implement guarded merge coordinator and evidence recording"
```

---

### Task 4: CLI Subcommand `run-gh-merge` in `cmd/summa42-box`

**Files:**
- Create: `cmd/summa42-box/ghmerge.go`
- Modify: `cmd/summa42-box/main.go`
- Test: `cmd/summa42-box/ghmerge_test.go`

**Interfaces:**
- Flags:
  - `-case`: active Case ID
  - `-source-repo`: local Git repository
  - `-repository`: GitHub owner/repo
  - `-base-branch`: base branch (e.g. `main`)
  - `-merge-method`: `merge`, `squash`, or `rebase` (default: `merge`)
  - `-required-checks`: comma-separated required CI check run names
  - `-credential-file`: private GitHub token file

- [ ] **Step 1: Write the failing test**

Write `cmd/summa42-box/ghmerge_test.go` verifying CLI flag parsing, missing required flags, and execution with a mock server.

- [ ] **Step 2: Run test to verify it fails**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./cmd/summa42-box -run TestGHMerge -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement `cmd/summa42-box/ghmerge.go` and wire into `main.go`**

Wire `run-gh-merge` into `cmd/summa42-box/main.go` `main()` dispatcher.

- [ ] **Step 4: Run test to verify it passes**

Run: `GOCACHE=/tmp/summa42-full-go-cache go test ./cmd/summa42-box -run TestGHMerge -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/summa42-box/ghmerge.go cmd/summa42-box/ghmerge_test.go cmd/summa42-box/main.go
git commit -m "feat(summa42-box): add run-gh-merge subcommand"
```

---

### Task 5: End-to-End Suite Verification and Binary Rebuild

**Files:**
- Test: All affected packages

- [ ] **Step 1: Run full test suite for affected packages**

Run:
```bash
GOCACHE=/tmp/summa42-full-go-cache go test ./internal/ghpublish ./internal/ghtriage ./cmd/summa42-box -count=1
GOCACHE=/tmp/summa42-full-go-cache go vet ./internal/ghpublish ./internal/ghtriage ./cmd/summa42-box
```

- [ ] **Step 2: Rebuild binaries in `./bin`**

Run:
```bash
GOCACHE=/tmp/summa42-full-go-cache go build -o ./bin/summa42 ./cmd/summa42
GOCACHE=/tmp/summa42-full-go-cache go build -o ./bin/summa42-box ./cmd/summa42-box
```

- [ ] **Step 3: Verify CLI help displays `run-gh-merge`**

Run:
```bash
./bin/summa42-box run-gh-merge -h
```

- [ ] **Step 4: Final commit and verification summary**
