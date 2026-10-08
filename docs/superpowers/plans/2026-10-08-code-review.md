# Independent candidate review implementation plan

> **For agentic workers:** Use superpowers:executing-plans. Native execution chosen under owner/AGENTS.md delegation; Astra reviews the resulting branch.

**Goal:** Accept a local implementation only after a separately claimed, pinned code review.

**Architecture:** Authority-free review Task keyed by implementation Work; immutable bounded Git projection; staged verdict recovery; guarded completion and acceptance. Keep Case Work unchanged until a future publication stage.

**Tech Stack:** Go 1.27, existing SQLite, Git, CLI model wrapper.

**Spec:** docs/superpowers/specs/2026-10-08-code-review-design.md

## Global constraints

- Complete UTF-8 before/after contents, at most 512 KiB and 64 changed paths; no truncation.
- Five-minute review lease/four-minute model timeout; native UNENFORCED trusted wrappers.
- Strict ACCEPT/REVISE/BLOCK with reason 1..1000 characters.
- No tests rerun, no GitHub writes, no automatic remediation or merge.

## Review focus

- A test-passing change can still be rejected: REVISE/BLOCK must never accept implementation.
- Model inputs can mutate shared slices: freeze canonical bindings outside provider copies.
- Completion/acceptance can fail after paid review: stage and resume without a second invocation.
- Source/candidate can drift while model runs: observe both again before completing.
- Revision/grant can change during review: transactional canonical guards must reject stale acceptance.

### Task 1: Bounded candidate projection and acceptance guard

Files: internal/repoworkspace/review.go, review_test.go; internal/verification/service.go, completion_guard_test.go.
Interfaces: `ReviewCandidate(ctx, dir, Candidate) ([]FileChange,error)` with Path, Before/After *ReviewFile {Mode,Content}; `AcceptTaskWithGuard(ctx, taskID, AcceptanceRequest, func(context.Context,*sql.Tx)error)`.

- [x] Write projection tests for add/delete, exact contents, drift, invalid bytes, size bounds; acceptance rollback test.
- [x] Run targeted tests RED for missing APIs.
- [x] Implement complete pinned blob projection and transactional acceptance guard without altering existing callers.
- [x] Run packages GREEN and commit.

### Task 2: Claimed independent code reviewer

Files: internal/ghtriage/codereview.go, codereview_test.go.
Interfaces: `CodeReviewModel.ReviewCode(ctx, CodeReviewInput) (PlanReviewOutput,error)`; `NewCodeReviewer(...)`; `Review(ctx, caseID, sourceRepo) (CodeReviewResult,error)`.

- [x] Write real workflow tests asserting implementation succeeds only for passing validation+ACCEPT; REVISE/BLOCK stays awaiting verification; replay calls model once.
- [x] Add drift/revision/grant, concurrent callers and interrupted completion/acceptance tests.
- [x] Run RED; implement full chain validation, guarded authority-free Task, lease, copied input, staged verdict, guarded acceptance and saved-result replay.
- [x] Run GREEN plus affected race tests and commit.

### Task 3: Adapter, CLI and delivery

Files: internal/ghtriage/climodel/adapter.go/tests; cmd/summa42-box/ghcode_review.go/tests and main.go; README.md.

- [x] Add strict protocol and invalid startup CLI tests; observe RED.
- [x] Add model adapter and `run-gh-code-review`; emit result JSON and return error for held verdict.
- [x] Document flow, trusted execution and independence limitations; run affected and full tests, race, vet/build/diff.
- [x] Astra review; fix findings with behavioral regressions, publish exact verified tree in PR #20 and observe CI.

## Decisions

Use native implementation and a separate authority-free verification Task on the existing Work. This keeps acceptance independently observable without adding workflow grants. Cost: publication must explicitly consume the exact acceptance in a future step. Linux runtime only; trusted wrapper changes cannot be prevented by this execution profile.

## Review and delivery record

Astra reviewed e2ba930..2758004 with fresh context. No Critical or Minor findings; two Important findings reproduced and fixed in one pass, no final re-review.

- Historical acceptance fallback could swallow current authority rejection. `TestCodeReviewReplayCannotSwallowRevokedGrant` and `TestGuardedAcceptanceReplayRevalidatesAndMatchesRequest` observed RED, then GREEN after guarded acceptance gained exact transactional replay and both catch-all fallbacks were removed. Legacy unguarded acceptance stays one-time.
- An upstream plan-review challenge during model invocation could still accept implementation. `TestCodeReviewCannotAcceptChallengedPlanReview` and `TestCodeReviewCannotAcceptChallengedTriage` observed RED, then GREEN after every guard retained exact accepted upstream Task/Attempt/fence and succeeded state.

Rulings on areas the reviewer declined:

- Live model quality/identity, credentials and actual billing remain untested: wrapper configuration is supervised, separate invocation is the independence contract. Cost: owner must select a suitable reviewer and later verify provider/accounting integrations.
- Trusted native UNENFORCED execution and Git/data drift observations stand; hostile host/repository/database tampering and atomic filesystem protection are outside this profile. Cost: TEB/containment integration is needed before untrusted workloads.
- GitHub publication/merge, automatic remediation, worker composition and operator recovery UX remain next stages. Cost: further integrations are needed for a fully autonomous loop; future publication must consume exact acceptance.
- Linux runtime only; production-scale performance and exhaustive filesystem/OS crash fault injection remain uncertified. Cost: platform/fault/performance gates are needed before claiming those guarantees.
- Parent owns full/race/build gates and environment-forbidden Unix socket checks; reviewer ran only two overlay reproductions. Cost: final CI must remain a separate gate. No final re-review or deferred minors.

Final verification and published-head CI results are recorded in the PR description.
