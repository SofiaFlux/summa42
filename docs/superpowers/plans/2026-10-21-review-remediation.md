# Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix every finding of the 2026-10-21 full review (16 Critical, 43 Important, ~70 Minor) so the Box is again the only writer of canonical state and the only path to external effects, and so the GitHub issue pipeline can actually run from triage to a published PR.

**Architecture:** Delete rather than patch where the review found fabricated or authority-widening code: the Python reconciler, the fake model adapters, the merge stack, capability acquisition, and `AUTHORITY_PROMOTION`. A single Go subcommand, `run-gh-pipeline`, replaces the reconciler and calls the existing idempotent `run-gh-*` stages in-process. Fix the remaining Go defects in place, each with a regression test. Data cleanup of the live `~/.summa42` and GitHub happens last (Phase F) and is **not** executed without the owner's go-ahead.

**Tech Stack:** Go 1.27 (modernc sqlite, goose migrations, OPA/rego), Python 3 stdlib (call-worker, query), Node (ACP bridge).

**Spec:** README.md, docs/VISION.md, docs/superpowers/specs/2026-09-14-summa42-design.md. Review reports (the findings this plan cites by ID): `/tmp/claude-1000/-home-topol-git-summa42/57918b97-d967-4bc6-9488-24a2d11600e7/scratchpad/review/{scripts,cmd,ghtriage,core,caps}.md`. Every implementer reads the report sections for the IDs in their task.

## Global Constraints

- Go commands: `GOCACHE=/tmp/summa42-full-go-cache go …`. Long gates need explicit `-timeout`. `internal/workflowcase` costs ~12 s per `-count=1`; never multiply the whole package by a high `-count`.
- The concurrency gate is `go test -run TestCloseConcurrentReplay ./internal/workflowcase -count=600 -timeout 30m`. Run it at the end of each Phase B task that touches `workflowcase`.
- Nothing in this plan writes to `~/.summa42`, to GitHub, or runs the live Box. Tests use `testutil.OpenStore` and httptest servers.
- Only Box Go code writes canonical SQLite tables. After Phase A, no script may open the DB read-write.
- Every external write goes through `operations.Service` (PREPARED → DISPATCHED → reconciliation).
- New migrations are numbered from `00020`. They must apply cleanly to an existing DB that contains Python-written rows (non-RFC3339 timestamps, `json.dumps` spacing, `issue_hierarchy` table).
- Deletion over addition. A `ponytail:` comment marks every deliberate simplification with a known ceiling.
- Commit after each task, with the attribution lines from the session. Work on branch `fix/review-remediation`; never commit to `main`.
- Full verification at the end of each phase: `go vet ./...`, `go test ./... -count=1 -timeout 30m`, `go test -race ./... -count=1 -timeout 45m`, `gofmt -l .` (empty after Task 29), `python3 -m py_compile scripts/*.py`, `node --check scripts/summa42-acp.js`.

## Review Focus

These conditions are implied by the spec but no single finding pins them. Each line names the task that owns its test.

1. **Upgrading a live Collective:** after the migrations and policy changes land, an existing `~/.summa42` DB (policy v1 active, Python-written rows) must still start. `summa42 policy upgrade` must activate v2. → Task 20, `TestUpgradeFromV1CollectiveStarts`; Task 23, `TestMigrationsApplyOverLegacyRows`.
2. **Model backend unavailable, slow or rate-limited:** every review stage must end in a recorded failure or a hold, never ACCEPT and never a stuck EXECUTING task. → Task 2, `test_all_backends_fail_exits_nonzero`; Task 9, `TestTransientModelFailuresDoNotBlockPermanently`.
3. **Box daemon running while `run-gh-pipeline` runs** (the normal deployment): neither process may crash on `SQLITE_BUSY`. → Task 19, `TestConcurrentProcessesWriteWithoutBusyCrash`.
4. **Prompts larger than ARG_MAX:** an argv-only backend must reject them with a clear error, and must not silently drop grounding. → Task 2, `test_argv_backend_rejects_oversized_prompt`.
5. **Owner commits to the developer checkout mid-pipeline:** a case between implementation and publication must not be permanently killed by that commit. → Task 14, `TestOwnerCommitBetweenStagesDoesNotKillCase`.

---

## Phase A: stop the bleeding (scripts layer)

### Task 1: Delete fake model adapters and alias scripts (S-C6)

**Files:**
- Delete: `scripts/agy-model.py`, `scripts/opencode-model.py`
- Delete (untracked, local): `bin/agy-model`, `bin/opencode-model`
- Modify: any doc or script that references them (`grep -rn "agy-model\|opencode-model"`)

- [ ] **Step 1:** Run `grep -rn "agy-model\|opencode-model" --exclude-dir=.git .` and record every reference.
- [ ] **Step 2:** Delete the four files. Replace each reference with `bin/call-worker --backend agy` or `--backend opencode`.
- [ ] **Step 3:** In this plan's Phase F list, record that the verification evidence of PR #21 (commit 84485a6) came from canned output.
- [ ] **Step 4:** Run `grep -rn "agy-model\|opencode-model" --exclude-dir=.git .`. Expected: no hits outside `docs/superpowers/plans/2026-10-21-*`.
- [ ] **Step 5:** Commit: `chore: delete canned fake model adapters`.

### Task 2: Make call-worker fail closed (S-C4, S-I1 prompt part, S-I4, S-I5, S-M6–S-M9)

**Files:**
- Modify: `scripts/call-worker.py`
- Create: `scripts/test_call_worker.py` (stdlib `unittest`; fake backends are tiny shell scripts written to a temp dir)

**Interfaces:**
- Produces the CLI contract: read one JSON request on stdin, write exactly one JSON response on stdout, exit 0. On any failure, exit non-zero with a one-line reason on stderr. The Go `climodel` adapter already treats a non-zero exit as an attempt failure.
- Task type is decided by an exact `schema` constant, using the same strings Go uses (`github.issue.plan.review.v1` and so on, read from `internal/ghtriage`). An unknown schema is an error.

- [ ] **Step 1: Write the failing tests** in `scripts/test_call_worker.py`:
  - `test_all_backends_fail_exits_nonzero`: every backend exits 1. Assert return code ≠ 0 and empty stdout.
  - `test_missing_verdict_is_error` / `test_unknown_verdict_is_error`: backend returns `{}` or `{"verdict":"MAYBE"}`. Assert non-zero exit.
  - `test_plausible_requires_json_bool`: `{"plausible":"false"}` gives non-zero exit; `{"plausible":false}` passes through unchanged.
  - `test_accept_reason_with_violation_word_stays_accept`: `{"verdict":"ACCEPT","reason":"no violations found"}` passes through unchanged.
  - `test_argv_backend_rejects_oversized_prompt`: an argv-only backend with a 200 KiB prompt gives non-zero exit, and stderr contains `prompt exceeds argv limit`.
  - `test_stdin_backend_receives_full_prompt`: the fake backend writes its stdin to a file. Assert the file contains the grounding context verbatim.
  - `test_no_github_credentials_in_backend_env`: run with `GITHUB_TOKEN=x GH_TOKEN=y`. The fake backend dumps `env`. Assert neither variable is present.
  - `test_openrouter_off_by_default`: an OpenRouter key is present but `--allow-remote` is not set. Assert OpenRouter is never invoked.
  - `test_unknown_schema_is_error`.
- [ ] **Step 2:** Run `python3 -m unittest scripts/test_call_worker.py -v`. Expected: the failures above.
- [ ] **Step 3: Rewrite the dispatch path.**
  - Delete every default verdict and default classification (lines ~887-908, ~965-981).
  - Delete `compact_spec` grounding stripping.
  - Send the prompt on stdin to backends that support it (claude, codex, opencode). Keep argv only for backends that require it (agy `-p`), behind a 100 KiB cap that raises an error.
  - Drop all `GITHUB_TOKEN`/`GH_TOKEN`/token-file injection.
  - Make OpenRouter opt-in with `--allow-remote` and `SUMMA42_ALLOW_REMOTE_MODELS=1`.
  - Remove the duplicate `agy` key in `HARNESS_TYPES` and the hard-coded fictional model IDs.
  - Make the total fallback-chain time respect `--timeout` (stop starting backends when the remaining time is less than one backend's minimum).
  - Move the capability cache to `$XDG_CACHE_HOME/summa42/` (mode 0600), not `~/.summa42/state` or `/tmp`.
- [ ] **Step 4:** Run the tests. Expected: all pass. `python3 -m py_compile scripts/call-worker.py` is clean.
- [ ] **Step 5:** Commit: `fix(call-worker): fail closed, stdin prompts, no credentials, opt-in remote models`.

### Task 3: Fix the skills prompts (S-I6)

**Files:** Modify `skills/plan_review.md`, `skills/planning.md`, `skills/implementation.md`, `skills/autonomous_engineering.md`, and `scripts/call-worker.py` (the rules-block application).

- [ ] **Step 1:** Delete the "No Human Checkpoint" items (plan_review.md:23-24, planning.md:24-25) and section 4 of autonomous_engineering.md (lines ~40-41). Replace them with one line: "Owner approval steps required by policy are correct and must not be removed."
- [ ] **Step 2:** In implementation.md:32, replace the "verification evidence / passes go test" request with: "Do not claim tests were run; validation is executed by the Box."
- [ ] **Step 3:** In call-worker, append `rules_block` only for review schemas (plan review, code review, triage review). Include autonomous_engineering.md once.
- [ ] **Step 4:** Add `test_rules_block_only_on_review_schemas` to `scripts/test_call_worker.py` and run it. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(skills): stop training out owner approval and fabricated verification`.

### Task 4: Make the ACP bridge and summa42-query read-only or owner-gated (S-C1 query part, S-C7, S-I7, S-M4, S-M5, S-M10)

**Files:**
- Modify: `scripts/summa42-acp.js`, `scripts/summa42-query.py`
- Create: `scripts/test_summa42_query.py`

**Interfaces:**
- summa42-query.py keeps only read subcommands (status, cases, tickets). It opens `file:…?mode=ro` with `timeout=5`, resolves paths from `SUMMA42_HOME`, and derives the ticket state from the snapshot's real field. If the snapshot has no state field, it reports `UNKNOWN`, not OPEN.
- ACP commands:
  - `status`, `cases` and `tickets` stay open to everyone.
  - `chat` and `/mission show` require `verifyOwnerAuthorization`.
  - `/issue`, `reconcile`, `/mission <set>` and directive injection are deleted.

- [ ] **Step 1: Write the failing tests** in `scripts/test_summa42_query.py`:
  - `test_db_opened_read_only`: running against a temp DB copy, any write attempt through the module raises `sqlite3.OperationalError` (readonly).
  - `test_set_mission_and_inject_directive_removed`: the CLI exits 2 (argparse) for both subcommands.
  - `test_ticket_state_unknown_without_field`.
  - `test_honours_summa42_home`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3: Rewrite.**
  - Delete `set_mission` and `inject_directive` from query.py.
  - In acp.js, delete `createGitHubIssue`, the `reconcile` handler and the mission set/directive handlers. Owner-gate `chatWithCollective`.
  - Replace the hard-coded authority levels in `getSummaStatus` with the output of `summa42 status`.
  - Pass the OpenRouter key through `env`, not curl argv.
  - Fix the header comment: there is no cryptographic auth; it is an owner-ID allowlist in `discord_auth.json` (mode 0600, checked at load).
  - Remove the `lower.includes('ticket')` hijack (use an exact command match) and the `/home/topol` and `SofiaFlux/summa42` hard-codes (read them from env with no default; fail if unset).
- [ ] **Step 4:** Run `python3 -m unittest scripts/test_summa42_query.py -v` and `node --check scripts/summa42-acp.js`. Expected: PASS, clean.
- [ ] **Step 5:** Commit: `fix(acp,query): read-only canonical access, owner-gated chat, drop unauthenticated writes`.

### Task 5: Fix GitHub App token plumbing and registration (S-I8, S-I9 app part, S-M11)

**Files:** Modify `scripts/gh-app-token.py`, `scripts/register-gh-app.py`.

- [ ] **Step 1:**
  - In gh-app-token.py, write the token with `os.open(path, O_WRONLY|O_CREAT|O_TRUNC, 0o600)` to `~/.summa42/github_token` only.
  - Fail if the expected installation account is missing; do not pick `installations[0]`.
  - Add `--refresh-if-older-than 50m` so a timer can call it.
- [ ] **Step 2:** In register-gh-app.py:
  - drop `workflows: write`;
  - bind the callback to `127.0.0.1`;
  - add and verify a random `state` parameter;
  - create files with mode 0600 at open time.
- [ ] **Step 3:** Run `grep -rn "/tmp/summa42_github_token" --exclude-dir=.git .`. Expected: no hits. The remaining consumers (reconciler, call-worker) are deleted in Task 17 or fixed in Task 2.
- [ ] **Step 4:** Run `python3 -m py_compile scripts/gh-app-token.py scripts/register-gh-app.py`. Expected: clean.
- [ ] **Step 5:** Commit: `fix(gh-app): private token file with refresh, least-privilege app, loopback callback`.

---

## Phase B: Go pipeline correctness

### Task 6: Scrub model child-process environments (C-C3)

**Files:**
- Modify: `internal/ghtriage/climodel/adapter.go:~122`, `cmd/summa42-box/main.go:~338` (Copilot allowlist), `cmd/summa42-box/main.go` (`registerTriageModelFlags`)
- Test: `internal/ghtriage/climodel/adapter_test.go`, `cmd/summa42-box/copilot_env_test.go`

**Interfaces:**
- Produces `climodel.Config.EnvAllowlist []string`. The child gets `PATH`, `HOME`, `TMPDIR`, `LANG` plus exactly the allowlisted names present in the Box env.
- Produces the flag `--model-env NAME` (repeatable) on every subcommand that composes climodel. It rejects `GITHUB_TOKEN`, `GH_TOKEN` and any name containing `SUMMA42_` (error `model environment may not carry GitHub or Box credentials`).

- [ ] **Step 1: Write the failing tests.**
  - `TestAdapterChildEnvIsMinimal`: set `GITHUB_TOKEN`, `GH_TOKEN`, `SUMMA42_CONTROL_TOKEN` and `FOO`. The fake model binary prints `env`. Assert none of the four names appear, and that `PATH` does.
  - `TestAdapterChildEnvHonoursAllowlist`: with `EnvAllowlist{"FOO"}`, `FOO` is present.
  - `TestModelEnvFlagRejectsCredentials`: `--model-env GH_TOKEN` is an error.
  - `TestCopilotAllowlistExcludesGitHubTokens`: the allowlist contains `COPILOT_GITHUB_TOKEN` and does not contain `GH_TOKEN` or `GITHUB_TOKEN`.
- [ ] **Step 2:** Run `go test ./internal/ghtriage/climodel ./cmd/summa42-box -run 'Env|Copilot' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Set `cmd.Env` explicitly in the adapter. Wire the flag. Edit the allowlist.
- [ ] **Step 4:** Run the same command. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(climodel): model processes get a minimal explicit environment`.

### Task 7: Intake grants Actions explicitly and gains `--once` (C-C4, T-I2, S-I2 intake part)

**Files:**
- Modify: `cmd/summa42-box/main.go` (`parseGHIntakeFlags` ~1030-1110, `runGHIntake`), `internal/ghissue` (only if `RunReporting` needs a single-pass variant)
- Test: `cmd/summa42-box/gh_intake_test.go`

**Interfaces:**
- Produces the flag `--grant-action NAME` (repeatable). Each value must also appear in `--grant-capability`; otherwise the error is `grant-action %q must also be a granted capability`. It sets `workflow.Grant.Actions`.
- Produces the flag `--once`, which runs one observe pass and returns.
- No inference: Actions never default from Capabilities.

- [ ] **Step 1: Write the failing tests.**
  - `TestIntakeGrantActionSubsetOfCapabilities` covers the error case and the happy case. In the happy case, `cfg.Grant.Actions == []string{"workspace.repo.write","workspace.test"}`.
  - `TestIntakeOnceRunsSinglePass`: with an httptest GitHub server, assert one request round, then return nil.
  - `TestIntakeWithoutGrantActionHasNoActions`.
- [ ] **Step 2:** Run `go test ./cmd/summa42-box -run Intake -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement both flags.
- [ ] **Step 4:** Run the tests. Expected: PASS. Then run `go test ./internal/ghtriage -run PlanReview -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(intake): explicit --grant-action and --once`.

### Task 8: Planner tick budget, attempt cap, per-case errors and context dedupe (T-I1, S-I3)

**Files:**
- Modify: `internal/ghtriage/planner.go:56-73`, `internal/ghtriage/grounding.go:107-128`, `internal/ghtriage/planning.go:44-122`
- Test: `internal/ghtriage/planner_test.go`

**Interfaces:**
- `PlannerConfig.TickBudget time.Duration` (default 5m). No new case starts once less than one model timeout remains.
- `PlannerConfig.MaxPlanFailures int` (default 3). Failures are counted durably per work ID from the `attempt_failures` rows of planning tasks (or a dedicated evidence kind; the implementer picks one and states it in the commit message). After the cap, the case goes BLOCKED through `workflowcase.Assess`, with reason `planning-failed:<class>`.
- `PlanningGate.List` returns `([]Case, []CaseError, error)`. A per-case integrity problem becomes a `CaseError` and does not abort the mission. It also requires `c.NextWork.Kind == TaskClass`.
- `captureGrounding` reuses an existing `github.issue.source.context` evidence row whose content hash is identical (`evidence.FindByContentHash`) instead of calling `Put` again.

- [ ] **Step 1: Write the failing tests.**
  - `TestPlannerStopsAtTickBudget`: 5 cases, with a fake model that sleeps for the model timeout. Assert at most `budget/timeout` model calls.
  - `TestPlannerHoldsCaseAfterMaxFailures`: the model always errors. After 3 ticks the case state is BLOCKED and the reason has the prefix `planning-failed:`. Tick 4 makes no model call for it.
  - `TestPlanningGateIsolatesBadCase`: one case has a corrupted citation and another is valid. The valid one is planned, and the corrupted one appears in `[]CaseError`.
  - `TestPlanningGateRequiresTriageKind`: a case with `NextWork{}` is not listed.
  - `TestGroundingReusesIdenticalContextEvidence`: two ticks over the same source produce exactly one `github.issue.source.context` row.
- [ ] **Step 2:** Run `go test ./internal/ghtriage -run 'Planner|PlanningGate|Grounding' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests. Expected: PASS. Then run `go test ./internal/ghtriage -count=1 -timeout 20m`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(planner): bounded ticks, durable failure cap, isolated case errors, deduped context`.

### Task 8b: Per-issue file selection replaces the fixed allowed-path list (S-I1)

**Files:**
- Modify: `internal/ghtriage/grounding.go`, `internal/ghtriage/plan.go`, `cmd/summa42-box/ghplan_source.go`
- Test: `internal/ghtriage/grounding_test.go`, `cmd/summa42-box/ghplan_source_test.go`

**Interfaces:**
- New grounding flag: `--allowed-prefix DIR/` (repeatable). It is mutually exclusive with `--allowed-path`. `GroundingConfig.AllowedPrefixes []string`. Each prefix must satisfy `repoworkspace.SafePath` and end in `/`.
- With prefixes set, planning makes two model calls:
  1. A **selection call** with schema `github.issue.plan.files.v1`. Input: the issue snapshot plus the tracked file list under the prefixes (paths only). Output: `{"context_files":[…≤32], "allowed_paths":[…≤64]}`.
  2. Deterministic code checks that every path is under a prefix, that context files are tracked regular files, and that allowed paths are either tracked or explicitly new (not existing).
  3. It captures context for exactly those files, then runs the existing plan call.
- `PlanSource.AllowedPaths` stays exact-file. Downstream (plan review, implementation) is unchanged.
- `--context-file` stays required in exact mode. In prefix mode it is optional and pins extra files.

- [ ] **Step 1: Write the failing tests.**
  - `TestFileSelectionRejectsPathOutsidePrefix`.
  - `TestFileSelectionCapsCounts`: 33 context files or 65 allowed paths are rejected.
  - `TestPrefixModeRecordsSelectedPathsInPlanSource`: a fake selector returns `internal/purpose/research.go`. Assert `plan.Source.AllowedPaths` equals that list.
  - `TestAllowedPrefixAndPathMutuallyExclusive` (in the cmd package).
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. Add the selection schema constant to `call-worker.py`'s exact-schema table (from Task 2).
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(planning): per-issue file selection within owner-chosen prefixes`.

### Task 9: Classify failure signatures by error class and hold blocked work (T-I3)

**Files:**
- Modify: `internal/ghtriage/planreview_attempt.go:81`, `internal/ghtriage/implementation.go:169`, `internal/ghtriage/codereview.go:204`
- Create: `internal/ghtriage/failureclass.go`
- Test: `internal/ghtriage/failureclass_test.go`

**Interfaces:**
- Produces `func failureSignature(stage string, workID domain.ID, err error) string`. The result is `stage+":"+class+":"+workID`, where class ∈ {`timeout`, `model-exit`, `parse`, `validation`, `internal`}. `context.DeadlineExceeded`/`climodel` timeout map to `timeout`.
- `timeout` and `model-exit` carry a counter suffix (`#n`, n = prior failures of that class + 1), so they never trip the "same signature twice" block. Instead they are capped at 3 per work. On reaching the cap, the stage holds the case via `Assess` with BLOCKED and reason `<stage>-failed:<class>`.
- `parse`/`validation`/`internal` keep the stable signature, so the second identical one BLOCKs as today, and then the case is held via `Assess`.

- [ ] **Step 1: Write the failing tests.**
  - `TestTransientModelFailuresDoNotBlockPermanently`: two timeouts, then success. Assert the review Task SUCCEEDED.
  - `TestRepeatedTimeoutsHoldCase`: three timeouts. Assert case BLOCKED with reason `plan-review-failed:timeout`.
  - `TestRepeatedParseFailureBlocksAndHolds`.
  - `TestFailureSignatureClasses` (table-driven).
- [ ] **Step 2:** Run `go test ./internal/ghtriage -run 'Failure|Transient|Repeated' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement and use it at the three call sites.
- [ ] **Step 4:** Run the tests. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(ghtriage): error-classified failure signatures, held cases instead of silent BLOCKED tasks`.

### Task 10: Guarded plan-review completion and staged verdict (T-I11, T-M1, T-M14)

**Files:**
- Modify: `internal/ghtriage/planreview_attempt.go:30-96`, `internal/ghtriage/planreview.go:131-135`
- Test: `internal/ghtriage/planreview_test.go`

**Interfaces:**
- Consumes `CompleteAttemptWithGuard`/`AcceptTaskWithGuard` with `workflowcase.GuardCurrentWork`. Implementation already uses this pattern; copy it.
- The verdict is staged as evidence linked by subject before completion. A resumed run with the same Task/Attempt reuses it without calling the model again.
- "newer issue revision exists" holds the case through `Assess` (BLOCKED, `superseded-by:<rev>`) instead of failing every tick.
- Check `found` from `findAcceptedIndex`.

- [ ] **Step 1: Write the failing tests.**
  - `TestPlanReviewCompletionFailureResumesWithoutSecondModelCall`: inject a trigger that fails the first `CompleteAttempt`. Assert the model call count is 1 after the resume.
  - `TestPlanReviewSupersededDuringCallIsNotAccepted`.
  - `TestPlanReviewNewerRevisionHoldsCase`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/ghtriage -run PlanReview -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(planreview): guarded completion, staged verdict, held supersession`.

### Task 11: Triage driver is task-class aware (T-I4, T-M3, T-M4, T-M5, T-M6)

**Files:**
- Modify: `internal/ghtriage/driver.go:191-276, 405-426`, `internal/ghtriage/document.go:139-177`, `internal/ghtriage/executor.go:144-165`, `internal/ghtriage/reviewer.go:336-363, 527-556`
- Test: `internal/ghtriage/driver_test.go`, `internal/ghtriage/reviewer_test.go`

**Interfaces:**
- `advanceAccepted` branches on `task.TaskClass`:
  - only `github.issue.triage` runs triage logic;
  - every other class only checks supersession (challenge the active task, then block).
- `readDecision` recomputes Stage3 from Stage2 and rejects a mismatch.
- `loadSnapshot` requires `snap.UpdatedAt == payload.Revision`.
- `stateMatches` accepts the later reason chain for ready-to-plan cases.
- Read paths stop writing `LinkSubject`. Add a one-time migration step that links legacy rows (Go code in `internal/ghtriage`, run from intake startup), then delete the `math.MaxInt` fallback.

- [ ] **Step 1: Write the failing tests.**
  - `TestDriverIgnoresSucceededPlanReviewTask`: no error, and supersession still applies to a newer revision.
  - `TestDriverDoesNotRecordImplementationFailureAsTriageFailed`.
  - `TestReadDecisionRejectsStage3Mismatch`.
  - `TestLoadSnapshotRejectsRevisionMismatch`.
  - `TestReviewerAcceptsPostPlanReviewChain`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/ghtriage -count=1 -timeout 20m`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(driver): branch on task class; recompute stage3; strict snapshot revision`.

### Task 12: Cross-mission exclusivity for an external object (T-I5)

**Files:**
- Modify: `internal/workflowcase/service.go:232-244`, `internal/workflowcase/latest.go:13-39`, `internal/scheduler/service.go` (eligibility)
- Test: `internal/workflowcase/service_test.go`, `internal/scheduler/service_test.go`

**Interfaces:**
- `Ensure` refuses with `ErrObjectOwnedByOtherMission` when a non-CLOSED case for the same (source, object) exists under a different mission.
- The scheduler does not lease tasks whose `purpose_kind='MISSION'` names an inactive mission. Those tasks stay ELIGIBLE and are cleaned up in Phase F.

- [ ] **Step 1: Write the failing tests.**
  - `TestEnsureRefusesObjectOwnedByOtherMission`.
  - `TestEnsureAllowsObjectAfterOtherMissionCaseClosed`.
  - `TestSchedulerSkipsTasksOfInactiveMission`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. The check runs inside the `Ensure` transaction.
- [ ] **Step 4:** Run `go test ./internal/workflowcase ./internal/scheduler -count=1` and then the concurrency gate. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(workflowcase): one live case per external object across missions`.

### Task 13: Terminal transitions after implementation (T-I7)

**Files:**
- Modify: `internal/ghtriage/implementation.go`, `internal/ghtriage/codereview.go`, `internal/ghtriage/publication.go`
- Test: the corresponding `_test.go` files

**Interfaces:**
- On a validation failure in implementation: `Assess` BLOCKED, `implementation-failed:validation`.
- On code review REVISE/BLOCK: `Assess` BLOCKED, `code-review:<verdict>: <reason>`.
- On confirmed publication: `Assess` to READY_FOR_VERIFICATION, reason `published:<pr-url>`, NextWork `{}`. Closing stays with an independent verifier (owner, or a later stage). That is the end state of the GitHub pipeline after the merge stack is deleted in Task 16.

- [ ] **Step 1: Write the failing tests.**
  - `TestValidationFailureHoldsCase`.
  - `TestCodeReviewReviseHoldsCase`.
  - `TestConfirmedPublicationMovesCaseToReadyForVerification`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/ghtriage -count=1 -timeout 20m` and the concurrency gate. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(ghtriage): cases leave ACTIVE after implementation, review and publication`.

### Task 14: Validation isolation and stage-independent source checks (T-I6, T-I9, T-M7, T-M8)

**Files:**
- Modify: `internal/repoworkspace/validation.go:18-25, 59-80, 315`, `internal/ghtriage/codereview.go:270-295`, `internal/ghtriage/publication.go:~196`, `internal/repoworkspace/source.go:200`
- Test: `internal/repoworkspace/validation_test.go`, `internal/ghtriage/codereview_test.go`

**Interfaces:**
- Validation env:
  - a private `GOCACHE` and `GOMODCACHE` under the attempt dir;
  - an owner-supplied read-only module cache via the existing explicit argv wrapper only;
  - no inherited `GOPATH`, `GOCACHE` or `GOMODCACHE`.
- `SUMMA42_VALIDATION_TIMEOUT` is deleted. The 60 s per command from the spec is fixed.
- `Passed` is true only when the exit code is 0 and the command did not time out.
- Later stages (code review, publication) no longer require `SourceState(sourceRepo) == record.SourceStateHash`. They require that the pinned base commit still exists in the source repo and that the candidate commit is present in the attempt workspace with the recorded tree. The SourceState equality check stays only within one implementation attempt (the README's guarantee).
- `repoworkspace.GC(root string, keep func(attemptDir string) bool) error` removes attempt directories of terminal attempts. Implementation calls it at the start of each run.

- [ ] **Step 1: Write the failing tests.**
  - `TestValidationDoesNotInheritHostGoCaches`: set `GOCACHE=/host/x`. The command prints `go env GOCACHE`. Assert it is under the attempt dir.
  - `TestValidationTimeoutNotOverridable`.
  - `TestTimedOutZeroExitIsNotPassed`.
  - `TestOwnerCommitBetweenStagesDoesNotKillCase`: implement, then commit to the source repo, then run code review. Assert the review proceeds.
  - `TestGCRemovesTerminalAttemptDirs`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/repoworkspace ./internal/ghtriage -count=1 -timeout 20m`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(repoworkspace): private validation caches, fixed timeout, stage-independent source checks, GC`.

### Task 15: The stage-2 retry respects the remaining deadline (T-I10)

**Files:**
- Modify: `internal/ghtriage/executor.go:22-24, 111-135`, `cmd/summa42-box/main.go` (worker exec deadline)
- Test: `internal/ghtriage/executor_test.go`

**Interfaces:**
- Retry only if `deadline - now > modelTimeout`.
- The worker's per-attempt context deadline is `min(lease, caller deadline) - 10s`.

- [ ] **Step 1: Write the failing test** `TestStage2RetrySkippedWhenDeadlineTooClose`.
- [ ] **Step 2:** Run it. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run it. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(triage): do not retry past the lease`.

### Task 16: Delete the merge stack; publication pre-write failures settle NoEffect (C-C1, C-C2, C-I2, C-I3, C-I4, C-I5, C-I6, C-I10, T-C1, T-I8, C-M2, C-M3, C-M11, T-M9)

**Decision (owner may flip at plan review):** delete `run-gh-merge` rather than patch it. The pipeline ends at a published draft PR in READY_FOR_VERIFICATION (Task 13), and the owner merges. The protected-merge design (2026-10-16 spec) gets re-implemented later from its spec, with the gates the review lists.

**Files:**
- Delete: `internal/ghtriage/merge.go`, `internal/ghtriage/merge_test.go`, `internal/ghpublish/merge.go`, `internal/ghpublish/merge_test.go`, `internal/ghpublish/observer.go`, `internal/ghpublish/observer_test.go`, `cmd/summa42-box/ghmerge.go`, `cmd/summa42-box/ghmerge_test.go`
- Modify: `cmd/summa42-box/main.go` (remove the `run-gh-merge`/`run-repo-merge` dispatch), `internal/ghpublish/providers.go:217-237, 317-322, 354-361`, `docs/superpowers/plans/2026-10-16-ci-observation-and-protected-merge.md` (status header: "Reverted 2026-10-21, see review-remediation plan")
- Test: `internal/ghpublish/providers_test.go`

**Interfaces:**
- `BranchProvider`/`PRProvider.Dispatch` return `(ProviderOutcome{State: OperationConfirmedNoEffect, Reason: …}, nil)` for conditions detected before any write (base moved, branch conflict, base tree mismatch, refs moved).
- `PRProvider.find` treats a non-draft PR carrying the marker as the Summa PR (a human may mark it ready), not as a conflict. A marker PR from a different head branch remains a conflict.

- [ ] **Step 1: Write the failing tests.**
  - `TestBranchProviderBaseMovedIsNoEffect`.
  - `TestPRProviderRefsMovedIsNoEffect`.
  - `TestPRProviderFindAcceptsReadyMarkedPR`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Delete the merge stack. Implement the provider changes. Fix the test fakes in `publication_test.go` to return real PR URLs.
- [ ] **Step 4:** Run `go build ./... && go test ./internal/ghpublish ./internal/ghtriage ./cmd/summa42-box -count=1 -timeout 20m`. Expected: PASS. Then run `grep -rn "extractPullNumber\|run-gh-merge" --include=*.go .`. Expected: no hits.
- [ ] **Step 5:** Commit: `fix(publish): delete fabricated merge stack; pre-write rejections settle as no effect`.

### Task 17: `run-gh-pipeline` replaces the Python reconciler (S-C1, S-C2, S-C3, S-C5, S-C8, S-I2, S-I5, C-M1, C-M7 pipeline part)

**Files:**
- Create: `cmd/summa42-box/ghpipeline.go`, `cmd/summa42-box/ghpipeline_test.go`
- Modify: `cmd/summa42-box/main.go` (dispatch `run-gh-pipeline`/`run-repo-pipeline`), `cmd/summa42-box/ghimplement.go`, `ghcode_review.go`, `ghpublish.go`, `ghplan_review.go` (busy exit code)
- Delete: `scripts/summa42-reconciler.py`, `bin/summa42-reconciler`, `bin/summa42-mission`. Deleting these also resolves S-M1, S-M2, S-M3 and S-M13; the merge-gate part of S-I9 goes away with Task 16.

**Interfaces:**
- `run-gh-pipeline --mission ID --source-repo ABS --workspace-root ABS --repository owner/name --base-branch B --credential-file ABS [grounding flags from Task 8b] [model flags] [--review-model-binary B --review-model-arg A… --review-model-timeout D] [--once | --interval D]`
- One pass runs these stages in order, each under its own `context.WithTimeout`:
  1. recovery: `box.Wake.RecoverExpiredLeases` (Task 18 later replaces this call with `box.Recover`)
  2. triage driver tick
  3. worker `--once` (triage)
  4. triage review
  5. plan
  6. plan review
  7. then, for each case whose `NextWork.Kind == ghtriage.ImplementationWorkKind`: implement, then code review, then publish
- Stages run in-process by calling the existing `runGH*(ctx, args)` with constructed argv.
- A stage error is logged as one line to stderr, `stage=<name> case=<id> err=<msg>`, and the pass continues. The pass exits 1 if any stage failed.
- Plan review and code review must use the `--review-model-*` flags. If the review model config equals the producer config (same binary and same args), startup fails with `review model must differ from producer model`.
- Busy: every `run-gh-*` exits with code 3 and prints `busy` when it found work held by another claimant. The pipeline counts busy as "skipped", not as failure.

- [ ] **Step 1: Write the failing tests** in `ghpipeline_test.go`, using the fake model and httptest GitHub patterns already in `cmd/summa42-box/*_test.go`:
  - `TestPipelineStageErrorDoesNotSkipLaterStages`: the plan stage errors, and plan review still runs (asserted by a fake review model call count).
  - `TestPipelineSelectsImplementCasesByGoConstant`.
  - `TestPipelineRequiresDistinctReviewModel`.
  - `TestPipelineExitCodeReflectsFailures`.
  - `TestRunGHImplementBusyExitsThree`.
- [ ] **Step 2:** Run `go test ./cmd/summa42-box -run 'Pipeline|Busy' -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement. Delete the reconciler and its two bin symlinks.
- [ ] **Step 4:** Run the tests. Expected: PASS. Then run `grep -rn "summa42-reconciler\|summa42-mission" --exclude-dir=.git . | grep -v docs/superpowers/plans/2026-10-21`. Expected: none.
- [ ] **Step 5:** Commit: `feat(box): run-gh-pipeline replaces the Python reconciler`.

---

## Phase C: core runtime

### Task 18: Wire recovery: ReconcilePending, WithoutCancel settlement, per-tick lease recovery, NO_EFFECT contract (K-I1, K-I2, K-I4, K-I7, C-M7)

**Files:**
- Modify: `internal/operations/service.go:477-499, 557-603`, `internal/operations/provider.go` (doc on `LookupOutcome`), `internal/scheduler/worker.go:170-233`, `cmd/summa42-box/main.go:858-862`, `cmd/summa42-box/ghpipeline.go` (stage 1 → `box.Recover`), `internal/runtime/box.go`
- Test: `internal/operations/service_test.go`, `internal/scheduler/worker_test.go`, `internal/runtime/box_test.go`

**Interfaces:**
- `runtime.Box.Recover(ctx) (RecoveryReport, error)` runs `Wake.RecoverExpiredLeases` and then `operations.NewReconciler(box.Operations).ReconcilePending`. It is called at every Box/worker start and on each worker tick. Errors are returned, not swallowed. `RecoveryReport{ExpiredLeases, ReconciledOperations int}`.
- After the commit boundary, `markUnknown`/`SettleOutcome` use `context.WithoutCancel(ctx)` with a 30 s timeout.
- `reconcile` refuses to settle CONFIRMED_NO_EFFECT for an operation dispatched less than `Provider.SettleWindow()` ago (new interface method; return 0 for providers that can prove absence). The slot stays OUTCOME_UNKNOWN until then.

- [ ] **Step 1: Write the failing tests.**
  - `TestBoxStartupReconcilesDispatchedOperation`.
  - `TestCancelledContextAfterDispatchStillSettles`: cancel ctx inside the fake `Dispatch`, then assert the operation is CONFIRMED_EFFECT.
  - `TestWorkerTickRecoversExpiredLease`.
  - `TestWorkerSurfacesRecoveryError`.
  - `TestReconcileWithinSettleWindowKeepsUnknown`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement. Add `SettleWindow()` to every Provider implementation (9 files per `grep -rln "AdapterVersionSemanticallyRelevant() bool"`).
- [ ] **Step 4:** Run `go test ./internal/operations ./internal/scheduler ./internal/runtime -count=1 -race`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(core): wire operation and lease recovery; settle past cancellation`.

### Task 19: Multi-process SQLite safety (K-I3)

**Files:**
- Modify: `internal/state/sqlite/store.go:23-53, 86-106`, `internal/scheduler/worker.go:228-230`, README "What is authoritative", and the in-code comments claiming single-connection correctness
- Test: `internal/state/sqlite/store_test.go`

**Interfaces:**
- The DSN adds `_txlock=immediate`, so every transaction is BEGIN IMMEDIATE.
- `Worker.Run` treats a busy/locked error from `StepOnce` as a retryable tick (log and continue), not as an exit.
- README states: the Box and the `run-*` subcommands may run concurrently as separate processes on one local DB; correctness comes from guarded transactions, not process exclusivity.

- [ ] **Step 1: Write the failing test** `TestConcurrentProcessesWriteWithoutBusyCrash`: two `*sql.DB` handles on one file, 200 interleaved guarded writes each. Assert no error. Also assert the DSN contains `_txlock=immediate`.
- [ ] **Step 2:** Run it. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/state/... ./internal/scheduler -count=1 -race`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(state): immediate transactions for multi-process safety`.

### Task 20: Provider-derived risk, policy v2 with owner approval, attribute namespacing (K-I6, C-I1, K-M3)

**Files:**
- Modify: `internal/operations/provider.go`, `internal/operations/service.go:104-108, 715-735, 888`, every Provider implementation, `internal/policy/default_profile.go`, `internal/bootstrap/service.go`, `cmd/summa42/` (new `policy_cmd.go`)
- Create: `internal/state/sqlite/migrations/00020_policy_set_activation.sql` (only if the existing triggers forbid flipping `policy_sets.active`; the implementer checks 00006 first)
- Test: `internal/operations/service_test.go`, `internal/policy/default_profile_test.go`, `cmd/summa42/policy_cmd_test.go`

**Interfaces:**
- `Provider.MinimumRisk() string` returns `LOW`, `MEDIUM` or `HIGH`. The effective risk is `max(provider, request.Risk)`. Values:
  - ghpublish Branch/PR: `MEDIUM`
  - ADO vote: `MEDIUM`
  - ADO comment: `LOW`
  - feedback issue: `LOW`
- Policy `policy_mvc_default_v2`:
  - LOW and `authority_valid`: ALLOW.
  - MEDIUM and `authority_valid`: REQUIRE_APPROVAL with `required_approvals: [input.owner_principal_id]`.
  - Everything else: DENY.
- The operations service always sets `owner_principal_id`, as a trusted key.
- Caller `Attributes` go under `input.attributes.caller.*`; they never overwrite trusted keys. They are persisted with the operation and replayed at DISPATCH.
- New installs bootstrap v2. `summa42 policy upgrade` is an Owner action:
  - it signs with the Owner key;
  - it inserts v2;
  - it deactivates v1 and activates v2 in one transaction;
  - it updates `config.json` `ActivePolicySetID`;
  - it refuses if any operation is PREPARED or DISPATCHED.

- [ ] **Step 1: Write the failing tests.**
  - `TestEffectiveRiskIsProviderMinimum`: the caller says LOW and the provider says MEDIUM. Assert the decision is REQUIRE_APPROVAL.
  - `TestCallerAttributesCannotOverrideTrustedKeys`.
  - `TestDispatchReplaysPrepareAttributes`.
  - `TestPolicyV2RequiresOwnerForMedium`.
  - `TestUpgradeFromV1CollectiveStarts`: bootstrap with v1, run upgrade, then `loadStartupMaterial` succeeds with v2.
  - `TestUpgradeRefusesWithInFlightOperations`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/operations ./internal/policy ./internal/bootstrap ./cmd/... -count=1` and `go test ./tests/acceptance -count=1 -timeout 20m`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(policy): provider-derived risk; v2 requires Owner approval for MEDIUM effects`.

### Task 21: The Owner sees what they approve (K-I5, C-I8, K-M10, K-M11)

**Files:**
- Modify: `internal/control/server.go:94-132, 461-566`, `internal/control/client.go:218-239`, `cmd/summa42/approve_cmd.go`, `cmd/summa42/approvals_cmd.go`, `cmd/summa42/inspect_cmd.go`, README "Policy and authority"
- Test: `internal/control/server_test.go`, `cmd/summa42/root_test.go`

**Interfaces:**
- `ApprovalDTO` and `OperationDTO` gain `CanonicalIntent json.RawMessage` and `Attributes map[string]any`.
- `summa42 approvals` prints provider, slot and the pretty canonical intent.
- `summa42 approve <id>` prints the intent and requires typing `yes` on a TTY. `--yes` is accepted only when stdin is not a TTY *and* `SUMMA42_APPROVE_NONINTERACTIVE=1`.
- README documents:
  - with `LOCAL_DEV_FILE` custody, approval is not a boundary against UNENFORCED executors running as the same OS user;
  - only the Owner can approve; other required approvers are unsupported (K-M10);
  - an approval expires with the lease (K-M11).

- [ ] **Step 1: Write the failing tests.**
  - `TestApprovalDTOIncludesCanonicalIntent`.
  - `TestApproveRequiresConfirmation`.
  - `TestApproveNonInteractiveNeedsExplicitEnv`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/control ./cmd/summa42 -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(approvals): show the bound intent and require explicit confirmation`.

---

## Phase D: capabilities, experience, ADO

### Task 22: Delete AUTHORITY_PROMOTION; preference filters by kind (P-C1, P-I2)

**Files:**
- Modify: `internal/experience/service.go:24, 250, 465-466, 543-573`, `internal/experience/evaluator.go:99-113`, `internal/experience/service_test.go`
- Create: `internal/state/sqlite/migrations/00021_adaptation_kind_check.sql`
- Test: `internal/experience/*_test.go`, `internal/state/sqlite/migrate_roundtrip_test.go`

**Interfaces:**
- `adaptation_kind` must be `EXECUTOR_PREFERENCE`. Enforce it with a trigger on `adaptation_grants` and `experience_rules` (INSERT/UPDATE `RAISE(ABORT, 'only EXECUTOR_PREFERENCE adaptation is permitted')`).
- The migration first sets any existing non-preference rule to `ROLLED_BACK`.
- `Preference` filters on that kind. Ambiguity returns "no preference" plus a field observation, not an error.

- [ ] **Step 1: Write the failing tests.**
  - `TestAuthorityPromotionKindRejected` (Go and DB trigger).
  - `TestAmbiguousPreferenceFallsBackToBaseline`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Delete the kind, `HasAuthorityPromotion` and its test. Implement the filter and the migration.
- [ ] **Step 4:** Run `go test ./internal/experience ./internal/scheduler ./internal/state/... -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(experience): experience can only reorder executors`.

### Task 23: Delete capability acquisition (P-I1, P-M1, P-M2, K-M17 approvals part)

**Files:**
- Delete: `internal/capabilities/discovery.go`, `internal/capabilities/discovery_test.go`
- Modify: `internal/capabilities/registry.go:107-122, 363-368` (remove `RegisterProvider`/`UnregisterProvider`/`Providers`), `internal/runtime/box.go` (remove `CapabilityDiscovery` and `Config.CapabilitySources`), `internal/approvals/service.go` (remove `IsApproved` and non-Tx `Consume` if no callers remain)
- Create: `internal/state/sqlite/migrations/00022_drop_capability_acquisitions.sql` (the up migration drops 00019's objects; the down migration recreates them)
- Test: `internal/state/sqlite/migrate_roundtrip_test.go`

- [ ] **Step 1: Write the failing test** `TestMigrationsApplyOverLegacyRows`. Build a DB at migration 00019, insert the legacy shapes seen in production, then migrate to head. Assert success. The legacy shapes are:
  - a `workflow_cases` row with `grant_json='{"priority": 1000}'`;
  - an `experience_outcomes` row with `recorded_at='2026-10-19 14:41:15'`;
  - a stray `issue_hierarchy` table.
- [ ] **Step 2:** Run it. Expected: FAIL (00022 is missing).
- [ ] **Step 3:** Delete the code and write the migration.
- [ ] **Step 4:** Run `go build ./... && go test ./internal/capabilities ./internal/runtime ./internal/approvals ./internal/state/... -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(capabilities): delete unbound capability acquisition`.

### Task 24: Experience metrics come from canonical records; detectors survive malformed rows (P-I5, P-I6)

**Files:**
- Modify: `internal/experience/service.go:301-400`, `internal/control/experience.go:125-128`, `internal/fieldfeedback/detectors.go:43-47, 339-365`
- Test: `internal/experience/service_test.go`, `internal/fieldfeedback/detectors_test.go`

**Interfaces:**
- `ExperienceOutcomeCreateRequest` loses `CostUnits`, `LatencyMs`, `RetryCount` and `HumanIntervention`. The service derives them:
  - cost: the resource ledger (settled) for the task;
  - latency: first attempt `started_at` to acceptance;
  - retries: attempt count minus 1;
  - human intervention: any Owner approval decision linked to the task's operations.
- A detector skips a row with an unparseable timestamp and emits one `MALFORMED_CANONICAL_ROW` observation per pass. It does not abort `Detect`.

- [ ] **Step 1: Write the failing tests.**
  - `TestOutcomeMetricsDerivedFromLedger`.
  - `TestControlRejectsCallerSuppliedMetrics`.
  - `TestDetectSkipsMalformedRecordedAt`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/experience ./internal/fieldfeedback ./internal/control -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(experience): canonical outcome metrics; tolerant detectors`.

### Task 25: ADO vote bound to identity and commit (P-I3, P-I4, P-M5, P-M6)

**Files:**
- Modify: `internal/adoeffects/effects.go:59-66, 242-248, 264-297, 322-335, 427-443`, `internal/adoreview/publish.go:233-241`
- Test: `internal/adoeffects/effects_test.go`, `internal/adoreview/publish_test.go`

**Interfaces:**
- `VoteIntent` gains `SourceCommit`, `TargetCommit` and `ReviewerID`; they are part of the canonical intent and so of the approval digest.
- `Dispatch` re-reads the PR and returns CONFIRMED_NO_EFFECT if `lastMergeSourceCommit != SourceCommit`.
- `LookupOutcome` confirms only if the reviewer with `id == ReviewerID` has vote 10. If the identity is unknown, it returns OUTCOME_UNKNOWN.
- `CommentIntent` sends `threadContext` (`filePath`, line) when `Path` is set.
- Delete the unreachable vote enum cases and close the session on the enum error path.

- [ ] **Step 1: Write the failing tests.**
  - `TestVoteLookupIgnoresOtherReviewersApproval`.
  - `TestVoteDispatchRefusesMovedSourceCommit`.
  - `TestCommentSendsThreadContext`.
- [ ] **Step 2:** Run them. Expected: FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run `go test ./internal/adoeffects ./internal/adoreview -count=1 -timeout 15m`. Expected: PASS.
- [ ] **Step 5:** Commit: `fix(ado): vote bound to reviewer identity and reviewed commit`.

---

## Phase E: minor findings

### Task 26: Delete dead and decorative code (K-M2, K-M6, K-M7, K-M17, C-M4, P-M3, P-M7, P-M11, P-M13)

**Files:**
- `internal/memory` (delete the service and its Box wiring if `grep` confirms there are no production callers)
- `internal/execution/service.go:396-442` (unguarded `ChallengeTask`)
- `internal/observability` (`Bridge`)
- `internal/resources` (non-Tx wrappers)
- `internal/operations/approval_gate.go:30` (tautological stale check)
- `internal/feedbackgithub/provider.go` (`Provider`/`New`), `internal/feedbackgithub/client.go:267-279`
- `internal/executors/codex.go:46-51`, `internal/executors/copilot.go:77, 85-87, 118-125`
- `internal/fieldfeedback/sanitizer.go:27-29, 35, 66, 136-141`
- `internal/capabilities/session.go:295-301`
- `internal/experience/service.go:366-367`

- [ ] **Step 1:** For each symbol, run `check_references`/`grep` and confirm it has no non-test production caller. Leave any that have one and list them in the commit body.
- [ ] **Step 2:** Delete them, along with their tests that exist only to cover the dead code.
- [ ] **Step 3:** Run `go build ./... && go vet ./... && go test ./... -count=1 -timeout 30m`. Expected: PASS.
- [ ] **Step 4:** Commit: `chore: delete dead and decorative code`.

### Task 27: Core minor fixes (K-M1, K-M4, K-M5, K-M9, K-M12, K-M13, K-M14, K-M15)

Each bullet gets one named test in the owning package. Implement them one at a time, with tests before code.
- **K-M1:** add `domain.FormatTime(t) string` with the fixed layout `2006-01-02T15:04:05.000000000Z07:00`, used for all canonical timestamp writes. `TestFormatTimeSortsLexically`.
- **K-M4:** `ReleaseInTx` accepts only HELD; `SettleInTx` records `over_settled` when actual > reserved. `TestReleaseRejectsUnresolved`, `TestSettleFlagsOverSettlement`.
- **K-M5:** `evidence.Get` hashes the returned bytes and checks `size_bytes`. A tmp sweep runs at `Open`. A corrupt blob is quarantined to `blobs/quarantine/` and rewritten. `TestGetVerifiesReturnedBytes`, `TestCorruptBlobIsReplaced`.
- **K-M9:** refuse the `/tmp` control socket fallback; set an owner-only SDDL on Windows. `TestNoTmpSocketFallback`.
- **K-M12:** verify the constitution signature in `loadStartupMaterial`. `TestStartupRejectsTamperedConstitution`.
- **K-M13:** `CompleteAttempt` refuses while the attempt has DISPATCHED or OUTCOME_UNKNOWN operations. Verifier ≠ executor is enforced in `AcceptTask`. `TestCompleteRefusesUnsettledOperations`, `TestVerifierMustDifferFromExecutor`.
- **K-M14:** the scheduler skips past-deadline tasks and sets `EXPIRED`. `TestSchedulerExpiresPastDeadline`.
- **K-M15:** `NewLocalEd25519` never creates a missing key file (bootstrap creates keys); loads require mode 0600. `TestLoadRejectsMissingKey`, `TestLoadRejectsWideMode`.
- [ ] **Step 1:** Write the 13 tests listed above. Run them. Expected: FAIL.
- [ ] **Step 2:** Implement them in the order listed.
- [ ] **Step 3:** Run `go test ./internal/... -count=1 -timeout 30m`. Expected: PASS.
- [ ] **Step 4:** Commit: `fix(core): minor review findings K-M1..K-M15`.

### Task 28: Workflow, ADO and executor minor fixes (K-M8, P-M4, P-M8, P-M9, P-M10, P-M12, P-M15, T-M2, T-M10, T-M11, T-M12, T-M13)

- **K-M8:**
  - `DockerArgs` rejects a workspace of `/`, or one containing a `docker.sock`.
  - The proxy binds only the given gateway address and sets `ReadHeaderTimeout` and `IdleTimeout`.
  - Package doc and README label the TEB as "primitives, not wired".
  - Tests: `TestDockerArgsRejectsRootWorkspace`, `TestProxyBindsGatewayOnly`.
- **P-M4:** Codex is invoked with an explicit `--sandbox workspace-write`. `TestCodexPassesExplicitSandbox`.
- **P-M8 and P-M10:** doc-only. Label the cost/enforcement constants "provider-declared", and label the MCP adapter and Session as primitives with no production caller.
- **P-M9:** a per-capability parameter allowlist in adomcp. `TestADOProviderRejectsUnlistedParameter`.
- **P-M12:** propagate the dropped `time.Parse`, `json.Marshal` and audit-append errors in experience. `TestActivationFailsWhenAuditAppendFails`.
- **P-M15:** copy before appending in `adoreview/assess.go:197-212`. `TestAssessDoesNotAliasCallerSlices`.
- **T-M2:** propagate the `Canonical()` errors. `TestDriftCheckFailsOnCanonicalError`.
- **T-M10:** `Assess` verifies that cited evidence exists. `TestAssessRejectsUnknownEvidence`.
- **T-M11:** `Ensure` compares only identity fields for an existing revision, and returns `ErrIntakeConfigChanged` with a clear message for a grant change. `TestEnsureReportsConfigChange`.
- **T-M12:** a typed `PlanReviewVerdict` field replaces the reason-prefix check. `TestImplementationInputUsesTypedVerdict`.
- **T-M13:** the intake revision ignores `updated_at` bumps caused only by comments from the GitHub App bot login. Use the issue's `updated_at`, falling back to the latest non-bot event time. `TestBotCommentDoesNotCreateRevision`.
- [ ] **Step 1:** Write the tests. Run them. Expected: FAIL.
- [ ] **Step 2:** Implement.
- [ ] **Step 3:** Run `go test ./... -count=1 -timeout 30m`. Expected: PASS.
- [ ] **Step 4:** Commit: `fix: minor workflow, ADO and executor review findings`.

### Task 29: Formatting and CI gates (C-M5, C-M6, C-M8, C-M9, C-M10, K-M16, P-M14, S-M12)

- [ ] **Step 1:** `gofmt -w $(gofmt -l .)`. Fix the misplaced `runGHPlan` doc comment (C-M6).
- [ ] **Step 2:**
  - Register `--once`/`--workspace-root` on `run-worker` as normal flags (C-M8), with test `TestWorkerOnceInsideModelArgIsPreserved`.
  - Bound `findByMarker` pagination to 10 pages and require the author to be the configured bot login (C-M9), with test `TestFindByMarkerIgnoresForeignAuthors`.
  - Consolidate GitHub credentials on `--credential-file` for every GitHub subcommand. Keep `SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE` only as a deprecated alias that logs once (C-M10).
- [ ] **Step 3:** In verify_docs.py, keep `_` in anchors and resolve `/docs/x` against the repo root (S-M12). Add `scripts/test_verify_docs.py` with `test_underscore_anchor` and `test_root_relative_link`.
- [ ] **Step 4:** CI:
  - add a `gofmt -l .` check that fails on output;
  - add `python3 -m unittest discover -s scripts -p 'test_*.py'`;
  - give the race step `-timeout 45m`.
- [ ] **Step 5:** Run the full verification matrix from Global Constraints. Expected: all clean.
- [ ] **Step 6:** Commit: `chore: gofmt, flag hygiene, CI gates`.

### Task 30: Documentation matches the code (C-I9, P-M9 README:53, K-I3/K-I5 docs)

**Files:** `README.md`, `docs/VISION.md` (only if it claims something removed), `CONTRIBUTING.md` (new gates).

- [ ] **Step 1:** Rewrite "Operational notes" to state only what the code does:
  - validation uses private caches;
  - the env var is `GIT_NO_LAZY_FETCH=1`, and README:79 is corrected;
  - no merge stage exists; the pipeline ends at a published draft PR awaiting Owner merge.
- [ ] **Step 2:** Document `run-gh-pipeline`, `--grant-action`, `--allowed-prefix`, `summa42 policy upgrade` and the `run-repo-*` aliases. Remove the AUTHORITY_PROMOTION, capability-discovery and merge claims. Fix README:53 (8 advertised ADO capabilities). Move the misplaced sections after "License" back to their topical place.
- [ ] **Step 3:** Run `python3 scripts/verify_docs.py`. Expected: clean.
- [ ] **Step 4:** Commit: `docs: README reflects the remediated system`.

---

## Phase F: cleanup (owner go-ahead required; not executed by the plan run)

Each item is an outward-facing or live-data action. Present them to the owner as a checklist after Phase E.

1. **Canonical DB incident repair** (`~/.summa42/state/summa42.db`; back it up first):
   - drop `issue_hierarchy` (migration 00022 handles fresh DBs);
   - set `deactivated_at` on the two inactive missions;
   - for the 69 rewritten grants and 18 `{}`-next-work ACTIVE cases (T-C2): BLOCK via `Assess` with reason `incident-2026-10-21`, or re-intake them;
   - cancel `task_dir_89be8e803b5f` and the 23 ELIGIBLE triage tasks of inactive missions;
   - review the 12 EXPIRED attempts with no events;
   - run `summa42 policy upgrade`.
2. **GitHub:** audit and close the duplicated decomposition issues (#31–#112) after owner review of the list.
3. **History note:** record that PR #21's plan and code reviews were produced by a canned adapter (S-C6).
4. **Deploy:** rebuild `bin/summa42` and `bin/summa42-box`; replace the reconciler loop with `run-gh-intake` plus `run-gh-pipeline --interval`, logging to `~/.summa42/logs/`.
