# Exact-SHA CI Observation and Protected Merge (Milestone A2)

Owner goal: implement Etap 6 of the self-development roadmap (`docs/superpowers/plans/2026-10-07-self-development-roadmap.md`), closing the autonomous PR delivery loop. A published draft/open PR cannot be merged without independent CI observation on the exact candidate SHA, review compliance, and a guarded merge operation with read-only reconciliation.

## Scope and Architecture

Building upon PR #20 (`cmd/summa42-box/ghpublish.go`, `internal/ghtriage/publication.go`, `internal/ghpublish/providers.go`), this subsystem adds:
1. **CI Observation Provider & Client (`internal/ghpublish/ci.go`)**:
   - Queries GitHub Check Runs (`GET /repos/{owner}/{repo}/commits/{ref}/check-runs`) and Commit Statuses (`GET /repos/{owner}/{repo}/commits/{ref}/status`).
   - Evaluates whether all required/reported checks for the exact `head_sha` are completed with success.
   - Rejects stale, in-progress, or failed checks fail-closed.
2. **Review Observation (`internal/ghpublish/reviews.go`)**:
   - Reads pull request reviews (`GET /repos/{owner}/{repo}/pulls/{number}/reviews`).
   - Ensures any required review is on the current `head_sha` and no active `CHANGES_REQUESTED` state remains.
3. **Protected Merge Provider (`MergeProvider` in `internal/ghpublish/merge.go`)**:
   - Capability: `github.pr.merge`
   - Provider Name: `github-candidate-merge`
   - Implements `operations.Provider` with `CanonicalIntent`, `CostProfile`, `Dispatch`, `LookupOutcome`.
   - Intent: `MergeIntent{ Repository, PullNumber, HeadSHA, MergeMethod, CommitTitle, CommitMessage }`.
   - Strict pinning: validates `head_sha` before calling GitHub merge endpoint (`PUT /repos/{owner}/{repo}/pulls/{number}/merge`).
   - Idempotent & Read-Only Reconciliation: checks if PR is already merged (`merged == true`) with matching head/merge commit. Never retries blindly on timeout.
4. **Merge Coordinator & Stage Service (`internal/ghtriage/merge.go`)**:
   - Loads the full provenance chain (triage $\to$ plan $\to$ plan-review $\to$ implementation $\to$ code-review $\to$ publication).
   - Validates that current Case grant contains `github.pr.merge`.
   - Verifies CI observation on the exact candidate SHA is `SUCCESS`.
   - Prepares and dispatches `MergeIntent` using `p.operations.PrepareWithGuard` and `p.operations.DispatchWithGuard`.
   - Records immutable `github.issue.merge` evidence bound to the attempt and fence.
5. **CLI Subcommand in `cmd/summa42-box`**:
   - `run-gh-merge`: accepts `--case`, `--source-repo`, `--repository`, `--credential-file`, `--merge-method`.
   - Coordinates CI check, review check, and protected merge execution.

## Invariants and Safety Rules

1. **Exact Head SHA Binding**:
   - Merge condition passes `sha: HeadSHA` to GitHub API. If head moved on remote, GitHub rejects and the provider fails closed.
2. **Never Overwrite or Duplicate**:
   - Inconclusive HTTP responses or timeouts do not re-issue a merge request; `LookupOutcome` checks whether `merged == true` on the PR.
3. **Required Checks Non-Empty**:
   - If CI checks are still running or missing required checks, the gate returns `PENDING` / `HELD`, never `SUCCESS`.
4. **Audit and Evidence**:
   - Produces immutable evidence for both CI evaluation and confirmed Merge commit.
