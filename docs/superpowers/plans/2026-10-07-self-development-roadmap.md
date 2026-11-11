# Summa42: project state and the path to self-development

> For the implementer: work through the tasks in order, following Superpowers `executing-plans`. Before implementing, compare the current HEAD with the base of this report. Every stage needs its own verification and review. This document proposes an architecture and an order of implementation; it does not start autonomy and it grants no new authority.

**Date:** 7 October 2026. **Base of the analysis:** `SofiaFlux/summa42`, `main`, commit `5a633000e849d3f49f5281314a30ebac9e08f73d`.

**Goal:** Summa42 picks work from an authorized backlog on its own, prepares a change in its own repository, tests it, obtains an independent review, handles the PR, CI and issues, and then, within the permitted scope, merges the change and deploys the new version with a way back.

**Architecture:** one Maintainer Collective on the existing single-Cube kernel. A deterministic driver runs a durable WorkflowCase, models do bounded cognitive tasks, and separate components handle checkout, tests and external effects. The existing Task/Attempt, fencing, evidence, policy, approvals, resources and External Operations remain the foundation.

**Stack:** Go 1.27, SQLite, the existing OPA, Git, the GitHub API, the existing executor adapter; Linux/OCI where technical isolation is needed.

**Scope:** analysis and plan. No code, issue, GitHub setting or deployment was changed.

## Implementation update — 8 October 2026

PR #19 delivered the first foundation of executor eligibility. The open draft PR #20 extends it with supervised source preparation and plan review, local implementation with real tests, independent candidate review, and protected publication of the exact commit as a branch and a draft PR. Publication needs separate Case grants, uses two ExternalOperation slots, keeps the Owner's approvals and does not retry uncertain writes. See the [publication plan](2026-10-08-github-publication.md).

These are still supervised commands, not an automatically composed worker. Today Summa takes no part in live work or publication. The next stage is an observer of CI results, independent of the implementer and bound to the exact candidate SHA, with durable evidence and verification of the publication. Only after that come bounded repair after review/CI, owner-approved merge and handling of issue state. PR #20 stays a draft; the publication stage grants no right to merge or to close issues.

The original analysis below describes the state at the stated base and remains the historical reference.

## Update — 10 November 2026

A review of the whole repository (`main` @ `c3d5454`) and the change described
in the
[specification](../specs/2026-11-10-safety-controls-and-vision-alignment-design.md).

**Where we are on the A0–A3 ladder.** A1 has been reached in one case since the
21 October remediation: issue #115 → PR #131, ten days and at least six Owner
interventions. A2 was reverted on 21 October. A3 has not been started. The
condition from stage 6 ("10 small A1 pilot tasks") stands at 1 of 10.

**What happened between 6 and 10 November.** 29 commits and about 25,600 lines
entered `main` from outside the pipeline, that is, in A0 mode. Some of them
closed "Post-MVC" epics with code that no running path calls. That code is now
parked outside `main`; the index with commits and restore commands is in
[parked spikes](../research/2026-11-10-parked-spikes.md).

**What was fixed.** The dead man switch has a horizon set by the Owner (seven
days by default) instead of ten leases; break glass closes leasing and leaves a
trail in `execution_events`; both are available as `summa42 emergency …` with
the Owner's signature. An issue's revision moves only on a maintainer's comment
or a change of the issue's content.

**Vision.** `docs/VISION.md` gained a section on the destination, articles
27–29 and a seventh question. The description of a blue-green handover of
`control.sock`, added by PR #23, was removed from it: it was an implementation
detail and contradicted stage 7 below (a separate supervisor; a running Box does
not replace its own binary). The text is in history at `c3d5454`.

**Order from here** (the stages below are unchanged, only the order in which
they are entered):

1. A required CI check on `main` and no merge without green CI (a repository
   setting, on the Owner's side).
2. The nine remaining small A1 tasks through the pipeline, measuring
   interventions and cost per task (section 9).
3. Continuity (Vision, Article 28): a budget in a currency, notifications to the
   Owner, tested backup and restore, a watchdog outside the process.
4. Stage 6 (A2), then stage 7 (A3).
5. The [Company OS](../../COMPANY_OS.md) profile: the first wiring of obligations
   into the scheduler.

A new package enters `main` only together with a call from a running Box and an
acceptance test over that path.

## 1. Conclusion

Summa42 already has a large part of its control and durability mechanics. The nearest obstacle is concrete: the GitHub flow reaches a plan and a `github.issue.plan.review` proposal, and then there is no executor and no routing contract to continue. The next missing pieces are repository preparation, executing and independently verifying a change, publishing a PR, and feeding CI/review results back into the workflow.

The shortest path is to close one development cycle. Company OS, A2A, a distributed runtime, autonomous provider acquisition and elaborate dreaming are not conditions for the first independently prepared PR.

Recommended first level: Summa42 prepares a verified PR and handles fixes, and the Owner approves the merge. Target level: an owner-approved policy lets it merge named classes of change on its own; a separate supervisor deploys the verified build. The human deals with exceptions and direction instead of every step of the work.

## 2. What I confirmed

| Area | State at the examined HEAD | Meaning |
|---|---|---|
| Kernel | Task/Attempt, leases/fencing, evidence, acceptance, approvals, operations, resources, run manifests | The foundation for a durable process already exists. |
| Worker | `internal/scheduler/worker.go`, `run-worker` | Can run registered executors. The README still claims the worker does not start. |
| GitHub intake | `internal/ghissue`, `run-gh-intake` | Reads open issues, filters authors/assignees/labels, records revisions and cases. The client exposes GET, no writes. |
| Triage | `internal/ghtriage` | Rules → bounded classification by a model → deterministic disposition. Large/unknown scope leads to needs-human. |
| Triage review | `ghtriage.Reviewer` | Checks structure, state consistency and plausibility of the classification. It is not a review of the plan or the code. |
| Planner | `ghtriage.Planner`, `run-gh-plan` | Records the plan as evidence and proposes `github.issue.plan.review`; does not materialize the next Task. |
| Plan | `ghtriage.PlanInput/PlanOutput` | Issue snapshot + decision, summary and 1–8 steps. No explicit code snapshot, base SHA, file scope or test contract. |
| Coding executor | `internal/executors/codex.go` | The adapter exists. The examined `run-worker` composition registers triage/Copilot/ADO publish; it does not wire Codex as the issue implementer. |
| Workspace | `Worker.executeAttempt` | Creates the Attempt directory with `MkdirAll`. Does not check out the repository. |
| GitHub writes | `feedbackgithub` | Protected creation of a sanitized feedback issue exists. No general branch/push/PR/review/merge cycle for self-development. |
| ADO | `adoreview`, `adoeffects` | Patterns for a driver, a publisher and a final verifier exist. The semantics can be reused, but the GitHub provider needs its own implementation. |
| CI | Run `36696570907` for the examined SHA | Green test, race, vet, build, bootstrap smoke, SQLite spike and OCI acceptance. |
| Repo | Public, only branch `main`, protected=true, auto_merge=false | No open PR. Branch protection details are not available to the integration: 403. The ruleset list is empty. |
| Backlog | 12 open issues, all without labels and assignees | The backlog holds mostly large directions; it is not yet a queue of small executable tasks. |

PR #6 and #7 were merged on 22 September. The old note in their descriptions about CI not having run does not describe the current HEAD: the latest checked run on main succeeded on 30 September.

**Limits of the evidence:** I analyzed the code, documents, backlog, PRs and GitHub Actions results. I did not run Summa42 on your server, with a model or on a real working repository. Green CI confirms the current test suite; it does not prove that a future issue → code → PR → merge cycle works. I did not run the whole Go matrix locally.

## 3. Gaps the plan must cover

### P0 — control, isolation and accounting

1. **Routing is not a hard executor qualification.** The Worker passes all registry kinds to `ChooseExecutor`. `TaskClassRouting` acts as a preference, and the fallback is alphabetical. For the future workflow a missing matching executor must block the work, not start another one. Capabilities should be assessed per executor and per Task.
2. **Capacity overstates the declared enforcement level.** `workerCapacity` assigns ENFORCED to all registry kinds. That does not prove technical containment of the process. The existing Codex adapter distinguishes PARTIAL/ENFORCED; the scheduler should consume the real assessment of the given executor and its capabilities.
3. **Model calls have no uniform lifecycle.** The planner and the reviewer call the model outside ordinary worker execution. `climodel.Adapter` inherits the process environment and sets no controlled workspace. A read-only capability in metadata is not proof that the started CLI has no tools, network or ambient credentials.
4. **Usage is not full accounting.** The Codex parser returns token usage, but the worker records evidence and a CompletionManifest without settling that Usage. The planner CLI returns no canonical usage either. Workflow RemainingBudget must not be read as USD without a documented conversion.
5. **Lease and length of work.** The default lease is 5 minutes, and the Worker bounds the whole execution by it. Implementation and tests need a deliberate timeout or lease renewal that preserves fencing. Simply setting a very long lease makes abandoned work harder to detect.
6. **Workflow limit.** Intake sets MaxSteps=3 by default. A full review/implementation/fix cycle needs a larger, still bounded, step budget.

The above are statements about the examined code path and the risks of extending it. I do not claim to have observed a containment breach or an unauthorized write in a running environment.

### P1 — missing execution

- The plan is based on the issue, without explicitly supplied repository context. That is enough for a draft; not for authorizing an implementation.
- Plan review, materialization of an implementation Task and binding of the patch to the accepted plan are missing.
- An independent test run for the exact candidate SHA and a code review of the same diff are missing.
- Protected GitHub writes and reading PR/checks/review threads as new process input are missing.
- The intake/worker/driver/reviewer/planner commands are separate modes; there is no complete supervised maintainer that runs the whole cycle by itself.

### P2 — autonomy after the first PR

- The current intake filter requires an author from the maintainer list. A bot creating its own issues needs explicit endorsement/provenance; adding all authors to the allowlist would be too broad.
- An issue's `updated_at` is the revision ID today. The bot's maintenance comments and labels can change that revision. A content/spec fingerprint is needed that tells a changed requirement from the bot's own status update.
- An external supervisor of the new Box version, a canary and a verified rollback are missing, also for SQLite schema changes.

## 4. Architecture decisions

I considered three paths:

| Path | Advantage | Cost/limitation | Decision |
|---|---|---|---|
| An agent CLI drives everything through the shell and gh | The fastest demonstrator | Bypasses canonical operations, recovery, authority and accounting | May serve for a manual bootstrap under supervision; not as the target maintainer. |
| A maintainer workflow on the current kernel | Uses the existing durability and governance | The missing adapters and contracts have to be built | **Recommended.** |
| A full Company OS/distributed swarm first | A wide range of future capabilities | Delays the first useful cycle and adds dependencies | Off the critical path. |

The chosen division of responsibility:

- **Driver:** picks the next permitted step from canonical evidence and state.
- **Planner/implementer/reviewer:** cognitive roles; run sequentially at first, with separate contexts. Independent review does not have to mean another provider, but it does not rely on the author's unverified assurance of correctness.
- **Workspace and test runner:** deterministic preparation of the repository and checking of the result.
- **GitHub publisher:** carries out exactly specified effects through External Operations. The model gets no token for push/merge.
- **Supervisor:** outside the process of the Box being developed; activates a version and restores the previous one.

Do not build a new agent framework. Use one already available executor first. A Claude adapter or another provider is an independent extension if your way of working needs it.

## 5. Work contract

Every implementation task binds:

`repo + issue + spec_fingerprint + plan_hash + base_sha + allowed_paths + acceptance_commands + budget_envelope + policy_version`.

An implementation artifact binds:

`task_id + attempt_id + fence + base_sha + candidate_sha + diff_hash + changed_paths`.

Verification binds:

`candidate_sha + test_configuration_hash + command_results + reviewer_verdict + unresolved_findings`.

A change of SHA or of the requirements invalidates the corresponding approval. A model's CLEAN verdict, a stdout "tests pass", valid JSON or exit code 0 are not enough for Task acceptance.

**Target flow:** observe the issue → qualify → checkout and context → plan → plan review → implementation → deterministic tests → independent code review → fixes within limits → push/PR → read CI/review → merge gate → merge → close the issue → optional deployment and feedback.

Working with Git means: an isolated checkout at a specific SHA, diff validation, commits, publication of a branch/PR, updating a branch against a specific remote SHA, conflict handling and archiving. No unconditional force-push, no writing directly to main, no automatic reset of someone else's work.

## 6. Global constraints and autonomy thresholds

**A0 — bootstrap:** an external agent implements the missing parts; Summa42 cannot yet do them itself.

**A1 — issue → verified PR:** the task and its fixes are carried out independently; a human approves the merge. This is the first milestone.

**A2 — policy merge:** the Owner approves classes of change, paths, costs and gates in advance. Only permitted changes may be merged independently. Branch protection still applies.

**A3 — self-upgrade:** a separate supervisor deploys a verified release; health check/canary and rollback. This is a second, separate problem after source-code self-development.

For the first pilot I propose:

- One repo, `SofiaFlux/summa42`, and one active development case; the rest can wait in the backlog.
- Positive queue selection through `summa:ready`, hold through `summa:hold`; labels are a projection, the canonical grant stays in Summa42.
- MaxSteps=24 per development case; at most 2 retries of an infrastructure error in a given step and 3 repair cycles after findings. All bounded by a shared budget.
- Proposed initial API limit: 5 USD per case and 20 USD a day. These are pilot settings for calibration, not an estimate of the cost of the work. An internal monetary ledger in integer micro-USD; unknown usage stays exposure. With a subscription, measure usage/quota separately; do not pretend the cost is 0.
- Enforce the time and cost limit before the next call. If the provider offers no hard cap, declare enforceability explicitly and reserve a conservative exposure.
- At first the paths for owner keys, policy/authority, approvers, spending configuration, deployment credentials and branch protection require the Owner. Changes to GitHub Actions execution also go to a separate review before A2.
- The agent may report that it needs a wider scope; it may not grant it to itself. Lack of progress, an unknown outcome and unmet gates have a visible reason and a time of the next action.

## 7. Execution plan — independently accepted stages

Carry out every package as: a test reproducing the requirement → confirm FAIL → minimal implementation → PASS → review → commit. The names of new files and components below are a proposal for implementation, not a description of existing code. The stages need a detailed spec limited to their scope, without redesigning the whole kernel.

### Stage 0: a current picture of the system and the pilot queue

**Files:** `README.md`, a new `docs/maintainer-runbook.md`, `cmd/summa42-box/main.go` only as far as documenting existing modes.

- [ ] Add the existing worker/intake/triage/review/plan commands to the README, and the exact point where the current flow ends.
- [ ] List the configuration of the model, repo, grants, resource envelope and the required permissions without revealing credentials.
- [ ] Choose 3–5 small pilot tasks: documentation of the current commands, a small bug with a reproduction and a test, a minor CLI improvement. Split large issues into child issues with one result and explicit dependencies.
- [ ] In the runbook give start/stop/status, backup, and how to read why a case is blocked. Check the GitHub App and the actual required checks/reviews in the administrative settings this integration cannot access.

**Acceptance:** a second operator can say which components a given command really starts and what it does not do yet. The queue holds measurable, small tasks.

### Stage 1: a hard executor contract and accounting

**Change:** `internal/scheduler/worker.go`, `service.go`, `routing.go`, `internal/executors/executor.go`, `internal/resources/service.go`, `cmd/summa42-box/main.go`.

**Add:** `internal/scheduler/eligibility.go`, `internal/executors/accounting.go` and the corresponding tests.

- [ ] Define an executor descriptor: supported TaskClass, semantic capabilities, actual enforcement and the cost/usage contract. Compute eligibility for the specific Task before preference and Lease.
- [ ] A missing matching executor returns unschedulable with a reason. Do not pick an alphabetical fallback for a class that requires an explicit contract.
- [ ] Capacity comes from the assessment of the specific executor; a partially enforced process does not become ENFORCED by being entered in the registry.
- [ ] Put all production triage/planning/review calls under governed execution: resource reservation, Attempt provenance, timeout, cancellation, usage settlement. Manual commands may remain as diagnostic tools.
- [ ] Provide a controlled environment/CWD/tool access for the model adapter and no Owner key/GitHub write token in the cognitive process.
- [ ] Introduce lease renewal or a deterministic split of long work; cancellation covers the whole process tree. An old fence must not publish a result.

**Tests:** `TestMissingExecutorBlocksClass`, `TestPartialExecutorCannotSatisfyEnforcedTask`, `TestUsageSettledOnceAfterRestart`, `TestUnknownUsageRetainsExposure`, `TestExpiredFenceCannotPublish`, `TestModelCannotReadOwnerMaterial`.

**Verification:** `GOCACHE=/tmp/summa42-full-go-cache go test ./internal/scheduler ./internal/executors ./internal/resources ./cmd/summa42-box -count=1 -timeout 5m`; result PASS. In addition, containment acceptance in the target environment, because mocks alone do not prove isolation.

**Acceptance:** a task never reaches the wrong executor, and model calls do not bypass the budget or durability.

### Stage 2: repo snapshot, plan review and implementation handoff

**Change:** `internal/ghtriage/plan.go`, `planner.go`, `planning.go`, `internal/workflowcase/materialize.go`.

**Add:** `internal/workspace/git.go`, `manifest.go`, `internal/maintainer/plan_review.go`, `contracts.go` and tests.

- [ ] The workspace prepares an isolated checkout at the base SHA; the source copy of the repo is not a write directory shared between Attempts. The repo URL comes from approved configuration.
- [ ] Give the planner an explicit context manifest: HEAD, AGENTS, relevant files and tests, requirement/dependency documents and a size limit. Version and hash this input.
- [ ] Add plan schema v2 with the base SHA, scope/allowed paths, acceptance commands and steps. Keep the ability to read old v1 evidence; do not modify it.
- [ ] Implement `github.issue.plan.review`: an independent model review plus a deterministic check of scope/grant/budget/commands; outcomes ACCEPT, REVISE, BLOCK with evidence.
- [ ] ACCEPT materializes `github.issue.implement` through the existing WorkflowCase contract, only for the current revision and the right plan_hash. Plan review is not a new source of authority.
- [ ] Raise the limits of the development recipe explicitly; do not change the limits of other recipes globally.

**Tests:** `TestCheckoutPinnedToBaseSHA`, `TestPlanReviewRejectsScopeExpansion`, `TestPlanV1RemainsReadable`, `TestNewIssueSpecInvalidatesHandoff`, `TestMissingPlanReviewerDoesNotMaterializeImplementation`.

**Verification:** tests of `./internal/workspace ./internal/ghtriage/... ./internal/maintainer ./internal/workflowcase`, count=1, timeout 5m.

**Acceptance:** an accepted, code-grounded plan creates exactly one implementation work; a repeated tick/restart creates no duplicate.

### Stage 3: implementation, independent tests and code review

**Change:** `internal/executors/codex.go`, `internal/scheduler/worker.go` only through new explicit contracts.

**Add:** `internal/maintainer/implement.go`, `verify.go`, `code_review.go`, `repair.go`, `internal/workspace/diff.go`, `tests/acceptance/maintainer_development_test.go`.

- [ ] Wire the implementer into the task registry/routing. The Objective contains the accepted plan, and the Payload/context carries the hash-bound snapshot and criteria; do not rely on the general codexPrompt alone.
- [ ] The agent changes files locally without external write credentials. A trusted wrapper collects and validates the diff: allowed paths, symlinks, submodules, size, secrets and agreement with the base SHA. It creates an immutable candidate artifact/commit.
- [ ] The verifier prepares a fresh checkout of the candidate SHA and runs the approved tests itself. A repository with agent-written test code is executable input: the runner has no Owner keys, GitHub write token or other ambient credentials.
- [ ] Code review reads the exact diff/base/candidate and the test results. Findings must name the file, the reason and the evidence; UNCERTAIN blocks acceptance.
- [ ] Repair creates a new Attempt on a new candidate; test/review evidence of the old SHA is not carried over as approval of the new one. After 3 cycles or no progress the work blocks with a diagnosis.

**Tests:** `TestOutOfScopePatchRejected`, `TestAgentTestsPassClaimIgnored`, `TestReviewBoundToCandidateSHA`, `TestRepairCannotReuseOldAcceptance`, `TestNoProgressStopsWithinBudget`, `TestTestRunnerHasNoPublisherCredentials`.

**Verification:** package tests of maintainer/workspace/executors and acceptance. For Go changes the full `go test`, `go test -race`, `go vet`, `go build`; for kernel changes also the existing persistence/OCI gates.

**Acceptance:** a real model fixes a small reproducible bug, an independent runner confirms the regression test and the suite, and a reviewer assesses the same candidate SHA. At this stage a verified local change exists.

### Stage 4: protected GitHub publisher and milestone A1

**Add:** `internal/githubeffects/client.go`, `provider.go`, `intents.go`, `reconcile.go`, `internal/maintainer/publish.go`, `pr_observer.go` and tests.

**Use:** `internal/operations.Provider`, stable effect slots, reservations and LookupOutcome; the `feedbackgithub` and `adoeffects` patterns.

- [ ] Expose typed effects: creating a branch/ref, publishing the named candidate commit, creating/updating a PR, a status comment, labels and closing an issue. Every effect has its own capability and canonical intent.
- [ ] The publisher checks the repo, the branch prefix, the base/candidate SHA and the acceptance evidence; the token stays in the trusted component. Any Git transport is controlled by the publisher, not by the model's shell.
- [ ] A ref update requires the expected remote SHA. Object/blob publication can be deduplicated by content hash; a visible branch/PR effect requires a read and reconciliation after an uncertain result.
- [ ] The PR contains the issue linkage, a description of the behavior, the candidate SHA, test/review evidence and limitations. Read checks, reviews, threads, branch state and merge status.
- [ ] Findings/failed CI return to a bounded repair workflow. Tell a CI infrastructure error from a code regression; do not repair code on the strength of a runner timeout alone.
- [ ] The bot's own status comments/labels do not cause endless new triage/planning. The spec fingerprint covers a change of requirements and authorized decisions, not every touch of updated_at.

**Tests:** `TestPRCreateTimeoutReconcilesWithoutDuplicate`, `TestPushRequiresExpectedRemoteSHA`, `TestCommentUpdateDoesNotRestartImplementation`, `TestChangedRequirementInvalidatesCandidate`, `TestFailedCIProducesBoundedRepair`, `TestIssueNotClosedBeforeVerifiedDelivery`.

**Acceptance of A1:** from one owner-endorsed issue to a verified PR with CI and at least one findings → repair cycle handled. A restart between the push and the recording of the result creates no second PR and no second effect.

### Stage 5: one supervised maintainer and backlog management

**Add:** `internal/maintainer/driver.go`, `recipe.go`, `intake.go`, `status.go`, a separate `cmd/summa42-box/maintainer_cmd.go`, `docs/maintainer-runbook.md`.

**Change:** `internal/ghissue` and the existing control/status only as far as needed; keep the new CLI out of further growth of `main.go`.

- [ ] `run-maintainer` runs all stages through canonical transitions. Every tick has a durable reason for the next action; the scheduler does not depend on firing 5 commands by hand.
- [ ] Ensure one coordinator of state transitions/publishing per Collective. An executor may run separately; do not assume that arbitrary processes opening the same SQLite automatically give single-writer orchestration.
- [ ] Heartbeat/status shows the active case, step, candidate SHA, budget, exposure, blocked reason and next retry. Pause holds new work; stop cancels processes and removes the ability to dispatch further.
- [ ] Backlog qualification: ready + endorsement + dependencies + permitted scope; recommended priorities: reproducible bugs, missing tests/gates, small features, refactoring tied to a task.
- [ ] Summa may split a large issue, detect a duplicate and create a child-issue proposal. Auto-created work becomes executable only under an approved generator/policy, with provenance and within the allowlist. A bot author by itself confers no authority.
- [ ] Bound polling/backoff, handle the GitHub rate limit, a PR closed by hand, an issue closed/reopened, a base conflict and a parallel human change. Labels are a projection, not the source of truth.

**Tests:** a restart at every transition; `TestTwoDriversDoNotPublishTwice`, `TestUnendorsedBotIssueNotExecuted`, `TestHumanEditSupersedesPlan`, `TestClosedIssueStopsPendingWork`, `TestRateLimitBackoff`, `TestPausePreventsNewDispatch`.

**Acceptance:** 24–48 hours of the pilot without pushing stages along by hand, with an explicit report of every intervention. Then 7 days of dogfooding to calibrate cost and blocks.

### Stage 6: policy merge, milestone A2

**Add:** `internal/maintainer/merge_gate.go`, a typed merge effect in `githubeffects`, a separate owner-approved policy profile and tests.

- [ ] Carry out 10 small A1 pilot tasks; none may be accepted by skipping the required gates. Record interventions, costs and reopens/regressions.
- [ ] The gate requires the current candidate SHA, green required checks, a review of that SHA, resolved blocking findings, a current plan/policy, permitted paths and no unknown external effects.
- [ ] The merge is done by the GitHub provider with an expected head SHA condition. If main has moved and that affects the result, the branch update produces a new candidate and a new verification; it does not inherit the old CLEAN.
- [ ] Confirm the merge commit and its relation to the plan/candidate. Close the issue after confirmed delivery according to its acceptance, not after merely sending the merge request.
- [ ] Take the actual branch protection into account. If the required approval must come from an account/principal other than the PR author, use an approved review identity or stay at A1; do not weaken protection to get around the limit.

**Tests:** `TestNewHeadInvalidatesMergeApproval`, `TestPendingOrMissingRequiredCheckBlocksMerge`, `TestUnresolvedReviewBlocksMerge`, `TestMergeTimeoutReconcilesCommit`, `TestProtectedPathNeedsOwner`.

**Acceptance of A2:** three small, permitted changes pass the full cycle without intervention in the routine steps; a protected change correctly waits for the Owner.

### Stage 7: self-upgrade, milestone A3

**Add:** a separate `cmd/summa42-supervisor`, `internal/upgrade/manifest.go`, `health.go`, `rollback.go`, `docs/upgrade-runbook.md`.

- [ ] A build/release from the exact merged SHA has a hash and a declared DB/config compatibility range. A running Box does not overwrite its own binary while it works.
- [ ] The supervisor stops new leases, finishes or cancels active Attempts and reconciles unknown effects before changing the version; work state stays durable.
- [ ] Back up SQLite through a supported snapshot/backup with correct WAL handling. Run the upgrade and restore test on a copy with active tasks and operations.
- [ ] The canary checks startup, policy provenance, reading data, scheduling and a simple synthetic case. Not only the response of the status endpoint.
- [ ] On failure start the previous build only on a compatible database. Do not restore an old DB after the new version's external effects without reconciliation: that could replay operations already performed.
- [ ] For breaking migrations require an owner-reviewed migration/rollback plan. Prefer compatible expand-contract migrations so that a binary rollback does not require rewinding the history of operations.

**Tests:** `TestBadBuildRollsBackBeforeEffects`, `TestRollbackDoesNotReplayPublishedPR`, `TestMigrationIncompatibilityBlocksUpgrade`, `TestUpgradePreservesPendingCases`, `TestSupervisorSurvivesBoxFailure`.

**Acceptance of A3:** Summa deploys its own permitted change, performs the next task on the new version and passes a controlled failure/rollback test without losing canonical state and without duplicating effects.

## 8. Relation to the current issues

| Issue | Decision for self-development |
|---|---|
| #1 field dogfooding | Much of the code is already on main. Verify the acceptance criteria and a live smoke before updating the status; use the observations for the backlog. Do not assume full closure automatically. |
| #14 Summand vocabulary | A candidate for a small docs pilot after strictly limiting its scope. A rename alone is not proof of the ability to implement a bug fix. |
| #4 replay/regression | Extract the minimal slice: a replay and failure/restart harness for one maintainer workflow. A full replay system can wait. |
| #5 rogue containment | The pilot needs the minimum of pause/stop/revoke/credential separation; the full rogue detection layer later. |
| #15 learning/dreaming | Start with result → evidence → issue proposal. Automatic changes of process/policy need separate gates; full dreaming after a working cycle. |
| #12 intent compiler | For now a structured issue contract and simple decomposition. Do not build a full compiler before the first PR. |
| #2 capability discovery | Not on the critical path. Choose an existing executor; acquisition of new tools is not a condition of operation. |
| #3 OSS/control plane | Keep the architecture and assess specific dependencies as needed; do not rewrite as part of the bootstrap. |
| #11 DTU | Deterministic fake providers and failure injection are enough for the first cycle. |
| #16 decision engine | The current rules + model interfaces are enough; a future engine may replace a specific role. |
| #17 A2A, #18 Company OS | Outside stages 0–7. A Maintainer Collective can work without the Company OS profile. |

Proposed new epic: **Maintainer Collective — bounded issue-to-release self-development**. The child work corresponds to stages 1–7, with dependencies; let only small tasks with working prerequisites into the ready queue. I did not create these issues in this analysis.

## 9. Success criteria and the operational report

The minimum report after every case: issue/plan/candidate/PR/merge SHA, test and review evidence, measured cost and unknown exposure, retries, waiting time, and the number and reason of human interventions.

Measure:

- the share of authorized small issues that end in a verified PR;
- the share of changes merged and not needing a reopen/regression fix;
- Owner interventions per task, split into product decisions and technical pushing of the process;
- cost per accepted change, including all failed Attempts;
- duplicate external effects — target 0;
- blocked cases without a reason or a next action — target 0;
- the number of bounded repairs and repeated findings.

The statement "Summa42 develops Summa42" is justified at A1 for producing changes/PRs. The statement "it maintains the repo and its own releases by itself" requires A2 and A3. The vision of about an hour of attention a month needs a longer period of stable operation; a 7-day pilot is not such evidence.

## 10. Main places to review

1. An old SHA/revision/fence trying to approve or publish a newer result — stages 1/2/3/6.
2. A timeout after an external effect was performed — stages 4/6/7: read and reconcile instead of a blind retry.
3. A model/test runner inheriting authority or credentials — stages 1/3.
4. The bot's own comments, labels and bot-generated issues causing loops or unintended authorizations — stages 4/5.
5. A DB upgrade/rollback replaying effects performed earlier — stage 7.

Do not widen coverage "just in case": every test should reproduce a specific contract condition or a risk named above.

## 11. Sources and reproducibility of the analysis

All source paths in sections 2–3 were read at the base SHA. Repository: https://github.com/SofiaFlux/summa42/tree/5a633000e849d3f49f5281314a30ebac9e08f73d

- CI and the steps of both jobs: https://github.com/SofiaFlux/summa42/actions/runs/36696570907
- The planner and the existing decision to stop at the handoff: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/docs/superpowers/plans/2026-09-29-github-issue-planner.md
- Planner contract: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghtriage/plan.go
- Worker/workspace: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/scheduler/worker.go
- Startup/routing/capacity: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/cmd/summa42-box/main.go
- Model adapter: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghtriage/climodel/adapter.go
- GitHub read-only intake: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/ghissue/client.go
- External-operation contract: https://github.com/SofiaFlux/summa42/blob/5a633000e849d3f49f5281314a30ebac9e08f73d/internal/operations/provider.go
- Backlog: https://github.com/SofiaFlux/summa42/issues

**Next concrete task:** stage 1 — hard executor eligibility, real capacity and accounting — in parallel with the documentation clean-up of stage 0 only if the implementers have independent file scopes. Then stage 2 closes the current `github.issue.plan.review`. The first run of external GitHub writes comes only after these contracts and the local cycle from stage 3 are accepted.
