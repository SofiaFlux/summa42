# Reviewer isolation: producer and reviewer as different OS users

**Status:** proposed, awaiting the Owner's approval. **Card:** M9-T6 of the
[development plan](../plans/2026-11-11-development-plan.md), sections 2.5 and
rule R12. **Base:** `main` @ `2db7d4a`.

No build card of M9-T6 starts before the Owner approves this document.

## 1. Problem

The independent review is the control that stands between a model-written
change and publication, and after milestone M5 a standing delegation will lean
on it without the Owner looking at each change. It is independent only if the
producer cannot reach the reviewer except through the artefact under review.

Today that does not hold on the host:

- `run-gh-pipeline` registers `--review-model-user`
  (`registerModelFlags(flags, "review-model", …)` in
  `cmd/summa42-box/ghpipeline.go`), but its default is `SUMMA42_MODEL_USER`
  and nothing requires it to differ from `--model-user`. The only independence
  check is `sameModel`, which compares models.
- The single-stage commands `run-gh-plan-review`, `run-gh-code-review` and
  `run-gh-triage-review` take their model under `--model-user`, the same flag
  and default as the producer commands.
- Both roles therefore run as `summa42-model`, with one home directory. A
  producer call can leave a file, a changed CLI setting, a hook or a running
  process that a later reviewer call reads or meets.

The validation user already shows the pattern: candidate code runs as
`summa42-validate`, which cannot read the model home
(`Spec.Unreadable` in `internal/repoworkspace/validation.go`).

## 2. Goal and limits

**Goal.** A reviewer call cannot read or be influenced by anything a producer
call left on the host, and the reverse, except the evidence the Box projects
into its prompt.

**What this spec closes:** home directories, CLI configuration, leftover
processes, files in world-writable directories.

**What it does not close, and says so:** two processes on one kernel can still
signal each other through timing, load, or what they send to a shared provider
account. Those channels carry no authority and reach no secret. Closing them
needs separate mount, process and network namespaces per call, which the
containers of milestone M1A provide; this spec is the step that is possible on
a plain host today. The enforcement level of model calls stays `UNENFORCED`
for containment; this spec adds no claim beyond the checks it tests.

**Not in scope:** output hygiene between roles (M9-T8), network egress
(M9-T7), containers (M1A).

## 3. Roles and users

| Role | OS user (default name) | Runs | Holds |
| --- | --- | --- | --- |
| Producer | `summa42-model` | triage classification, planning, implementation | producer CLI logins |
| Reviewer | `summa42-review` (new) | triage review, plan review, code review, fallback reviewers | reviewer CLI logins |
| Validation | `summa42-validate` | candidate code | nothing |

The Copilot executor that reviews human pull requests in Azure DevOps keeps
running as the producer user. It reviews work no model of this Collective
produced, so the independence rule does not apply to it, and its GitHub login
must not be copied into the reviewer home.

Each user is a nologin system account with its own home, mode 0700, and no
supplementary groups, created by `scripts/setup-model-user.sh` exactly as the
two existing users are.

## 4. Decisions

### 4.1 Configuration

- The reviewer user comes from `--review-model-user`, default
  `SUMMA42_REVIEW_MODEL_USER`. The default no longer falls back to
  `SUMMA42_MODEL_USER`. The flag is not added again; its default and its
  checks change.
- The single-stage review commands (`run-gh-plan-review`,
  `run-gh-code-review`, `run-gh-triage-review` and their `run-repo-*` aliases)
  keep the flag name `--model-user` for compatibility, but their default
  becomes `SUMMA42_REVIEW_MODEL_USER`.
- A new resolver `modelrun.ResolveReviewUser(name, producer, validation)`
  mirrors `ResolveValidationUser`: the user must exist, must not be root or
  the Box user, and must differ by uid and by name from the producer user and
  the validation user. Construction fails closed. There is no flag that
  accepts the same user for both roles.
- Fallback reviewers already copy the reviewer's configuration
  (`cfg := reviewer` in `fallbackConfig`), so they inherit the reviewer user.
  A test pins that.

### 4.2 The rule is checked against recorded facts, not only flags

A single-stage review command does not know which user the producer ran as, so
a flag check alone cannot stop an operator from starting it with the producer
user. The rule is therefore also enforced on data:

- Migration `000NN_model_calls_run_as.sql` adds `run_as TEXT NOT NULL DEFAULT
  ''` to `model_calls`. Every recorded call stores the OS user name it ran as.
- Before a review stage calls its model, it reads the `run_as` of the calls
  that produced the artefact it reviews (same `case_id`, producing stages).
  If any equals the reviewer's user, the stage refuses with a held result and
  a non-zero exit, and records no verdict. An empty `run_as` (a call recorded
  before the migration) also refuses: the plan or candidate is produced again.

The implementer locates the producing-stage names with
`grep -rn "model_calls" internal/ cmd/` and lists them in the pull request.

### 4.3 Mutual unreadability

`modelrun.Spec.Unreadable` already probes extra paths per user, through the
same launcher, with a positive control. It is used as follows:

| Calls of | must not read |
| --- | --- |
| Producer | reviewer home |
| Reviewer | producer home |
| Validation | producer home (today) and reviewer home (new) |

A readable path refuses the launch, as today.

### 4.4 Nothing survives a call

Model calls of one user run concurrently (pipeline and worker, several
Projects), so validation's `kill -KILL -1` of the whole user cannot be used
after each call. Two mechanisms, both with existing parts:

1. **Per call.** Every model call sets `Spec.Scratch`. When a call ends, for
   any reason, the Box sends `KILL` to the call's process group as that user.
   The process-group id is already reported on the first stderr line and
   already used for cancellation (`killModelGroup`); this extends it to the
   normal end of a call.
2. **When idle.** A process can leave its group with `setsid`. Each call holds
   a shared file lock `model-<user>.lock` next to the state database for its
   duration. When a call ends, the Box tries the exclusive lock without
   waiting; if it gets it, no other call of that user is in flight in any Box
   process, and it runs the existing `modelrun.Sweep` (kill everything of the
   user, remove stale scratch directories). Before the kill it counts the
   user's processes; a count above zero is recorded as a rogue signal on the
   Attempt of the call that just ended ("process survived a model call").

World-writable directories are part of the same cleanup: the sweep also
removes, as that user, every file the user owns under `/tmp`, `/var/tmp` and
`/dev/shm`. Model processes get `TMPDIR` inside their scratch directory, so
nothing legitimate lives there.

Before a reviewer call starts, the Box runs the idle sweep for the producer
user if it can take that user's exclusive lock, and the reverse. If it cannot
(a producer call is in flight), the reviewer call proceeds; the remaining
window is listed in section 6.

### 4.5 Configuration files a model must not be able to change for the next call

A model can write inside its own home. A producer call could change its own
CLI's settings, hooks or instruction files so that later calls of the same
role behave differently. That is a channel between Attempts of one role.

- The setup script writes, for each role home, a manifest of the
  instruction-bearing files it installed (CLI settings, MCP configuration,
  hook directories, `AGENTS.md`, `CLAUDE.md`, `GEMINI.md` at the top of the
  home) with their SHA-256, to a Box-owned file outside every role home.
  Login token files are listed separately and are exempt from hashing: CLIs
  refresh them.
- Before each call the launcher's model-side script computes the hashes and
  lists instruction-bearing file names that are not in the manifest. A
  mismatch or an unexpected file refuses the call and records a rogue signal.
  The Owner repairs it by re-running the setup script.

The list of instruction-bearing paths per harness is data in the harness
profile, not code. A profile without the list is refused for the reviewer
role.

### 4.6 Logins

- The setup script offers the reviewer's CLI logins for the reviewer home with
  the same per-file prompt, secret scan and filtering it uses for the producer
  home. Nothing is copied from one role home to the other.
- Copilot's login is a GitHub credential. It is never offered for the reviewer
  home.
- One-time Owner task: list every login file present in each role home and
  what it can do at its provider. Any login that can write to a repository or
  spend outside a subscription is removed or replaced. The result is recorded
  in `docs/runbook.md` when that file exists (M3-T5), until then in the pull
  request.

### 4.7 Process listings

Prompts of some harnesses travel on argv (`agy`, Copilot) and are visible in
`ps` to every local user for the duration of a call (README). With two model
users running at the same time, one role could read the other's prompt.

The startup probe checks, as each role user, whether it can read
`/proc/<pid>/cmdline` of a process owned by another user. If it can, the Box
logs one warning at startup that names the fix (mount `/proc` with
`hidepid=2`) and continues. It does not refuse: changing a host mount is the
Owner's decision (question 3 in section 8).

## 5. Build cards

Each is one pull request. All touch protected paths (`internal/modelrun/`,
`scripts/setup-model-user.sh`); the Owner reviews line by line.

**M9-T6a: reviewer user in configuration.** Sections 4.1 and 4.3.
Files: `internal/modelrun/modelrun.go` (resolver), `cmd/summa42-box/main.go`
(defaults, `prefixedModelConfig`), `ghpipeline.go`, `ghplan_review.go`,
`ghcode_review.go`, the triage review command, `internal/repoworkspace/validation.go`.
Tests: `TestReviewUserMustDifferFromProducer`, `TestReviewUserMustDifferFromValidation`,
`TestReviewUserDefaultsToItsOwnVariable`, `TestSingleStageReviewUsesReviewUserDefault`,
`TestFallbackReviewerKeepsReviewUser`, `TestReviewerCannotReadProducerHome`,
`TestProducerCannotReadReviewerHome`, `TestValidationCannotReadReviewerHome`
(the three probe tests use the launcher fakes in `internal/modelrun/modelruntest`).

**M9-T6b: recorded user and the data check.** Section 4.2. Depends: M9-T6a.
Files: one migration, the code that writes `model_calls`, the three review
stages.
Tests: `TestModelCallRecordsRunAs`, `TestReviewRefusedWhenProducerRanAsReviewUser`,
`TestReviewRefusedWhenProducerUserUnknown`, `TestRefusedReviewRecordsNoVerdict`.

**M9-T6c: setup script.** Sections 4.5 (manifest writing) and 4.6. Depends:
M9-T6a.
Files: `scripts/setup-model-user.sh`, `scripts/test_setup_model_user.py`.
Tests: reviewer user created as nologin with 0700 home; its sudoers drop-in
validated and rolled back on failure like the others; Copilot login never
offered for the reviewer home; manifest written outside the role homes;
re-run is idempotent.

**M9-T6d: nothing survives a call.** Section 4.4. Depends: M9-T6a.
Files: `internal/modelrun/modelrun.go`, `process_unix.go`, the callers that
build a `Spec` without `Scratch`.
Tests: `TestGroupKilledAtNormalEnd`, `TestIdleSweepRunsOnlyWithoutSiblingCalls`,
`TestSurvivorIsRecordedAsRogueSignal`, `TestSweepRemovesOwnedFilesInSharedTmp`,
`TestEveryModelCallUsesScratch`, and one `realhost` test (build tag as in
`internal/repoworkspace`) that starts a `setsid` child in a model call and
finds it gone.

**M9-T6e: configuration integrity and the process-listing probe.** Sections
4.5 (checking) and 4.7. Depends: M9-T6c, M9-T6d.
Tests: `TestChangedSettingsFileRefusesCall`, `TestUnexpectedInstructionFileRefusesCall`,
`TestRefreshedTokenFileDoesNotRefuse`, `TestProfileWithoutPathListRefusedForReviewer`,
`TestVisibleProcessListingWarnsOnce`.

Documentation in the last card: README sections "Automated GitHub pipeline"
and "Verify isolation on a real host" (a third user in every loop), and the
row in section 2.5 of the plan.

Done when (M9-T6): on a real host, after `sudo scripts/setup-model-user.sh`,
the `realhost` tests pass for three users, and a pipeline started with the
same user for both roles refuses to start.

## 6. What remains after this spec

| Channel | State |
| --- | --- |
| The artefact under review (plan text, code, comments, commit text) | Open by nature; M9-T8 adds hygiene. It carries no authority |
| Concurrent calls: a producer process alive while a reviewer call runs | Open in time, closed in space (separate homes, private scratch, cleaned shared directories at the next idle moment) |
| Loopback sockets and abstract Unix sockets between concurrent calls | Open until M9-T7 (egress) or M1A (network namespace per call) |
| Shared provider account (two roles on the same provider) | Open; profiles for the two roles should use different providers where the Owner has them |
| Timing and load | Open; not addressed |

## 7. Compatibility

- A Collective that sets only `SUMMA42_MODEL_USER` stops at startup of any
  review-capable command with a message that names
  `SUMMA42_REVIEW_MODEL_USER` and the setup script. This is intended: the old
  behavior is the gap.
- Cases whose plan or candidate was produced before M9-T6b are refused at
  review (unknown producer user) and are produced again.
- No change to policy, approvals, the operation boundary or the validation
  user.

## 8. Questions for the Owner

1. Reviewer user name `summa42-review`. Default: yes.
2. Which CLI logins go to the reviewer home. Default: Claude only, because the
   Owner decided that Claude reviews; the producer home keeps Gemini (`agy`),
   Command Code and opencode. Note: the fallback reviewer list in
   `cmd/summa42-box/fallbackreviewers.go` carries the comment that Claude
   stays out of pipeline review until the Owner decides otherwise; this answer
   is that decision.
3. `hidepid=2` on `/proc`. Default: warn, do not refuse; recommend it in the
   runbook for the new VM.
4. Cases produced before the change are produced again. Default: accepted.
