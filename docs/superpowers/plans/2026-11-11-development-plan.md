# Summa42 development plan: projects, continuity, delegation and self-adopted automation

> **For the implementing agent (Gemini Flash or any other model):** read
> sections 1 to 4 completely before touching code. Work on exactly one task
> card at a time, in the order given in section 5. Section 3 is binding; a
> change that breaks a rule there is rejected even if its tests pass.

**Date:** 11 November 2026. **Base:** `main` @ `9ad78eb`.

**Goal:** take Summa42 from "one supervised issue-to-draft-PR loop on one
repository" to a system the Owner can use at three scopes (one project, several
projects, the whole company), that keeps running without an operator, and that
finds routine work it can take over, proposes the automation, and adopts it by
itself where the Owner granted that in advance.

**Owner questions:** the decisions this plan assumed are listed with their
defaults in [Owner questions](2026-11-11-owner-questions-pl.md). A default
stands until the Owner answers. Tasks that depend on an unanswered question
name it (`needs Q<n>`); do not start those on a guess.

This plan continues the
[self-development roadmap](2026-10-07-self-development-roadmap.md) (stages 5 to
7 are milestones M1, M5 and M8 here) and the
[Company OS profile](../../COMPANY_OS.md) (M7). It does not change
[the Vision](../../VISION.md).

## 1. Where the project stands

The README table "What runs today" is the source of truth. In short:

| Runs | Library only (constructed, never called by a running path) | Not built |
| --- | --- | --- |
| Kernel: Task/Attempt leasing, evidence, policy, approvals, external operations, resource ledger, audit | `internal/company`, `obligation`, `invariant`, `attention`, `board` | Projects (a scope below the Collective) |
| GitHub issue to draft PR, supervised (A1, one case completed) | OCI TEB, MCP adapter and capability Session, context projection compiler | Money budgets, Owner notifications, backup and restore, external watchdog |
| ADO PR observation and review | | CI observation, merge, issue closure, standing delegation (A2) |
| Break glass, dead man switch, containment for three signals | | Automation discovery and adoption |
| | | Self-upgrade (A3), any Company OS function besides Build |

Four facts shape everything below:

1. **One active Mission for the whole Collective.** `missions_one_active` is a
   unique index (`00002_purpose.sql`). There is no way to run two projects side
   by side with separate budgets, grants or a separate pause.
2. **Configuration is flags.** A pipeline run needs about twenty flags and five
   separately started commands. Nothing describes a project declaratively.
3. **The Owner is still the operator.** Nothing notifies, backs up, restarts or
   reports cost. A1 took ten days and six interventions for one issue.
4. **Shelfware happened.** Between 6 and 10 November about 25,600 lines entered
   `main` that no running path called; they are parked
   ([parked spikes](../research/2026-11-10-parked-spikes.md)). Earlier, reviews
   were fabricated by a canned adapter
   ([incident](../../incidents/2026-10-21-canned-model-reviews.md)). Section 3
   and milestone M0 exist to make both impossible to repeat unnoticed.

## 2. Target shape

### 2.1 Three scopes, one kernel

```text
Collective (one Owner, one Box, one SQLite database)
  └── Project            NEW, kernel concept: a scope for purpose, authority and budget
        ├── Mission      one active Mission per Project (today: one per Collective)
        ├── Sources      repositories, trackers, inboxes bound to the Project
        ├── Envelope     a child resource envelope; the Project cannot spend more
        ├── Grants and standing delegations, valid only inside the Project
        ├── Board        the Project's work queue
        └── Cases, Tasks, Attempts, Evidence, Operations (unchanged)
Company OS profile (optional, above Projects)
  └── Company State, obligations, invariants, functions that span Projects
```

- **Project use:** one Project, one repository, no company model. This must be
  a complete and useful installation.
- **Multi-project use:** several Projects with separate budgets and grants, one
  Owner inbox across them.
- **Company use:** the Company OS profile on top. The dependency rule stays:
  the profile may depend on the kernel and on Projects, never the reverse.

A Project is a scope, not a new authority. It can only narrow what the
Collective may do.

### 2.2 The automation loop

The Owner's requirement: the system detects what it can take over and either
proposes the automation or adopts it, depending on its scope and permissions.
The Vision allows this only in one form (Articles 6, 7 and 27): authority never
expands by itself, proof earns a proposal, and routine work leaves the Owner
through a standing delegation. The loop is therefore:

```text
observe  ->  detect  ->  shadow  ->  decide  ->  run  ->  watch
Owner       repeated     record      inside a     as a     any failure or
actions     pattern,     what it     signed       standing rejection
and         evidence-    would       mandate:     delegation suspends it
manual      backed       have done   adopt and    or a     (contracting is
steps       candidate    and compare notify;      schedule  autonomous)
                                     outside:
                                     ask the Owner
```

- An **automation mandate** is an Owner-signed, per-Project statement of which
  classes of automation the Collective may adopt without asking (risk ceiling,
  reversibility, budget, count per period). Adopting inside a mandate is use of
  granted authority, not expansion.
- Everything outside a mandate becomes one decision packet in the Owner inbox,
  with the evidence, the exact delegation it would create and the way back.
- Detection is deterministic first (counts over recorded Owner actions). A
  model may later suggest candidates; it never adopts.

### 2.3 Autonomy ladder per Project

The A0 to A3 ladder from the Vision becomes a property of each Project, not of
the Collective. A new Project starts at A1. The Summa42 repository itself is
the first Project and stays the proving ground.

## 3. Rules for the implementing agent

These are checked in review. Milestone M0 turns most of them into tests.

**R1. One task card, one branch, one draft pull request.** Never merge. Never
mark a PR ready. The Owner merges.

**R2. Tests first.** Write the tests the card names, run them, confirm they
fail for the right reason, then implement. A test must call real kernel
services (`testutil` gives a real SQLite store). A test that only checks values
the test itself supplied is a defect.

**R3. No package or table without a caller.** New code must be reached from
`cmd/summa42` or `cmd/summa42-box` in the same pull request, with an acceptance
test over that path. No schema outside
`internal/state/sqlite/migrations/`. Applied migrations are never edited; add a
new numbered file (next free number: `00038`).

**R4. Never fabricate.** No canned model output, no hard-coded verdict, no
constant confidence, no matching on phrases from test fixtures, no stub that
returns success. If something cannot be built as specified, stop and write
`BLOCKED: <reason>` in the pull request description.

**R5. Protected paths need the Owner.** Do not change these unless the card
says so explicitly: `docs/VISION.md`, `internal/policy/default_profile.go`,
`internal/emergency/`, `internal/approvals/`, `internal/identity/`,
`internal/modelrun/`, `scripts/setup-model-user.sh`, `.github/workflows/`,
`LICENSE`, `SECURITY.md`. `docs/VISION.md` is never changed by an agent.

**R6. Authority only narrows.** No change may let a Task, a model process or a
Project do more than before unless the card is marked `DESIGN` and cites an
Owner-approved spec. No new dependency in `go.mod` without a card saying so.

**R7. State what you can prove.** An enforcement level, a capability or a
"runs" claim is written only when a test or a real run shows it. Update the
README table row in the same pull request that makes a claim true.

**R8. Small diffs.** A card is sized for at most about 400 changed lines of
non-test code. If it grows past that, stop and split it; do not continue.

**R9. Validation before the pull request.** Run `make verify-fast` during work
and `make verify` before opening the pull request (both arrive in M0-T1; until
then use the matrix in `CONTRIBUTING.md`). Always set
`GOCACHE=/tmp/summa42-full-go-cache`. Never skip, delete or weaken a test to
get green. `internal/ghtriage` tests refuse to run as root; run them as an
unprivileged user.

**R10. Stop conditions.** Stop and report instead of improvising when: a card's
"Depends" is not merged; a card needs an unanswered Owner question; the code
does not match what the card describes; a test outside the card's packages
fails; the change would touch a protected path.

**Card types.** `BUILD`: implement as written. `DESIGN`: write a spec in
`docs/superpowers/specs/` and stop; the spec needs the Owner's approval and
should be written or at least reviewed by a stronger model than the one that
will implement it. `OWNER`: only the Owner can do it.

**Execution mode.** `pipeline`: small enough for Summa42's own
`run-gh-pipeline` (one package, complete-file edits, at most a handful of
files). Run these through the pipeline; each one counts toward the ten A1 pilot
tasks. `agent`: needs an external coding agent working in a checkout.

**Review.** The reviewer must be a different model from the producer
(existing pipeline rule; it applies to agent mode as well).

## 4. Milestones and order

| ID | Milestone | Result | Depends |
| --- | --- | --- | --- |
| M0 | Guardrails | The rules in section 3 fail CI when broken | none |
| M1 | A1 at volume | Ten pilot tasks done, repair loop, per-case report, one `serve` command | M0 |
| M2 | Projects | Several Projects with separate Mission, budget, grants, pause | M0 |
| M3 | Continuity | Money budget, notifications, backup and restore, watchdog | M2 |
| M4 | Owner surface | One inbox; containment and quarantine commands; Owner key out of the Box user's reach | M2, M3 |
| M5 | Standing delegation (A2) | CI observation, delegation records, protected merge, issue closure | M1, M4 |
| M6 | Automation loop | Detect, shadow, propose, adopt inside a mandate, suspend on failure | M5 |
| M7 | Company OS for the first product | Obligations, operate, support and money functions on real sources | M6, needs Q1 to Q12 |
| M8 | Release and self-upgrade (A3) | Supervisor, canary, rollback | M5 |
| M9 | Containment hardening | OCI execution for validation and implementation; MCP capability sessions wired | may start after M3 |

M1 runs in the background from M0 onward: pilot tasks are taken from the
`pipeline` cards of every milestone. M2 comes before M3 because budgets,
notifications and pauses are all per Project; building them first would mean
rebuilding them.

## 5. Task cards

Each card lists: type and mode, what it depends on, files, tests, and when it
is done. Where a card says "locate", run the given `grep` first and record the
result in the pull request description; file names not marked "create" exist at
the base commit.

### M0. Guardrails

**M0-T1 `BUILD` `agent`: one verification command.**
Files: `Makefile`, `CONTRIBUTING.md`.
Add `verify-fast` (gofmt check, `go vet ./...`, `go build ./cmd/...`,
`go test ./... -count=1`) and `verify` (`verify-fast`, Python unit tests,
`scripts/verify_docs.py` on every tracked Markdown file, `go test -race ./...
-count=1 -timeout 45m`, the SQLite spike script). Both export
`GOCACHE=/tmp/summa42-full-go-cache` unless `GOCACHE` is set.
Done when: both targets pass on `main`; `CONTRIBUTING.md` names them.

**M0-T2 `BUILD` `pipeline`: orphan package gate.**
Create: `tests/acceptance/architecture_test.go`.
`TestEveryInternalPackageHasACaller`: run `go list -deps ./cmd/...` and
`go list ./internal/...`; fail for any internal package not in the dependency
list. Allowlist with a written reason per entry: `internal/testutil`,
`internal/securityregression`. If another package is orphaned at the base
commit, do not allowlist it silently: list it in the pull request for the
Owner to decide (park or wire).
Done when: the test passes and fails when a new empty package is added.

**M0-T3 `BUILD` `pipeline`: schema gate.**
Same file as M0-T2. `TestNoSchemaOutsideMigrations`: no `.go` file outside
`internal/state/sqlite/migrations/` and outside `_test.go` contains
`CREATE TABLE`, `ALTER TABLE` or `CREATE INDEX`.
`TestMigrationsAreSequential`: file numbers are contiguous and unique.

**M0-T4 `BUILD` `agent`: protected path gate.** Touches `.github/workflows/`
by design; the Owner reviews this card line by line.
Create: `scripts/check_protected_paths.py`, `scripts/test_check_protected_paths.py`.
Modify: `.github/workflows/ci.yml`.
The script takes a base ref, lists changed files, and exits non-zero when a
path from rule R5 changed, unless the pull request carries the label
`owner-approved-protected`. The list of paths lives in one place in the script.
Tests: changed protected file fails; with label passes; unrelated file passes.

**M0-T5 `OWNER`: repository settings.** Require the CI check on `main`; forbid
merging without it; decide pull request #176 (default in Q38: close it and keep
it as a research note, because no running path calls it).

**M0-T6 `BUILD` `pipeline`: fabrication tripwire.**
Create: `internal/securityregression/fabrication_test.go`.
Fail if a non-test `.go` file under `internal/` or `cmd/` contains a string
literal verdict returned unconditionally from a function named `Evaluate`,
`Review`, `Assess` or `Verdict` (parse with `go/ast`: a function whose body is
a single `return` of only literals). This is a tripwire, not a proof; say so in
the test comment.

### M1. A1 at volume

**M1-T1 `BUILD` `agent`: bounded automatic repair after REVISE.**
Today a `REVISE` holds the case and `scripts/requeue-revise.py` re-queues by
hand. Locate: `grep -n "REVISE" cmd/summa42-box/ghpipeline.go
internal/ghtriage/*.go`.
Behavior: on plan-review or code-review `REVISE`, the pipeline materializes a
new plan or implementation work that carries the reviewer's reasons as input,
at most 3 repair cycles per case and never past `max_steps` or the envelope.
Evidence of the old candidate is never reused for the new one. `BLOCK` still
holds.
Tests: `TestReviseStartsBoundedRepair`, `TestRepairCannotReuseOldAcceptance`,
`TestThirdReviseBlocksCase`, `TestRepairRespectsEnvelope`.
Done when: an acceptance test drives a case through REVISE then ACCEPT with a
fake model adapter from `internal/ghtriage/fakemodel`, without any manual
command. Remove `scripts/requeue-revise.py` and its test in the same change.

**M1-T2 `BUILD` `pipeline`: case list.**
Create: `cmd/summa42/cases_cmd.go`, `cmd/summa42/cases_cmd_test.go`; modify
`cmd/summa42/root.go`. `summa42 cases [--state S]` prints case ID, source,
object, state, current work, completed steps, last assessment reason, updated
time. Read-only; reads the store the way `emergency status` does.

**M1-T3 `BUILD` `agent`: per-case report.**
`summa42 case report <case-id> [--json]`: issue, plan hash, candidate SHA, PR
URL, evidence IDs, model calls and tokens (table `model_calls`), retries, wall
time per stage, and Owner interventions (approvals, rejections, manual blocks,
cancels). This is the report of roadmap section 9. Until M4-T1 exists,
interventions are counted from `approval` decisions and `summa42.owner.*`
evidence only; say so in the output.

**M1-T4 `BUILD` `agent`: one long-running command.**
Create: `cmd/summa42-box/serve.go`, `cmd/summa42-box/serve_test.go`.
`summa42-box serve` runs intake, the pipeline and the worker in one process as
supervised goroutines with the existing interval and recovery logic. It adds
no behavior of its own: it calls the existing `run*` functions. One failing
loop is restarted with backoff and logged; it does not stop the others. Flags
are the union of the three commands until M2-T3 replaces them with a Project
manifest.
Tests: `TestServeRunsAllLoopsOnce`, `TestServeRestartsFailedLoop`,
`TestServeStopsOnContextCancel`.

**M1-T5 `OWNER` + `pipeline`: the nine remaining pilot tasks.** Take the
`pipeline` cards of this plan in order (M0-T2, M0-T3, M0-T6, M1-T2, then later
ones). For each, the Owner opens one issue containing the card text, the
allowed paths and the validation commands. Record `summa42 case report` for
each in `docs/superpowers/research/2026-11-pilot-results.md`: interventions,
cost, wall time, repair cycles.
Done when: ten cases reached a merged pull request with no skipped gate, and
the median of Owner interventions per case is known.

### M2. Projects

**M2-T1 `DESIGN`: Project scope spec.**
Create: `docs/superpowers/specs/2026-11-project-scope-design.md`. It must
settle: the `projects` table; the default Project that adopts the existing
Mission, cases, envelopes and board; one active Mission per Project; how a
Project's envelope hangs under the Collective root envelope; which records gain
`project_id` directly and which reach it through their Mission; Project states
(`ACTIVE`, `PAUSED`, `ARCHIVED`) and what each refuses; the signed Project
manifest (M2-T3); what the Vision says that this must not contradict (Articles
3 to 6, 15, 20). It must show that a Collective with only the default Project
behaves exactly as today.

**M2-T2 `BUILD` `agent`: Project records and Mission per Project.** Depends:
M2-T1 approved.
Create: migration `00038_projects.sql` (table `projects`; `missions.project_id`
with backfill to the default Project; replace `missions_one_active` with a
unique index on `project_id` where `active = 1`), `internal/project/service.go`
and tests.
Modify: `internal/purpose/service.go` (`CreateMission` and `ActiveMission` take
a Project), `internal/runtime/box.go`, `cmd/summa42/mission_cmd.go`
(`--project`, default Project when omitted), new `cmd/summa42/project_cmd.go`
(`project create|list|show`, Owner-signed like `mission create`).
Tests: `TestUpgradeAdoptsExistingMissionIntoDefaultProject`,
`TestTwoProjectsEachHaveOneActiveMission`,
`TestSecondActiveMissionInSameProjectRefused`,
`TestProjectCreateRequiresOwnerSignature`.

**M2-T3 `BUILD` `agent`: signed Project manifest instead of flags.** Depends:
M2-T2.
A file `$SUMMA42_HOME/projects/<slug>/project.json` holds what the pipeline
flags hold today: source repository and identity, base branch, allowed
prefixes, context files, validation commands, producer and reviewer model
profiles, case grants (capabilities and actions), intake maintainers and bot
login, limits (`max_steps`, repair cycles). Unknown fields are rejected.
`summa42 project apply <slug>` shows the manifest, asks the Owner to confirm,
signs its SHA-256 with the Owner key and records the hash. `serve` and the
`run-*` commands accept `--project <slug>` and refuse a manifest whose hash is
not the recorded one. Credentials are never in the manifest; it names
credential files.
Tests: `TestUnsignedManifestRefused`, `TestEditedManifestRefusedUntilReapplied`,
`TestManifestRejectsUnknownField`, `TestManifestGrantsCannotExceedPolicy`,
`TestPipelineFlagsAndManifestAreEquivalent`.

**M2-T4 `BUILD` `agent`: Project envelope and Project pause.** Depends: M2-T2.
Each Project gets a child envelope of the Collective root at creation, with
limit zero until the Owner funds it (`summa42 project fund <slug> --limit N`,
Owner-signed). Intake uses the Project envelope. `summa42 project pause|resume
<slug>` is the break glass of one Project: it refuses new leases and new
external operations for that Project inside the same transactions the
Collective-wide gate uses (`internal/execution` lease gate, `operations`
prepare and dispatch). Locate the gate: `grep -rn "CheckLeaseAllowedTx"
internal/`. This card touches `internal/emergency/` only to add the scoped
check; mark the pull request for Owner review.
Tests: `TestPausedProjectRefusesLeaseOtherProjectContinues`,
`TestProjectCannotSpendBeyondItsEnvelope`,
`TestCollectiveFreezeStillStopsAllProjects`,
`TestResumeDoesNotRestoreRevokedLeases`.

**M2-T5 `BUILD` `agent`: serve all Projects fairly.** Depends: M1-T4, M2-T3.
`serve` without `--project` runs every `ACTIVE` Project that has an applied
manifest. Order of work across Projects: obligations first, then round-robin,
so one busy Project cannot starve another (Vision, Article 20).
Tests: `TestTwoProjectsBothAdvance`, `TestBusyProjectDoesNotStarveQuietOne`,
`TestArchivedProjectIsNotServed`.

**M2-T6 `BUILD` `agent`: board wired to intake.** Depends: M2-T2. This is the
first Company OS block to get a caller; it closes issue #124.
Intake calls `board.Service.SyncItem` for each observed item; each pipeline
stage transition calls `MoveCard` with the assessment's evidence IDs.
`summa42 board [--project <slug>]` prints the columns. A card move never causes
an external effect.
Tests: the six acceptance criteria of issue #124, as acceptance tests over the
real intake and pipeline path with a fake GitHub server.

**M2-T7 `BUILD` `agent`: a second, non-GitHub work source.** Depends: M2-T6,
needs Q14. Default: a local file inbox, `$SUMMA42_HOME/projects/<slug>/inbox/`,
where each Owner-written Markdown file becomes one card and one case. It proves
that a Project works without GitHub and gives the Owner a way to hand over work
that is not an issue. The connector implements the same interface as the GitHub
connector in `internal/board/connectors.go`; remove `FakeADOConnector` from
non-test code in the same change.

### M3. Continuity

**M3-T1 `DESIGN`: money ledger spec.** Integer micro-USD. A price table per
model profile (input and output token prices, or a flat subscription marked as
`PREPAID_QUOTA`). Unknown usage stays exposure and consumes budget. Caps per
case, per Project per day and per Collective per day, checked before each model
call. Relation to the existing request-unit envelopes: a second envelope kind,
not a conversion. Needs Q20 to Q23.

**M3-T2 `BUILD` `agent`: money ledger.** Depends: M3-T1 approved. Reservation
before each model call in `internal/modelrun` callers, settlement from reported
usage, refusal when a cap would be exceeded. `summa42 spend [--project]` shows
settled, reserved and unknown amounts.
Tests: `TestCallRefusedWhenCapWouldBeExceeded`, `TestUnknownUsageRetainsExposure`,
`TestUsageSettledOnceAfterRestart`, `TestSubscriptionModelCountsQuotaNotMoney`.

**M3-T3 `BUILD` `agent`: Owner notifications.** Needs Q24.
Create: `internal/notify/` (outbox table in a new migration, a sender
interface, one real sender). Default channel: a Discord webhook, because a
Discord bridge already exists; the webhook URL is read from a 0600 file.
Events: safe mode begins or is 24 hours away, an approval waits, a case is
blocked, a budget passes 80 percent, an attempt is contained, a backup fails.
Sending is an external operation of LOW risk through the existing commitment
boundary; a lost acknowledgement is reconciled, not blindly resent. The message
carries identifiers and reasons only, never repository content.
Tests: `TestEventIsDeliveredOnce`, `TestSenderFailureKeepsOutboxRow`,
`TestMessageContainsNoPayloadText`, `TestSafeModeWarningFiresBeforeHorizon`.

**M3-T4 `BUILD` `agent`: backup and restore.**
`summa42 backup create <dir>` uses SQLite `VACUUM INTO` plus a copy of the
evidence directory and a manifest of hashes; `backup verify <dir>`;
`backup restore <dir>` only into an empty home. After a restore, every
operation that was `DISPATCHED` or unknown at backup time is reconciled by
lookup before any work resumes; nothing is re-dispatched from restored state.
Keys are not in the backup unless `--include-keys` is given.
Tests: `TestRestoreReconcilesInsteadOfReplaying`, `TestVerifyDetectsCorruptEvidence`,
`TestRestoreRefusesNonEmptyHome`, `TestBackupWhileBoxIsWriting`.

**M3-T5 `BUILD` `agent`: watchdog outside the process.**
Create: `deploy/systemd/summa42-serve.service`, `summa42-watchdog.service`,
`summa42-watchdog.timer`, `summa42-backup.timer`, `docs/runbook.md`.
`serve` writes a liveness row every tick. `summa42 watchdog` (a separate short
process) checks it, checks the last backup age and the safe mode horizon, and
notifies through M3-T3 directly (it must work when the Box hangs). The watchdog
never restarts the Box itself; systemd does (`Restart=on-failure`).
Tests: `TestWatchdogReportsStaleLiveness`, `TestWatchdogReportsOldBackup`.
Done when: the runbook tells an Owner how to install, start, stop, back up,
restore and read why a case is blocked, and a second person could follow it.

### M4. Owner surface

**M4-T1 `BUILD` `agent`: Owner action record.**
Every Owner action gets one normalized row (new migration): actor, Project,
kind (`approve`, `reject`, `block`, `cancel`, `fund`, `apply`, `pause`,
`heartbeat`, `manual-run`), a class key (provider, descriptor type, recipe
stage, reason code), the subject ID and time. Written in the same transaction
as the action. This is the input of the automation loop (M6); without it the
loop has nothing to observe.
Tests: one per kind, plus `TestRecordIsWrittenInSameTransaction`.

**M4-T2 `BUILD` `agent`: the inbox.**
`summa42 inbox [--project]` lists everything that waits for the Owner, with the
reason and the evidence, ordered by cost of delay: approvals, blocked and held
cases, contained attempts, unfunded envelopes with waiting Tasks, safe mode
countdown, proposals (from M6). It wires `internal/attention`: related items
are grouped into one packet and the attention budget is shown. `summa42 inbox
decide <packet>` walks the packet and performs the existing signed actions.
Tests: `TestInboxMatchesCanonicalState` (compares against direct queries),
`TestRelatedApprovalsFormOnePacket`, `TestInboxIsReadOnlyUntilDecide`.

**M4-T3 `BUILD` `pipeline`: containment and quarantine commands.**
`summa42 contain clear <attempt-id> --reason`, `summa42 quarantine set|lift
--reason`, Owner-signed like `emergency freeze`. The library calls exist
(`internal/containment`, `internal/quarantine`); the README lists the missing
commands as a known limit.

**M4-T4 `DESIGN` then `BUILD`: Owner key custody.** Needs Q26. Before any
standing delegation exists, the Owner key must not be readable by a process
running as the Box user (README, "Limits to know"). Default: the key is
encrypted with a passphrase and decrypted only inside the `summa42` CLI for the
duration of one signed action; the Box user never holds the plaintext. A
hardware key or `ssh-agent` custody is the stronger option.
Done when: a test running as the Box user cannot produce a valid Owner
signature without the passphrase, and the custody mode is shown by `summa42
status`.

### M5. Standing delegation (A2)

The first attempt at this milestone was deleted on 21 October because parts of
it were fabricated (roadmap, update of 10 November;
[remediation plan](2026-10-21-review-remediation.md), Task 16). Rebuild it from
the [protected merge spec](../specs/2026-10-16-ci-observation-and-protected-merge-design.md).
Do not restore the deleted code from history.

**M5-T1 `DESIGN`: standing delegation spec.** Vision, Article 27. A delegation
record names: principal, Project, operation class (provider and descriptor
type), constraints (path prefixes, maximum diff size, excluded protected paths,
risk ceiling), limits per period (count, money), expiry, and the revocation
path. It is Owner-signed. Policy v3 receives `standing_delegation` as an input
computed by deterministic code, and allows a MEDIUM-risk operation without a
per-operation approval only when a valid delegation covers it. Each use is
recorded and appears in the Owner's digest. Any rejection, failed verification
or reverted change in the class suspends the delegation automatically. The spec
must list what can never be delegated (policy, keys, approvers, budgets,
workflow files, the delegation mechanism itself). Entry condition: M1-T5 done
and M4-T4 done.

**M5-T2 `BUILD` `agent`: CI observation bound to the exact SHA.**
Read-only. Required checks come from the Project manifest. A missing required
check is pending, not success. A CI infrastructure failure is distinguished
from a test failure; only the second starts a repair cycle (M1-T1).
Tests: the seven observer tests named in the reverted plan
(`2026-10-16-ci-observation-and-protected-merge.md`, Task 1), written against a
fake GitHub server, plus `TestInfrastructureFailureDoesNotStartRepair`.

**M5-T3 `BUILD` `agent`: delegation records and policy v3.** Depends: M5-T1
approved. Protected paths by design (`internal/policy/default_profile.go`,
`internal/approvals/`); Owner review line by line.
`summa42 delegation grant|list|show|revoke|suspend`.
Tests: `TestOperationOutsideDelegationStillNeedsApproval`,
`TestExpiredDelegationRefused`, `TestDelegationCannotCoverProtectedPath`,
`TestUseIsRecordedOncePerOperation`, `TestRejectionSuspendsDelegation`,
`TestDelegationOfOtherProjectDoesNotApply`, `TestPeriodLimitEnforced`.

**M5-T4 `BUILD` `agent`: protected merge and issue closure.** Depends: M5-T2,
M5-T3. A merge provider implementing `operations.Provider` with an expected
head SHA condition; `LookupOutcome` reads the pull request state; a moved base
that changes the result produces a new candidate and a new verification. The
issue is closed only after the merge commit is confirmed.
Tests: the five merge-gate tests of roadmap stage 6, plus
`TestMergeWithoutDelegationWaitsForOwner`.
Done when (A2): three small changes in the Summa42 Project go from issue to
merged without an Owner action, and one change to a protected path waits.

### M6. Automation loop

**M6-T1 `DESIGN`: automation loop spec.** Settles the proposal record and its
states (`CANDIDATE`, `SHADOW`, `PROPOSED`, `ADOPTED`, `SUSPENDED`, `RETIRED`),
the detection rules below with their thresholds, the mandate record, the shadow
comparison, the suspension rules and the digest. It must show, article by
article, why adoption inside a mandate does not violate Vision Articles 6, 7
and 14. Needs Q30 to Q35.

Detection rules for the first version (deterministic, over M4-T1 rows):

| Rule | Pattern | Proposal |
| --- | --- | --- |
| D1 | The Owner approved the same operation class N times in a window with zero rejections | A standing delegation for that class, with the tightest constraints that cover the observed instances |
| D2 | The Owner ran the same manual command on a regular cadence | A schedule that creates that Task |
| D3 | Cases of one class block at the same gate for the same reason, and the Owner unblocks them the same way | A manifest change (grant, limit or allowed path), shown as a diff |
| D4 | An obligation was fulfilled by hand on time K times | A Task template funded from a named envelope |

**M6-T2 `BUILD` `agent`: detector and proposals.** Depends: M6-T1 approved.
Create `internal/automation/` with a caller in `serve` (one detection pass per
tick, bounded). A proposal cites the Owner action rows it was derived from and
contains the exact record it would create. No model is involved.
`summa42 automation list|show`.
Tests: one positive and one negative test per rule, plus
`TestOneRejectionInWindowPreventsD1`, `TestProposalCitesItsEvidence`,
`TestSamePatternYieldsOneProposal`.

**M6-T3 `BUILD` `agent`: shadow mode.** A proposal in `SHADOW` changes nothing.
For each new matching instance it records what the automation would have done
and, after the Owner decides, whether the two agree. It moves to `PROPOSED`
only after the agreement threshold from the spec.
Tests: `TestShadowCausesNoEffect` (no operation prepared, no lease taken),
`TestDisagreementResetsShadow`.

**M6-T4 `BUILD` `agent`: mandate, adoption and suspension.** Depends: M5-T3,
M6-T3. `summa42 mandate grant|show|revoke --project` (Owner-signed). A
`PROPOSED` item inside the mandate is adopted by the Collective: the delegation
or schedule is created with the mandate as its authority source, and a
notification is sent. Outside the mandate it becomes an inbox packet;
`summa42 automation approve|reject`. Any failed verification, rejection or
reverted effect in an adopted automation suspends it without asking.
Re-activation always needs the Owner.
Tests: `TestInsideMandateAdoptsAndNotifies`, `TestOutsideMandateWaitsInInbox`,
`TestAdoptionCannotExceedMandateRiskCeiling`,
`TestMandateCannotGrantWhatOwnerCouldNotDelegate`,
`TestFailureSuspendsAdoptedAutomation`, `TestSuspendedNeedsOwnerToResume`,
`TestNoMandateMeansEverythingIsProposed`.

**M6-T5 `BUILD` `pipeline`: digest.** `summa42 digest [--since]` and a weekly
notification: what was taken over, what is proposed, what was suspended and
why, Owner actions per Project against the previous period, money spent.
Done when (M6): on the Summa42 Project, a D1 proposal arises from real Owner
approvals, passes shadow, and is adopted under a mandate the Owner granted.

**M6-T6 `DESIGN`: model-assisted discovery.** Later and optional. A model reads
sanitized summaries of a Project's sources and suggests candidates that the
deterministic rules cannot see. Its output is a `CANDIDATE` only and must pass
the same shadow and mandate path.

### M7. Company OS for the first product

This milestone depends on facts only the Owner has. The cards are templates
until Q1 to Q12 are answered; do not write connector code against a guessed
system.

**M7-T1 `DESIGN`: company map.** From the Owner's answers: the functions of
the company (build, release, operate, support, money, obligations), the system
of record for each, who does it today and how often, and what an error costs.
One table; it orders the rest of M7 by Owner time saved against risk.

**M7-T2 `BUILD` `agent`: connector contract and template.** One connector is: a
read capability with an action and parameter allowlist, an optional
`operations.Provider` for writes with `LookupOutcome`, a board connector, and a
fake server for tests. Prefer the existing MCP adapter where a maintained MCP
server exists for the system (this wires `adapters/mcp` and the capability
Session into execution; see M9-T2).
Done when: `docs/connector-guide.md` and one connector built from it exist.

**M7-T3 `BUILD` `agent`: first real obligation.** `summa42 obligation
add|list|fund`; the obligation planner (constructed in `internal/runtime/box.go`)
gets called from `serve`. One obligation from Q8 runs end to end and is funded
by the Owner.

**M7-T4 `BUILD`: operate.** For a data product the natural first invariants are
freshness, completeness and delivery of each dataset or feed (needs Q1 to Q4).
A breach raises a case in the operating Project; remediation runs only inside
granted authority. Wires `internal/invariant` and `internal/company`.

**M7-T5 `BUILD`: support.** Intake from the support channel into the board;
triage; a drafted reply as a MEDIUM-risk operation behind approval; later a D1
delegation for reply classes the Owner always approves unchanged.

**M7-T6 `BUILD`: money.** Read-only first: invoices, receivables, usage against
plan. Writes (reminders, invoices) are external operations of at least MEDIUM
risk and stay outside any mandate until the Owner says otherwise (Q11).

### M8. Release and self-upgrade (A3)

Roadmap stage 7, unchanged: a separate `cmd/summa42-supervisor`, a release
manifest bound to the merged SHA, backup before upgrade (M3-T4), canary with a
synthetic case, rollback only onto a compatible database, expand-contract
migrations. Start with a `DESIGN` card. The first product's own deployment is a
separate Project using the same supervisor pattern (needs Q5).

### M9. Containment hardening

**M9-T1 `BUILD` `agent`: validation in OCI.** Run validation commands through
the existing TEB Docker provider instead of the `summa42-validate` user alone,
when Docker is present; record the enforcement level actually achieved.

**M9-T2 `BUILD` `agent`: capability Sessions in execution.** Wire the MCP
adapter and capability Session so a Task's `RequiredCapabilities` become the
only tools its executor can reach.

**M9-T3 `BUILD` `agent`: context projection.** Call the compiler in
`internal/runmanifest/compiler.go` from the planner and implementer paths and
record the projection hash in the run manifest.

Parked spikes return only by the rule in the parked-spikes index: with a
caller, an acceptance test, and their listed findings resolved.

## 6. How to hand a card to the implementing agent

Use this prompt, one card per session:

```text
You are implementing one task card of the Summa42 development plan.
1. Read AGENTS.md and docs/superpowers/plans/2026-11-11-development-plan.md
   sections 1 to 4 completely.
2. Your card is <ID>. Check that every card under "Depends" is merged in main.
   If not, stop and say so.
3. Create branch <id-lowercase>-<short-name> from the latest main.
4. Write the tests the card names. Run them. Show that they fail.
5. Implement the minimum that makes them pass. Follow rules R1 to R10.
6. Run make verify. Paste the real output tail. Do not claim a pass you did
   not see.
7. Update the README "What runs today" row if the card made something run.
8. Open a draft pull request titled "<ID>: <card title>". In the description
   list: what changed, what you could not do, every deviation from the card.
If anything in the card does not match the code, stop and report. Do not
guess, do not widen the scope, do not add packages the card does not name.
```

A different model reviews every pull request against the card and section 3
before the Owner looks at it. The review asks four questions: does a running
path call the new code; do the tests exercise real kernel services; did any
authority widen; is every claim in the README true.

## 7. Risks

| Risk | Where it bites | Control |
| --- | --- | --- |
| A weaker model produces plausible code nothing calls | every card | R3, M0-T2, review question 1 |
| A weaker model weakens a test or a gate to get green | every card | R9, R5, different-model review, required CI |
| A design decision is taken by the implementer | `DESIGN` cards | Spec approved by the Owner first; stronger model writes or reviews it |
| Delegation or mandate widens authority | M5, M6 | Spec lists the undelegable; suspension is automatic; M4-T4 first |
| The Owner key is readable by the Box user | M5 onward | M4-T4 is an entry condition of M5 |
| Projects migration breaks the one running installation | M2-T2 | Upgrade test on a copy of the live database; backup first |
| Company connectors built on guesses | M7 | Cards blocked on Q1 to Q12 |
| Plan grows stale | all | A card that no longer matches the code is a stop condition (R10), not a licence to improvise |

## 8. What is deliberately not in this plan

Multi-Cube coordination, PostgreSQL, NATS, Dapr, inter-Collective delegation, a
decision engine, a digital twin, dreaming. None is needed for one Owner, one
product and several Projects. Each may return when a card in this plan needs
it.
