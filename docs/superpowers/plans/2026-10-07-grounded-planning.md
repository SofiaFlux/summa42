# Grounded Planning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans task-by-task with TDD, then one independent Astra review.

**Goal:** Supply pinned repository evidence to issue planning and independently review the bounded plan before an authorized implementation proposal.
**Architecture:** Local Git source projection → v2 immutable plan → independent model review → guarded WorkflowCase assessment. Existing v1 planning stays compatible.
**Tech Stack:** Go 1.27, Git, SQLite/evidence and existing CLI model adapter.
**Spec:** `docs/superpowers/specs/2026-10-07-grounded-planning-design.md`

## Global Constraints

- Explicit local repository and full 40-character SHA; no ambient Git config/credentials/hooks/replace refs.
- Exact safe relative paths, regular UTF-8 tracked context files, maximum 32 files and 256 KiB total.
- Owner-supplied scope/test argv are inert data; model cannot grant authority or expand them.
- V1 remains readable. ACCEPT needs v2 and the existing grant/latest revision guard.
- No implementation execution or GitHub writes in this slice.

## Review Focus

- Malicious Git configuration, hooks, dirty worktrees and symlink/submodule context must not affect pinned input.
- Tampered or cross-case evidence must not authorize continuation.
- Source identity and allowed-path drift must fail closed.
- Concurrent issue revisions or duplicate ticks must not advance stale work.
- Legacy commands remain usable; partial source options must fail before a paid model call.

### Task 1: Pinned source and checkout

**Files:** `internal/repoworkspace/source.go`, `source_test.go`.
**Interfaces:** Produce `Capture(ctx, Config) (Snapshot,error)` and `Checkout(ctx, Config, destination) (Snapshot,error)`. Config holds local repository, GitHub repository identity, Commit, Paths. Snapshot holds schema, repository, commit and hashed File records.
- [x] RED: real Git fixture pins old commit despite dirty files/new HEAD; malicious paths/links/oversize fail; detached checkout cannot write shared source objects or run hooks.
- [x] GREEN: implement bounded Git subprocesses with explicit environment and canonical Snapshot validation/hash.
- [x] Verify: `go test ./internal/repoworkspace -count=1 -timeout 2m` → PASS; commit.

### Task 2: Grounded plan and CLI contract

**Files:** `internal/ghtriage/grounding.go`, `planner.go`, `plan.go`, grounded tests; `cmd/summa42-box/ghplan_source.go` and tests.
**Interfaces:** Consume Snapshot; produce `GroundingConfig`, `PlanSource`, PlanSchemaV2 and optional PlanInput context. Planner `SetGrounding(Config) error` freezes caller-owned configuration.
- [x] RED: v2 planning stores source evidence and preserves exact owner scope/test argv; wrong repository fails before model; partial CLI grounding fails; v1 tests remain unchanged.
- [x] GREEN: capture context before model, persist source references/hash in v2 plan; add validated source flags to `run-gh-plan`.
- [x] Verify: `go test ./internal/ghtriage ./cmd/summa42-box -count=1 -timeout 3m` → PASS; commit.

### Task 3: Independent plan review and handoff

**Files:** `internal/ghtriage/planreview.go`, `planreview_test.go`, `climodel/adapter.go`; `cmd/summa42-box/ghplan_review.go`, main dispatch and tests.
**Interfaces:** Consume exact v2 plan/projection/case assessment; produce strict `PlanReviewOutput`, durable bound review record and guarded implementation proposal.
- [x] RED: ACCEPT continues only with scope-backed grant; missing grant holds; v1/cross-case/tampered evidence does not call model; a new issue revision during call does not advance; second tick does not repeat review.
- [x] GREEN: load authoritative current assessment/citations, validate projection/contract, separately invoke review model, persist verdict and assess using RequireLatestRevision.
- [x] Verify: `go test ./internal/ghtriage/... ./cmd/summa42-box -count=1 -timeout 3m` → PASS; commit.

### Delivery

Run full suite (record known local socket limitation), affected race tests, vet/build and diff checks. Astra reviews main-to-HEAD including PR #19 foundation. Fix Important/Critical with RED→GREEN and re-run affected/full suite. Publish identical tested Git tree to PR #19, update description and observe CI. Do not merge/deploy.


## Implementation record

Tasks 1–3 are implemented on `codex/maintainer-foundation` for draft PR #19. The scope is supervised preparation and review; implementation execution, automated GitHub publication, merge and self-upgrade remain future milestones.

Astra reviewed the complete branch against main. Its Important findings were duplicate paid reviews under concurrent ticks and possible automatic partial-clone fetching. Both received behavioral RED→GREEN regressions. Review now claims a Task/Attempt before invoking the model, reuses the exact completed verdict after interrupted assessment, and rejects expired attempt completion. Source preparation disables lazy fetching and constrains transport. Minor findings were also addressed: explicit empty grounding flags cannot fall back to v1, and Git >=2.45 is documented. Astra did not re-review the final fix tree.

Implementation rulings:
- Extend the supervised recipe and preserve v1; explicitly supplied source, scope and validation argv remain owner authority. Governed model composition and implementation execution require separate milestones.
- Resolve accepted triage index `task_id` through its existing Work idempotency key, as PlanningGate does; confusing it with the generated Task ID would prevent review.
- Treat explicitly empty source flags as a contract error because silent v1 fallback could incur an unintended model call. This deliberately rejects previously ignored empty values.
- Keep native model invocation classified UNENFORCED. These checks do not certify hostile Git metadata containment, live provider integration, monetary settlement or future implementation execution; composition must be revisited before autonomous operation.

Validation: the available full Go suite passed with only the two unchanged Unix socket tests excluded (`TestListenLocalRefusesToReplaceActiveSocket`, `TestListenLocalUsesPlatformLocalTransport`). The original full command failed on precisely those environment limitations. `go vet ./...`, both CLI builds and `git diff --check` passed. CI keeps the full suite and OCI gates enabled.

Final affected race gate passed: `go test -race ./internal/repoworkspace ./internal/ghtriage/... ./internal/verification ./cmd/summa42-box -count=1 -timeout 8m`. The earlier foundation race gate also passed for scheduler, executors and ADO review.
