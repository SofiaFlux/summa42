# Summa42 development plan: projects, continuity, delegation and self-adopted automation

> **For the implementing agent (Gemini Flash or any other model):** read
> sections 1 to 4 completely before touching code. Work on exactly one task
> card at a time, in the order given in section 4.1. Section 3 is binding; a
> change that breaks a rule there is rejected even if its tests pass.

**Date:** 11 November 2026. **Base:** `main` @ `9ad78eb`.

**Goal:** take Summa42 from "one supervised issue-to-draft-PR loop on one
repository" to a system the Owner can use at three scopes (one project, several
projects, the whole company), that keeps running without an operator, and that
finds routine work it can take over, proposes the automation, and adopts it by
itself where the Owner granted that in advance.

**Owner decisions:** the Owner answered the plan's questions on 11 November
2026; the answers are recorded in
[Owner questions](2026-11-11-owner-questions-pl.md) and summarized in section
2.4.

This plan continues the
[self-development roadmap](2026-10-07-self-development-roadmap.md) (stages 5 to
7 are milestones M1, M5 and M8 here) and the
[Company OS profile](../../COMPANY_OS.md) (M7). It does not change
[the Vision](../../VISION.md). Section 9 maps every open issue and discussion
to the cards that serve it.

## 1. Where the project stands

The README table "What runs today" is the source of truth. In short:

| Runs | Library only (constructed, never called by a running path) | Not built |
| --- | --- | --- |
| Kernel: Task/Attempt leasing, evidence, policy, approvals, external operations, resource ledger, audit | `internal/company`, `obligation`, `invariant`, `attention` (constructed in the Box, never called); `internal/board` (not even imported outside its tests) | Projects (a scope below the Collective) |
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
Owner       repeated     record      a routine    as a     any failure or
actions     pattern,     what it     inside a     schedule rejection
and         evidence-    would       mandate:     or, once suspends it
manual      backed       have done   adopt and    signed,  (contracting is
steps       candidate    and compare notify;      as a     autonomous)
                                     anything     standing
                                     else: the    delegation
                                     Owner signs
```

- An **automation mandate** is an Owner-signed, per-Project standing
  delegation (Article 27) for one thing: turning a detected routine into a
  schedule or Task template for work that is already allowed without approval
  under the Project's existing grants and envelope. Adoption under a mandate
  creates no authority record. It never creates, widens or reactivates a
  delegation, a grant, a budget or a manifest entry; those are created only by
  an Owner signature (Articles 7 and 27: proof earns a proposal, and only the
  Owner enlarges the envelope).
- Everything else, in particular every proposed standing delegation, becomes
  one decision packet in the Owner inbox, with the evidence, the exact record
  it would create and the way back. The Owner's signature on that packet is
  what creates it. "Adopts by itself" therefore means: routines inside what the
  Owner already allowed. Taking over a decision the Owner used to approve
  always costs one signature, once per class.
- Detection is deterministic first (counts over recorded Owner actions). A
  model may later suggest candidates; it never adopts.

### 2.3 Autonomy ladder per Project

The A0 to A3 ladder from the Vision becomes a property of each Project, not of
the Collective. A new Project starts at A1. The Summa42 repository itself is
the first Project and stays the proving ground.

### 2.4 The first deployment (Owner decisions of 11 November 2026)

| Topic | Decision |
| --- | --- |
| Product | Data as a Service, delivered through an API and a web portal (dashboard) |
| Promises to customers | No formal SLA yet; contracts will carry one. Measure freshness, completeness and availability first; thresholds come later |
| Hosting and release | Azure; deployment through Azure Pipelines |
| Code | Many repositories, on GitHub and on Azure DevOps |
| People | The Owner only |
| Projects at the start | `summa42` (this repository) and one `daas` Project spanning many repositories |
| Second work source | Azure DevOps work items; milestone M2A adds the full ADO delivery path |
| Support intake | E-mail and the portal's contact form, which stores requests in the product database; a future API endpoint for data requests uses the same store |
| Billing | inFakt (sandbox today), already automated inside the product behind a provider abstraction; Summa42 observes it |
| Customer communication and financial documents | Not sent or issued by Summa42 at the start: a drafted reply is delivered to the Owner, who sends it. Later a send is an external operation of at least MEDIUM risk behind per-operation approval, and doing it without approval needs an Owner-signed delegation. Invoicing stays in the product |
| Private data and code | Three data classes (agentic executor spec, section 5.1): `public`; `private-code`, which may go to subscription models; `customer-data`, which goes only to zero-data-retention or local models and never enters a sandbox |
| Implementer | Gemini Flash through the `agy` profile; other models through Command Code or opencode. Direct API providers and more harnesses later (M3-T6) |
| Specs and review | Claude writes the `DESIGN` specs and reviews agent pull requests. In the pipeline Gemini writes and Claude or Command Code reviews, by complexity of the work |
| Model access | Gemini through `agy`, Claude, Copilot, Command Code (Go plan), opencode (free models), Ollama; no per-token API billing today |
| Money | 5 USD per case, 20 USD per day for the whole Collective; paid API calls only inside those caps |
| Notifications | Discord webhook first, behind a sender interface |
| Owner key | Passphrase-encrypted now, behind a signer interface that later admits a hardware key or an SSO-backed signer |
| Host | A Linux VM with systemd; nothing cloud-specific in the runtime (Azure today, another provider possible) |
| Backup | Daily, local plus Azure Blob, behind a destination interface |
| Owner horizon | Seven days |
| Automation thresholds | Propose after 5 approvals of one class in 30 days with no rejection; 5 consecutive agreeing shadow cases before adoption or proposal |
| First mandate | Schedules and Task templates for work that is LOW risk, reversible and has no external effect; everything else, including every delegation, is proposed and needs the Owner's signature |
| Constructors (discussion #136) | An adopted automation is the minimal Constructor v1 |

### 2.5 Design goal: no runaway agents, no private channels

One of the Owner's founding goals: an agent must not be able to break out and
do damage, and agents must not be able to coordinate with each other outside
the Collective's sight. The Vision states it (Articles 6, 9, 17, 18: "no number
of cooperating agents can collectively acquire a permission that none of them
received"). The design answer is not to trust detection but to make sure that
whatever an agent says or does carries no authority by itself:

- An agent's only output is a proposal or evidence, handed to the Box. Every
  effect goes through policy, approval and the commitment boundary, performed
  by trusted code that holds the credentials.
- Agents never address each other. The only path from one model call to
  another is evidence the Box stored and chose to project into the next prompt.
- Containment comes first, detection second (epic #5). A missed signal must
  not be what stands between an agent and damage.

Where the running system stands against that goal:

| Threat | Holds today | Gap | Card |
| --- | --- | --- | --- |
| A model reads the Owner key, tokens or state | Separate OS users for models and validation; startup probe refuses if a secret is readable | Executors other than models still run as the Box user; Owner key in a file | M4-T4, M1A-T3 |
| A model causes an external effect directly | The Box's tokens are unreadable to models; effects only through `operations` | Models have unrestricted network access; the model user's home holds copied CLI logins (a Copilot login is a GitHub credential) whose scopes are not audited | M9-T7, M9-T6 |
| Producer and reviewer coordinate through the filesystem | Validation runs as a third user so candidate code cannot poison the model's configuration | Producer and reviewer are the same OS user with one home; a call can leave files the next call reads | M9-T6 |
| Producer and reviewer coordinate over the network | none | No egress restriction on model processes | M9-T7 |
| The producer steers the reviewer through its own output (instructions in a plan, code comment or commit text) | Different models; recorded validation is not treated as proof; the Owner merges | No deterministic check; after M5 a delegation would rely on the reviewer | M9-T8 |
| Hidden content in output (invisible or bidirectional characters, non-text) | Non-text changes are rejected | Not checked in plans and review text | M9-T8 |
| An agent widens its own authority | Workflow grants are subset-checked; policy fails closed; contracting only | Delegations and mandates do not exist yet; they must not be creatable by an agent | M5-T1, M6-T1 |
| A runaway loop spends without limit | Step limit, envelope, no-progress stop, lease expiry | No money cap | M3-T2 |
| Nobody notices | Break glass, dead man switch, containment for three signals | No notification; two sensors have no caller | M3-T3, M9-T4 |

What cannot be promised: two models that both read the same artefact can
always pass information through it. The goal is that such a channel carries no
authority and reaches no secret, not that it cannot exist.

### 2.6 Design goal: full capability inside the boundary

The second founding goal balances the first: governance must not make the
agents weaker. A current model in an agentic harness can work for hours from
one prompt, run tools, build, test and iterate until a whole application
exists. A Summand must be able to do the same.

Today it cannot. Every model call in the pipeline is a bounded read-only
question: the harness profile is `read_only` or `no_tools`, the turns are few,
and an implementation comes back as complete-file edits in one JSON document,
at most 64 files and 256 KiB. The model never runs the code it writes. That was the right way to earn A1 on a host without a
sandbox; it is far below what the same model does in its own harness.

The rule that reconciles 2.5 and 2.6:

> Constrain the blast radius, not the agent. Inside its sandbox a Summand has
> every tool and as much time as its budget allows. At the edge of the sandbox
> everything is governed.

- **Inside:** a full agentic harness with write and execute permission, a
  shell, compilers, test runners, a package cache, hours of wall time.
- **The edge:** no route out except a Box-side gateway (model provider,
  package registries, a fetch restricted per Project); the provider credential
  stays outside where the harness allows it, and where it does not, a
  provider-only login inside is declared and limited to non-private Projects; the only thing that leaves is a
  commit that trusted code collects without executing anything the agent
  wrote, and usage measured at the proxy.
- **Outside:** unchanged. Independent validation, independent review,
  publication through the commitment boundary, the Owner or a delegation.

Auto-approve options that the harness profiles refuse today (rule in
`internal/ghtriage/climodel/profile.go`) become legal exactly when the call
runs in a sandbox whose enforcement level is proven, and never otherwise.
Milestone M1A builds this.

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
new numbered file with the next free number at the time the branch is cut (`00038` at the base commit).

**R4. Never fabricate.** No canned model output, no hard-coded verdict, no
constant confidence, no matching on phrases from test fixtures, no stub that
returns success. If something cannot be built as specified, stop and write
`BLOCKED: <reason>` in the pull request description.

**R5. Protected paths need the Owner.** Do not change these unless the card
says so explicitly: `docs/VISION.md`, `internal/policy/default_profile.go`,
`internal/emergency/`, `internal/approvals/`, `internal/identity/`,
`internal/modelrun/`, `scripts/setup-model-user.sh`, `.github/workflows/`,
`LICENSE`, `SECURITY.md`, and the guardrails themselves:
`scripts/check_protected_paths.py`, `Makefile`,
`tests/acceptance/architecture_test.go`, `internal/securityregression/`,
`internal/ghtriage/climodel/profile.go`, `AGENTS.md`, `go.mod`, this plan, and
every CI or pipeline definition file in any repository of any Project
(`.github/workflows/`, `azure-pipelines*.yml`). `docs/VISION.md` is never
changed by an agent.

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

**R11. Issues and discussions are the record.** Every card has one GitHub
issue (M0-T7). The pull request body says `Closes #<card issue>` and, where
section 9 names one, `Part of #<epic>`. Never write `Closes` or `Fixes` for an
epic: the Owner closes an epic when its closing criterion in section 9 is met.
The agent does not open, close, relabel or edit issues and does not post in
discussions. If a card conflicts with an issue's closing criterion or with an
open discussion, that is a stop condition (R10).

**R12. No agent-to-agent channel.** New code must not let one model call
reach another, or reach anything outside its sandbox, except as evidence handed
to the Box. No shared writable directory between roles, no model-held
credential that can cause an effect, no model output executed or obeyed as an
instruction. A card that needs an exception is a `DESIGN` card.

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
| M1A | Agentic execution | A Summand works for hours with full tools inside a sandbox; parity with the bare harness is measured | M0, M9-T6; M1A-T3 also M2-T3 |
| M2 | Projects | Several Projects with separate Mission, budget, grants, pause | M0 |
| M2A | Azure DevOps delivery | Work item to draft pull request in ADO, build observation | M2 |
| M3 | Continuity | Money budget, notifications, backup and restore, watchdog | M2 |
| M4 | Owner surface | One inbox; containment and quarantine commands; Owner key out of the Box user's reach | M2, M3 |
| M5 | Standing delegation (A2) | CI observation, delegation records, protected merge, issue closure | M1, M2A, M4, M9-T6, M9-T8 |
| M6 | Automation loop | Detect, shadow, propose, adopt inside a mandate, suspend on failure | M5 |
| M7 | Company OS for the first product | Obligations, operate, support and money functions on real sources | M6 |
| M8 | Release and self-upgrade (A3) | Supervisor, canary, rollback | M5 |
| M9 | Containment hardening | Separate reviewer user, model egress allowlist, output hygiene, OCI execution, capability sessions | M9-T6 and M9-T8 right after M0; the rest after M3 |

### 4.1 Execution order

The "Depends" line of a card is authoritative; the table above is a summary.
Card IDs say which milestone a card belongs to, not when it runs. The order:

1. M0-T1 to M0-T8.
2. M9-T6, M9-T8 (reviewer independence), M1-T1 to M1-T4, M1A-T1, M1A-T2.
3. M2-T1 to M2-T7.
4. M1A-T8, M1A-T3, M1A-T4, M1A-T7, M1A-T6, then M1A-T5.
5. M2A, M3 (M3-T7 and M3-T8 last), M7-T3.
6. M4, then M9-T4, M9-T7, M9-T3, M2-T8, M2-T9.
7. M5, M6, the rest of M7, M8, the rest of M9.

M1-T5 (the pilot tasks) draws `pipeline` cards from every step.

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
list. Allowlist with a written reason per entry. At the base commit six
packages are orphaned and all six are decided here: `internal/testutil`,
`internal/securityregression`, `internal/ghtriage/fakemodel` and
`internal/modelrun/modelruntest` (test helpers); `internal/board` (until
M2-T6 wires it; M2-T6 removes the entry); `internal/version` (until M1-T2
prints it; M1-T2 removes the entry). Any other orphan fails the test; never
extend the allowlist without a card that says so.
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
`owner-approved-protected`. The list of paths lives in one place in the script
and is the list of rule R5. CI runs the script from the base ref, never the
pull request's own copy. The label is a convenience, not the control: any
identity with write access can set it, so the control is a `CODEOWNERS` entry
for the same paths plus required Owner review (M0-T5); add the `CODEOWNERS`
file in this card.
Tests: changed protected file fails; with label passes; unrelated file passes.

**M0-T5 `OWNER`: repository settings.** Require both CI jobs (`test`,
`oci-acceptance`) and code-owner review on `main`; forbid merging without them. Pull request #176 was closed unmerged on the Owner's
decision (Q38); its branch remains.

**M0-T6 `BUILD` `pipeline`: fabrication tripwire.**
Create: `internal/securityregression/fabrication_test.go`.
Fail if a non-test `.go` file under `internal/` or `cmd/` contains a string
literal verdict returned unconditionally from a function named `Evaluate`,
`Review`, `Assess` or `Verdict` (parse with `go/ast`: a function whose body is
a single `return` of only literals). This is a tripwire, not a proof; say so in
the test comment.

**M0-T7 `OWNER`: one issue per card.** Before a card is started, an issue
exists for it: title `<ID>: <card title>`, body is the card text plus allowed
paths and validation commands, and `Part of #<epic>` where section 9 names one.
Create them a milestone at a time, not all at once: intake picks up maintainer
issues, so an issue is opened only when its "Depends" are merged and it is
meant to run (Q44). `DESIGN` cards whose question is still open start as a
discussion, not an issue (Q45).

A label named by intake's auto-grant setting gives an issue
`workspace.repo.write` and `workspace.test` (`internal/ghissue/observe.go`);
only the Owner applies it, only to `pipeline` cards.

**M0-T8 `OWNER`: confirm the implementer.** The README lists `agy`'s success
result as unconfirmed. Run one live `summa42-box model-probe` with the `agy`
profile and the Gemini Flash model as the model user, and one pilot card,
before relying on it for M1-T5. `agy` refuses prompts over 110000 bytes; keep
planner context under it.

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
Done when: an acceptance test drives a case through REVISE then ACCEPT without
any manual command. `internal/ghtriage/fakemodel` only classifies and reviews
triage; the scripted plan, implementation and verdict answers for this test go
through the real launcher with the test double in
`internal/modelrun/modelruntest`, extended there or in `_test.go` files only.
No scripted answer may exist in non-test code (R4). Remove `scripts/requeue-revise.py` and its test in the same change.

**M1-T2 `BUILD` `pipeline`: case list.**
Create: `cmd/summa42/cases_cmd.go`, `cmd/summa42/cases_cmd_test.go`; modify
`cmd/summa42/root.go`. `summa42 cases [--state S]` prints case ID, source,
object, state, current work, completed steps, last assessment reason, updated
time. `summa42 --version` prints `internal/version` (remove its entry from the
M0-T2 allowlist). Read-only; reads the store the way `emergency status` does.

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
manifest. `serve` is the single coordinator of state transitions and
publication for a Collective: a second `serve` on the same database refuses to
start (roadmap stage 5).
Tests: `TestServeRunsAllLoopsOnce`, `TestServeRestartsFailedLoop`,
`TestServeStopsOnContextCancel`, `TestSecondServeRefused`,
`TestTwoDriversDoNotPublishTwice`.

**M1-T5 `OWNER` + `pipeline`: the nine remaining pilot tasks.** Take the
`pipeline` cards of this plan in order (M0-T2, M0-T3, M0-T6, M1-T2, then later
ones). For each, the Owner opens one issue containing the card text, the
allowed paths and the validation commands. Record `summa42 case report` for
each in `docs/superpowers/research/2026-11-pilot-results.md`: interventions,
cost, wall time, repair cycles.
Done when: ten cases reached a merged pull request with no skipped gate, and
the median of Owner interventions per case is known.

### M1A. Agentic execution

Section 2.6. This milestone decides how much the system can do; do it early.
It reuses what exists: the TEB Docker provider (`internal/teb`), the launcher
(`internal/modelrun`), `execution.RenewLease`, the pinned checkout
(`internal/repoworkspace`), the Codex and Copilot executor adapters.

**M1A-T1 `DESIGN`: agentic workspace executor spec.** Written:
[agentic workspace executor](../specs/2026-11-11-agentic-workspace-executor-design.md).
It is binding for every M1A card once the Owner approved it, and its section 11
splits M1A-T3 and M1A-T8 into smaller cards (M1A-T3a to T3c, M1A-T8a and T8b)
and adds the harness spike M1A-T1b. Where a card below and the spec differ, the
spec wins. In short: one hardened container per Attempt; a Box-side gateway as
the only network; two credential modes (credential at the gateway, or a
provider-only login inside for subscription harnesses, never for private
Projects); collection through a separate no-network container into a fresh
Box-owned repository; one Box-authored candidate commit.

**M1A-T1b `OWNER` with `agent`: harness spike.** Depends: M1A-T1 approved. For
`agy`, Claude, Command Code and opencode: does it run headless in the sandbox
image in write mode, which credential mode does it need, where does its token
live, how does it refresh. Needs the Owner's logins. Result in
`docs/superpowers/research/`; it decides which harness M1A-T3c builds first.

**M1A-T2 `BUILD` `agent`: long Attempts.** A lease that an agentic Attempt
renews by heartbeat (`execution.RenewLease`), with fencing unchanged: a missed
heartbeat expires the lease, break glass and containment still revoke it at
once, and a revoked or expired Attempt cannot record a result. Budget is
checked at every renewal, so a run stops when its envelope is spent.
Tests: `TestHeartbeatKeepsLeaseForHours` (fake clock), `TestMissedHeartbeatExpires`,
`TestFreezeStopsRenewal`, `TestRenewalRefusedWhenEnvelopeSpent`,
`TestExpiredAttemptCannotRecordResult`.

**M1A-T3 `BUILD` `agent`: the agentic implement stage.** Depends: M1A-T1
approved, M1A-T2, M1A-T8, M2-T3, M9-T6. Split into several pull requests if it
passes the limit of R8. It changes protected paths
(`internal/ghtriage/climodel/profile.go`, the CI workflow for the OCI gate);
Owner review. A new executor runs a harness profile in agent mode
inside the sandbox on a pinned checkout, with the accepted plan as its task.
The profile validator accepts write and auto-approve options only for a
sandboxed call with a proven enforcement level. Trusted code collects the
commits, checks them against the Project's rules (protected paths, secrets,
size limits from the manifest, not the fixed 64 files) and records the
candidate exactly as the current implement stage does. Validation, code review
and publication are unchanged. The complete-file edit stage stays as the
fallback where no sandbox exists.
Tests: `TestAgentCanBuildAndRunTestsInside`, `TestNoCredentialReachableInside`,
`TestOnlyAllowlistedHostsReachable`, `TestResultIsReadFromGitNotFromAgentClaim`,
`TestAutoApproveRefusedOutsideSandbox`, `TestHostileGitConfigIsNotExecuted`,
`TestFreezeKillsSandbox`, `TestPaidProfileRefusedWithoutMoneyLedger`, and the
OCI gate in CI.

**M1A-T4 `BUILD` `agent`: checkpoints and resume.** Every commit the agent
makes is captured as checkpoint evidence while it works. After a crash, a lost
lease or a restart, a new Attempt continues from the last checkpoint commit
and its commit messages, instead of starting again. No harness session file
or other model-written state is carried over outside that evidence. Old checkpoints
are never accepted as the new Attempt's result.
Tests: `TestResumeStartsFromLastCheckpoint`, `TestCheckpointOfOldAttemptIsNotAResult`.

**M1A-T5 `DESIGN` then `BUILD`: large and greenfield work.** Depends: M1A-T3.
Three separate build cards after the spec, each with its own tests. Publication through a
Git push performed by the trusted publisher instead of per-file API calls, so
the size of a change is bounded by the Project manifest and the budget, not by
64 files. A Project may be declared greenfield: a new repository with no
allowed-path restriction inside it except the protected paths of R5. A plan may fan out into child Tasks that
run in parallel sandboxes (the single-host form of discussion #138).

**M1A-T6 `OWNER` with `agent`: parity benchmark.** The Owner starts the runs;
the results file is generated from `summa42 case report --json`, not written
by hand. The bare-harness baseline runs in the same sandbox image, never with
full permissions on the host. A small fixed set of tasks (a bug
fix, a feature across packages, a new small application from one prompt) run
twice: with the bare harness, and through Summa42 with the same model and
budget. Record completion, wall time, cost and Owner actions in
`docs/superpowers/research/`. Repeat when a model, a harness or the executor
changes.
Done when (M1A): through Summa42, one prompt produces a working, tested small
application in an hours-long unattended run, and every task the bare harness
completes and Summa42 does not is listed with its cause. A gap is closed by
improving the executor, never by relaxing a control.

**M1A-T8 `BUILD` `agent`: gateway for sandboxes.** Depends: M1A-T1 approved.
Two cards (spec section 11). The sandbox's only network is a Box-side gateway:
a registry mirror and a restricted fetch (T8a), and a provider gateway that
adds the credential outside the sandbox for harnesses that accept a base URL
(T8b). The existing CONNECT proxy is its outbound side. It records every
request and every refusal (a refusal is a rogue signal).
Tests: `TestSandboxHasNoRouteButGateway`, `TestCredentialAddedOutsideSandbox`,
`TestFetchRefusedForPrivateDataClass`, `TestFetchOutsideAllowlistRefused`.

**M1A-T7 `BUILD` `agent`: new dependency gate.** Depends: M1A-T3, M1A-T8. An agent with full tools will
add libraries. When the collected commits change a dependency manifest or lock
file (`go.mod`, `package.json`, `requirements*.txt`, `*.csproj` and the like,
listed as data), the candidate is held with one Owner packet per new
dependency: name, version, licence, and what the diff uses it for. Licence and
known-vulnerability data are fetched through the mediated fetch tool; if they
cannot be fetched the packet says so instead of guessing. Updating an existing
dependency inside its major version does not hold (that is the monthly
obligation of M7-T3).
Tests: `TestNewDependencyHoldsCandidate`, `TestPatchUpdateDoesNotHold`,
`TestUnknownLicenceIsReportedAsUnknown`.

### M2. Projects

**M2-T1 `DESIGN`: Project scope spec.**
Create: `docs/superpowers/specs/2026-11-project-scope-design.md`. It must
settle: the `projects` table; the default Project that adopts the existing
Mission, cases, envelopes and board; one active Mission per Project; how a
Project's envelope hangs under the Collective root envelope; which records gain
`project_id` directly and which reach it through their Mission; Project states
(`ACTIVE`, `PAUSED`, `ARCHIVED`) and what each refuses; the signed Project
manifest (M2-T3), which must describe several repositories on different hosts
(GitHub and Azure DevOps) in one Project, each with its own base branch,
allowed paths, validation commands and data class (`public`, `private-code`
or `customer-data`; agentic executor spec, section 5.1); what the Vision says that this must not contradict (Articles
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
flags hold today, per repository (a Project may list several, on GitHub and
on Azure DevOps): source repository and identity, base branch, allowed
prefixes, context files, validation commands, data class, producer and reviewer model
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
prepare and dispatch). Locate the gates: `grep -rn "CheckLeaseAllowedTx\|CheckExecutionAllowed"
internal/`. Only the lease gate runs inside a transaction today; the operation
gate is a plain call. The Project check for operations must be made inside the
prepare and dispatch transactions, so a pause and a dispatch cannot interleave. This card touches `internal/emergency/` only to add the scoped
check; mark the pull request for Owner review.
Tests: `TestPausedProjectRefusesLeaseOtherProjectContinues`,
`TestProjectCannotSpendBeyondItsEnvelope`,
`TestCollectiveFreezeStillStopsAllProjects`,
`TestResumeDoesNotRestoreRevokedLeases`.

**M2-T5 `BUILD` `agent`: serve all Projects fairly.** Depends: M1-T4, M2-T3.
`serve` without `--project` runs every `ACTIVE` Project that has an applied
manifest. Order of work across Projects: round-robin, so one busy Project cannot starve
another (Vision, Article 20). Obligations take precedence from M7-T3 on.
Tests: `TestTwoProjectsBothAdvance`, `TestBusyProjectDoesNotStarveQuietOne`,
`TestArchivedProjectIsNotServed`.

**M2-T6 `BUILD` `agent`: board wired to intake.** Depends: M2-T2. It closes
issue #124. The board is the Collective's work queue (issue #124), so it
becomes a kernel service here, and `docs/COMPANY_OS.md` is updated in the same
change to say so; the same holds for `internal/attention` in M4-T2. Company
State, obligations and invariants stay in the profile. Files: locate the
transitions with `grep -n "Assess(" cmd/summa42-box/*.go internal/ghtriage/*.go`
and list them in the pull request.
Intake calls `board.Service.SyncItem` for each observed item; each pipeline
stage transition calls `MoveCard` with the assessment's evidence IDs.
`summa42 board [--project <slug>]` prints the columns. A card move never causes
an external effect.
Tests: the six acceptance criteria of issue #124, as acceptance tests over the
real intake and pipeline path with a fake GitHub server.

**M2-T7 `BUILD` `pipeline`: local file inbox.** Depends: M2-T6. Small. `$SUMMA42_HOME/projects/<slug>/inbox/`: each Owner-written Markdown file
becomes one card and one case. It gives the Owner a way to hand over work that
is not an issue or a work item. The connector implements the same interface as
the GitHub connector (interface in `internal/board/types.go`).

**M2-T8 `DESIGN` then `BUILD`: Project memory.** Depends: M2-T2, M9-T3, M9-T8,
M4-T2. An
agent should not relearn the Project on every Task. Per Project: a generated
repository map (structure, build and test commands, entry points) refreshed on
each new base commit; conventions and decisions the Owner wrote or approved;
findings from earlier cases. All of it is evidence with provenance (Vision,
Article 10), selected into the prompt by the context projection compiler with
its hash in the run manifest. The repository map is generated by deterministic
code, not by a model. What a Summand learned enters memory only as an inbox
packet the Owner approves; a memory item is never adopted under a mandate.
Every item carries the data class of its source, and an item from a
`customer-data` source is never projected into a call to a model without zero data retention
or into another Project. Model-written items pass M9-T8 first.
Tests: `TestMapIsRegeneratedOnNewBase`, `TestMemoryItemCitesItsEvidence`,
`TestSummandCannotWriteMemoryDirectly`, `TestProjectionHashRecorded`,
`TestMemoryItemNeedsOwnerApproval`, `TestPrivateItemNotProjectedToNonZDRModel`.

**M2-T9 `DESIGN` then `BUILD`: work that is not code.** Depends: M1A-T3,
M2-T7, M4-T2. A second recipe next to issue-to-pull-request: a Task whose result is an
artefact (an analysis, a report, a data extract, a document) produced in the
same sandbox with the same tools, stored as evidence with the sources it used,
reviewed by the independent reviewer, and delivered to the Owner's inbox. No
external effect unless a separate operation publishes it. This is what lets a
Project serve the company and not only its repositories.
Tests: `TestArtefactTaskEndsInInboxNotPullRequest`, `TestArtefactListsItsSources`,
`TestArtefactTaskCausesNoExternalEffect`.

### M2A. Azure DevOps delivery

Today Summa42 reads Azure DevOps and reviews pull requests there
(`internal/adomcp`, `adoreview`, `adoeffects`). Part of the product's code
lives in ADO, so the `daas` Project needs the whole path. Reuse the GitHub
pipeline's stages; only the source and the effects differ.

**M2A-T1 `DESIGN`: ADO delivery spec.** Work item to case (which types and
states are picked up, who counts as a maintainer, what moves a revision);
checkout of an ADO Git repository pinned to a SHA; publication as two protected
operations (branch, draft pull request) in `internal/adoeffects` with
`LookupOutcome`; build observation bound to the exact SHA; what the existing
reviewer-vote path keeps. Credentials stay in the trusted component and never
reach a model. The repositories are `private-code`; work items may quote
customer text, and a work item marked `customer-data` goes only to
zero-data-retention or local models.

**M2A-T2 `BUILD` `agent`: work item intake.** Depends: M2-T6, M2A-T1 approved.
An ADO work item becomes one card and one case through a real connector;
remove `FakeADOConnector` from non-test code in the same change. Uses the
existing read capability `ado.work_item.read`.
Tests: the acceptance criteria of issue #124 repeated for an ADO source with a
fake ADO server, plus `TestNonMaintainerCommentDoesNotMoveRevision`.

**M2A-T3 `BUILD` `agent`: branch and draft pull request in ADO.** Depends:
M2A-T2. Same guarantees as GitHub publication: exact reviewed commit, create-only
branch, base unchanged, MEDIUM risk behind approval, a lost acknowledgement is
reconciled by lookup and never blindly repeated.
Tests: the ADO equivalents of the publication tests in `internal/ghpublish`.

**M2A-T4 `BUILD` `agent`: Azure Pipelines observation.** Depends: M2A-T3.
Read-only, bound to the exact SHA, through the existing `ado.build.status`
capability; a missing required build is pending, an infrastructure failure does
not start a repair. Tests: the CI observer cases listed in card M5-T2,
written here for ADO (M5-T2 itself is not a dependency).

### M3. Continuity

**M3-T1 `DESIGN`: money ledger spec.** Integer micro-USD. A price table per
model profile (input and output token prices, or a flat subscription marked as
`PREPAID_QUOTA`). Unknown usage stays exposure and consumes budget. Caps per
case, per Project per day and per Collective per day, checked before each model
call. Relation to the existing request-unit envelopes: a second envelope kind,
not a conversion. Caps: 5 USD per case and 20 USD per day for the Collective (Q20).
Subscriptions (Q21): Gemini through `agy`, Claude, Copilot, Command Code on
its Go plan, opencode on free models, Ollama locally; nothing is paid per token
today, so every current profile is `PREPAID_QUOTA` and the price table, kept as
data the Owner edits, starts with quota limits only.

**M3-T2 `BUILD` `agent`: money ledger.** Depends: M3-T1 approved. Reservation
before each model call in `internal/modelrun` callers, settlement from reported
usage, refusal when a cap would be exceeded. `summa42 spend [--project]` shows
settled, reserved and unknown amounts.
Tests: `TestCallRefusedWhenCapWouldBeExceeded`, `TestUnknownUsageRetainsExposure`,
`TestUsageSettledOnceAfterRestart`, `TestSubscriptionModelCountsQuotaNotMoney`.

**M3-T3 `BUILD` `agent`: Owner notifications.**
Create: `internal/notify/` (outbox table in a new migration, a sender
interface, one real sender). First channel: a Discord webhook (Q24), because a
Discord bridge already exists; the webhook URL is read from a 0600 file.
Events: safe mode begins or is 24 hours away, an approval waits, a case is
blocked, a budget passes 80 percent, an attempt is contained, a backup fails.
Sending is an external operation of LOW risk through the existing commitment
boundary; a lost acknowledgement is reconciled, not blindly resent. The message
carries identifiers and reason codes from a closed list only: never repository
content and never free text a model wrote (a reason written by a model would
make the webhook a model-controlled way out).
Tests: `TestEventIsDeliveredOnce`, `TestSenderFailureKeepsOutboxRow`,
`TestMessageContainsNoPayloadText`, `TestSafeModeWarningFiresBeforeHorizon`.

**M3-T4 `BUILD` `agent`: backup and restore.** Two pull requests: create,
verify and restore first; push second.
`summa42 backup create <dir>` uses SQLite `VACUUM INTO` plus a copy of the
evidence directory and a manifest of hashes; `backup verify <dir>`;
`backup restore <dir>` only into an empty home. After a restore, every
operation that was `DISPATCHED` or unknown at backup time is reconciled by
lookup before any work resumes; nothing is re-dispatched from restored state.
Never in a backup: keys, `config.json` (control token), `github_token`,
`gh-app.pem`, `discord_auth.json`, webhook and credential files; the list is
explicit and tested. The manifest of hashes is signed with the Owner key at
`backup create`, and `restore` refuses a manifest whose signature does not
verify against the Owner public key given on the command line: a restore from
a tampered copy could otherwise bring back forged approvals. A pushed backup is
encrypted on the host before upload; it contains private code and later
customer text.
`backup push` copies a verified backup to a remote destination behind a small
interface; the first destination is Azure Blob (Q27), and the interface must
not assume Azure so that an S3-compatible store can follow. The push is an
external operation of LOW risk. Schedule: daily.
Tests: `TestPushUploadsOnlyVerifiedBackup`, `TestPushedBackupIsEncrypted`,
`TestRestoreRefusesUnsignedManifest`, `TestBackupExcludesEveryCredentialFile`, `TestRestoreReconcilesInsteadOfReplaying`, `TestVerifyDetectsCorruptEvidence`,
`TestRestoreRefusesNonEmptyHome`, `TestBackupWhileBoxIsWriting`.

**M3-T5 `BUILD` `agent`: watchdog outside the process.** Single-host form of
discussion #137.
Create: `deploy/systemd/summa42-serve.service`, `summa42-watchdog.service`,
`summa42-watchdog.timer`, `summa42-backup.timer`, `docs/runbook.md`.
`serve` writes a liveness row every tick. `summa42 watchdog` (a separate short
process) checks it, checks the last backup age and the safe mode horizon, and
notifies through a second, declared path: the same sender called directly,
without the outbox, because it must work when the Box or the database hangs.
It sends reason codes only. A watchdog on the same host cannot report a dead
host, so `serve` also pings an optional external dead-man URL (a healthcheck
service that alerts when pings stop); the URL comes from a 0600 file. The watchdog
never restarts the Box itself; systemd does (`Restart=on-failure`). The unit
files and the runbook assume a plain Linux VM with systemd and nothing specific
to one cloud (Q28).
Tests: `TestWatchdogReportsStaleLiveness`, `TestWatchdogReportsOldBackup`.
Done when: the runbook tells an Owner how to install, start, stop, back up,
restore and read why a case is blocked, and a second person could follow it.

**M3-T6 `DESIGN` then `BUILD`: direct API model providers.** Depends: M3-T2,
M1A-T8.
Today every model runs through a CLI harness profile. Add a profile kind that
calls a provider API (OpenAI-compatible first; then Anthropic, Azure AI
Foundry, Vertex, Bedrock as data, not code, where the wire format allows). The
caller is a small binary run as the model user through `internal/modelrun`, so
the isolation and the launcher stay the same; the API key is added by the
provider gateway (M1A-T8b) and is never readable by the model process (R12). Reported token usage settles the money ledger;
a provider without zero data retention is refused for `customer-data`.
New harnesses (for example Pi) remain profile data through the existing
`model-profile-draft`, `model-probe`, `model-profile-adopt` path.

**M3-T7 `DESIGN` then `BUILD`: model routing by task class.** Depends: M3-T2,
M1A-T6. The Project manifest maps task classes (triage, plan, small change,
large change, review, artefact) to an ordered list of model profiles. The order
is proposed from the parity benchmark and from recorded outcomes
(`internal/experience` already returns one preferred executor kind per task
class under an Owner grant; pipeline model profiles are not scheduler
executors, so the spec decides whether to extend that mechanism or route
profiles separately, and must not end with two that disagree). A profile over its quota
or cap falls through to the next; the reviewer is never the producer's model;
`customer-data` skips profiles without zero data retention.
Tests: `TestExhaustedProfileFallsThrough`, `TestReviewerNeverEqualsProducer`,
`TestPrivateDataSkipsNonZDRProfile`, `TestRoutingNeverWidensCapability`.

**M3-T8 `BUILD` `agent`: drills.** Depends: M3-T4, M3-T5; the runaway drill
uses only the signals that exist when it is built and gains the egress and
workspace signals with M9-T7 and M9-T4. A scheduled,
automatic exercise in a scratch home that never touches live state or real
providers: restore the latest backup and reconcile; freeze and resume; a
simulated runaway agent (ungranted capability, denied egress, write outside the
workspace) that must end contained. Each drill writes a result as evidence and
a failed drill notifies the Owner. Uses the scenario runner of M8-T2 once it
exists.
Tests: `TestDrillNeverTouchesLiveHome`, `TestFailedRestoreDrillNotifies`,
`TestRunawayDrillEndsContained`.

### M4. Owner surface

**M4-T1 `BUILD` `agent`: Owner action record.**
Every Owner action gets one normalized row (new migration): actor, Project,
kind (`approve`, `reject`, `block`, `cancel`, `fund`, `apply`, `pause`,
`heartbeat`, `manual-run`), the key that signed it, a class key (provider, descriptor type, recipe
stage, reason code), the subject ID and time. Written in the same transaction
as the action. This is the input of the automation loop (M6); without it the
loop has nothing to observe.
A `manual-run` is a hand-started `summa42-box run-*` or maintenance command;
it is not Owner-signed, so its row is written at process start by the command
itself and is marked unsigned. Rule D2 reads these rows; rule D1 reads only
signed ones.
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

**M4-T4 `DESIGN` then `BUILD`: Owner key custody.** Before any
standing delegation exists, the Owner key must not be readable by a process
running as the Box user (README, "Limits to know"). Decision (Q26): the key is
encrypted with a passphrase and decrypted only inside the `summa42` CLI for the
duration of one signed action; the Box user never holds the plaintext. All
Owner signing goes through one signer interface with a named custody mode, so
a hardware key, `ssh-agent` or an SSO-backed signer can be added later without
touching the callers. The spec must say what an SSO login can and cannot prove:
it authenticates a person; the Collective still verifies a signature against
the stored Owner public key.
Done when: a test running as the Box user cannot produce a valid Owner
signature without the passphrase, and the custody mode is shown by `summa42
status`.

**M4-T5 `DESIGN`: decisions from a phone.** Depends: M4-T2, M4-T4. No build
card until the Owner approved the spec. The inbox and its decisions through Discord instead of a terminal. A
Discord account is an identifier, not a signature (README), so the spec must
settle where the signing key lives. Recommended shape: a second, registered
device key with a narrower ceiling than the Owner key (it may approve
operations up to MEDIUM risk and decide inbox packets; it may not grant
delegations or mandates, fund envelopes, change policy or lift break glass),
revocable by the Owner key at any time, with every use recorded. The spec must
fix: the private key lives on the phone and never on the host or in the bridge;
an exact allowlist of packet kinds the device key may decide (not "inbox
packets" in general, which include proposals and funding); that it cannot
record a heartbeat or end safe mode (Article 29: only the Owner's return does);
that its approvals do not count toward rule D1; the policy change this needs
(the default policy names only the Owner principal), which is a protected path.
Tests: `TestDeviceKeyCannotGrantDelegation`, `TestRevokedDeviceKeyRefused`,
`TestUnsignedDiscordMessageDecidesNothing`.

**M4-T6 `DESIGN` then `BUILD`: handing over work in one sentence.** Depends:
M2-T7, M4-T5 for the Discord path. `summa42 ask "<request>" --project <slug>` and the same from
Discord. The request becomes a draft case; a model may ask bounded clarifying
questions and proposes a plan with scope, budget and what it will not do;
nothing starts until the Owner confirms with a signature. The request itself
must be signed before any model sees it (the CLI signs with the Owner key; a
Discord request needs the device key): an unsigned message spends nothing and
reaches no model. From there the case
runs like any other, including an hours-long agentic Attempt (M1A).
Tests: `TestRequestCreatesDraftNotWork`, `TestUnsignedRequestReachesNoModel`, `TestWorkStartsOnlyAfterSignedConfirmation`,
`TestDraftShowsBudgetAndScope`.

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
per-operation approval only when a valid delegation covers it. Policy keeps
naming the Owner principal for everything else. Each use is
recorded and appears in the Owner's digest. Any rejection, failed verification
or reverted change in the class suspends the delegation automatically. The spec
must list what can never be delegated (policy, keys, approvers, budgets,
workflow files, the delegation mechanism itself). Entry condition: M1-T5, M4-T4,
M9-T6 and M9-T8 done. The spec must state that no delegation or mandate can be
created, widened or reactivated by anything but an Owner signature, and that
the undelegable list includes the mandate mechanism.

**M5-T2 `BUILD` `agent`: CI observation bound to the exact SHA.**
Read-only. Required checks come from the Project manifest. A missing required
check is pending, not success. A CI infrastructure failure is distinguished
from a test failure; only the second starts a repair cycle (M1-T1).
Tests, against a fake GitHub server: the four CI observer tests of the
reverted plan (`2026-10-16-ci-observation-and-protected-merge.md`, Task 1:
success, pending, failure, missing required check), plus
`TestStaleCheckOnOldHeadIgnored` and `TestInfrastructureFailureDoesNotStartRepair`.
The three review observer tests of that plan belong to M5-T4.

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
issue is closed only after the merge commit is confirmed. If branch protection
requires an approval from an identity other than the pull request's author, use
an Owner-approved review identity or stay at A1; never weaken protection
(roadmap stage 6).
Tests: the five merge-gate tests of roadmap stage 6, plus
`TestMergeWithoutDelegationWaitsForOwner`.
Done when (A2): three small changes in the Summa42 Project go from issue to
merged without an Owner action, and one change to a protected path waits.

### M6. Automation loop

**M6-T1 `DESIGN`: automation loop spec.** Answers the nine open questions of
discussion #136 as they apply to adopted automations (section 9). Settles the proposal record and its
states (`CANDIDATE`, `SHADOW`, `PROPOSED`, `ADOPTED`, `SUSPENDED`, `RETIRED`),
the detection rules below with their thresholds, the mandate record, the shadow
comparison, the suspension rules and the digest. It must show, article by
article, why adoption inside a mandate does not violate Vision Articles 6, 7
and 14. Thresholds: Q30 to Q32 in section 2.4. An adopted automation is the
minimal Constructor v1 (Q43): the spec names the record accordingly and records
the decision for discussion #136.

Detection rules for the first version (deterministic, over M4-T1 rows):

| Rule | Pattern | Proposal |
| --- | --- | --- |
| D1 | The Owner approved the same operation class N times in a window with zero rejections | A standing delegation for that class, with the tightest constraints that cover the observed instances |
| D2 | The Owner ran the same manual command on a regular cadence | A schedule that creates that Task |
| D3 | Cases of one class block at the same gate for the same reason, and the Owner unblocks them the same way | A manifest change (grant, limit or allowed path), shown as a diff. Proposal only |
| D4 | An obligation was fulfilled by hand on time K times (needs M7-T3) | A Task template and the funding it would need. Proposal only |

Only D2, and only for work that needs no approval today, can be adopted under
a mandate. D1, D3 and D4 always end as an Owner-signed packet: a delegation, a
manifest change and funding are authority.

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
`PROPOSED` schedule or Task template inside the mandate is adopted by the
Collective, with the mandate as its authority source, and a notification is
sent. Adoption never creates a delegation, a grant, funding or a manifest
change. Everything else becomes an inbox packet; `summa42 automation
approve|reject` is Owner-signed, and for D1 that signature is what creates the
delegation. Any failed verification, rejection or
reverted effect in an adopted automation suspends it without asking.
Re-activation always needs the Owner.
Tests: `TestInsideMandateAdoptsAndNotifies`, `TestOutsideMandateWaitsInInbox`,
`TestAdoptionCannotExceedMandateRiskCeiling`,
`TestAdoptionNeverCreatesDelegation`, `TestManifestChangeIsNeverAdopted`,
`TestFundingIsNeverAdopted`,
`TestFailureSuspendsAdoptedAutomation`, `TestSuspendedNeedsOwnerToResume`,
`TestNoMandateMeansEverythingIsProposed`.

**M6-T5 `BUILD` `pipeline`: digest.** `summa42 digest [--since]` and a weekly
notification: what was taken over, what is proposed, what was suspended and
why, Owner actions per Project against the previous period, money spent.
Done when (M6): on the Summa42 Project, a D1 proposal arises from real Owner
approvals, passes shadow, reaches the inbox and becomes a delegation by the
Owner's signature; and a D2 schedule for LOW-risk work is adopted under the
mandate without an Owner action.

**M6-T7 `DESIGN` then `BUILD`: reflection proposals (epic #15).** After a real
Attempt the Summand may emit a bounded reflection. It is stored as evidence and
becomes a `CANDIDATE` in the same proposal table, visible through
`summa42 automation list` and the inbox; promotion is by Owner approval only,
never under a mandate (M2-T8). Nothing a Summand writes
becomes Collective knowledge directly. Restore from the parked
`internal/epistemic` only what this path calls, with durable state in a
migration.

**M6-T6 `DESIGN`: model-assisted discovery.** Later and optional. A model reads
sanitized summaries of a Project's sources and suggests candidates that the
deterministic rules cannot see. Its output is a `CANDIDATE` only and must pass
the same shadow and mandate path.

### M7. Company OS for the first product

The first product is a Data as a Service offering on Azure with an API and a
web portal (section 2.4). Every system below stays the source of truth for its
domain; Summa42 reads it and changes it only through external operations.

**M7-T1 `DESIGN`: company map.** From the Owner's answers: the functions of
the company (build, release, operate, support, money, obligations), the system
of record for each (GitHub and ADO, Azure Pipelines, Azure monitoring, the
portal database, inFakt), who does it today and how often, and what an error
costs. One table; it orders the rest of M7 by Owner time saved against risk.

**M7-T2 `BUILD` `agent`: connector contract and template.** One connector is: a
read capability with an action and parameter allowlist, an optional
`operations.Provider` for writes with `LookupOutcome`, a board connector, and a
fake server for tests. Prefer the existing MCP adapter where a maintained MCP
server exists for the system (this wires `adapters/mcp` and the capability
Session into execution; see M9-T2).
Done when: `docs/connector-guide.md` and one connector built from it exist.

**M7-T3 `BUILD` `agent`: first real obligation.** Depends: M2-T5, M3-T2; it
does not wait for M6. `summa42 obligation
add|list|fund`; the obligation planner (constructed in `internal/runtime/box.go`)
gets called from `serve`. The first obligation is the monthly dependency update
(Q8), in the `summa42` Project first and then per repository of `daas`; it ends
in an ordinary draft pull request and is funded by the Owner.

**M7-T4 `DESIGN` then `BUILD`: operate.** Measure first (Q3): per dataset freshness and
completeness, and availability and error rate of the API and the portal, read
from Azure monitoring and from the product's own data. No thresholds are
invented; a measurement becomes an invariant only when the Owner sets its
threshold in the Project manifest, and from then a breach raises a case in the
`daas` Project. Remediation runs only inside granted authority. Wires
`internal/invariant` and `internal/company`. Needs from the Owner: read access
to the monitoring source and the list of datasets.

**M7-T5 `DESIGN` then `BUILD`: support.** Intake reads the portal's request store (the
contact form writes to the product database; a future API endpoint for data
requests writes to the same store) and the support mailbox, read-only, into the
board. Triage; a drafted reply is delivered to the Owner's inbox and the Owner
sends it. Summa42 has no send operation at the start (section 2.4); these
drafts do not count toward rule D1. Prefer
a read endpoint of the product's own API over direct database access. Customer
text is `customer-data`: zero-data-retention or local models only, never in a
sandbox.

**M7-T6 `DESIGN` then `BUILD`: money.** Invoicing already runs inside the product through
inFakt behind a provider abstraction; Summa42 does not issue invoices. It
observes: issued and unpaid invoices, failed automated runs, usage against
plan, and raises a case on an anomaly. The connector follows the same
provider-neutral shape as the product's abstraction. There is no write
operation at the start; when one is added it follows the rule of section 2.4.

**M7-T7 `DESIGN` then `BUILD`: structured Mission and goal proposals (epic
#12).** The structured Mission is the row `summa42 mission` creates and shows,
per Project, not a second model. Goal proposals derived from it reach the
Owner as inbox packets. Restore from the parked `internal/mission` only what
this path calls; its oracle (fixed phrases, constant confidence) does not
return.

### M8. Release and self-upgrade (A3)

Roadmap stage 7, unchanged: a separate `cmd/summa42-supervisor`, a release
manifest bound to the merged SHA, backup before upgrade (M3-T4), canary with a
synthetic case, rollback only onto a compatible database, expand-contract
migrations.

**M8-T1 `DESIGN`: supervisor and upgrade spec.** Roadmap stage 7 turned into
build cards with tests. The first product's own deployment is a
separate concern: it runs through Azure Pipelines (Q5). Summa42 observes the
runs (M2A-T4) and later triggers a release as an external operation of at
least MEDIUM risk behind approval; it does not replace the pipeline.

**M8-T2 `DESIGN` then `BUILD`: scenario runs (epic #11).** `summa42 scenario
run <name>` drives a scripted case through the real execution and operation
services against the fake servers the connectors ship with (M7-T2), including
lost acknowledgements and restarts. The canary of the supervisor uses one such
scenario. Nothing is asserted on flags a test double reports about itself.

### M9. Containment hardening

**M9-T1 `DESIGN` then `BUILD`: validation in OCI.** Run validation commands through
the existing TEB Docker provider instead of the `summa42-validate` user alone,
when Docker is present; record the enforcement level actually achieved.

**M9-T2 `DESIGN` then `BUILD`: capability Sessions in execution.** Wire the MCP
adapter and capability Session so a Task's `RequiredCapabilities` become the
only tools its executor can reach.

**M9-T3 `DESIGN` then `BUILD`: context projection.** Call the compiler in
`internal/runmanifest/compiler.go` from the planner and implementer paths and
record the projection hash in the run manifest.

**M9-T4 `BUILD` `agent`: remaining rogue sensors (epic #5).**
`ReportSandboxBoundaryViolation` and `ReportAuditTampering` in
`internal/detection/detector.go` have no caller. Call the first from the
workspace and validation path when an edit or a command leaves its allowed
paths, and the second from a periodic audit-chain check in `serve`. Each needs
an acceptance test over the running path. With M3-T3 and M4-T3 this meets the
closing criterion of #5.

**M9-T6 `DESIGN` then `BUILD`: producer and reviewer are different OS users.**
Section 2.5. Spec written:
[reviewer isolation](../specs/2026-11-11-reviewer-isolation-design.md). It is
binding once the Owner approved it and splits the work into five cards, M9-T6a
to M9-T6e, each one pull request on protected paths (`internal/modelrun/`,
`scripts/setup-model-user.sh`), reviewed by the Owner line by line. In short:
the flag `--review-model-user` already exists and must not be added again; it
gets its own default (`SUMMA42_REVIEW_MODEL_USER`) and must differ from the
producer and validation users; every model call records the OS user it ran as,
and a review refuses when the work it reviews was produced as its own user;
the role homes are mutually unreadable; nothing a call started survives it;
the CLI settings a model could change for the next call are checked before
each call.

**M9-T7 `DESIGN` then `BUILD`: model egress allowlist.** Section 2.5. A model
process may reach only the endpoints of its own provider. Use the existing
CONNECT egress proxy (exact allowlisted hosts, validated IPs) and a per-user
firewall rule on the host so that a model user has no route except the proxy.
Each profile declares its hosts; a denied connection is a rogue signal on the
Attempt. The allowlist must not cost capability (section 2.6): package
registries are reachable through a cache or mirror path, and web research goes
through a mediated fetch tool whose requests are logged as evidence. Record the enforcement level actually achieved; where the host cannot
enforce it (for example WSL), say `UNENFORCED` and do not claim more.

**M9-T8 `BUILD` `agent`: output hygiene between roles.** Section 2.5. Before
any model output is projected into another role's prompt or published: reject
invisible and bidirectional control characters and non-text content; pass
producer text to the reviewer only inside a delimited data block that the
prompt declares to be untrusted; flag text that addresses the reviewer or the
Owner with instructions (a deterministic pattern list, kept as data) as a
finding the reviewer must answer and as a rogue signal. This is a tripwire, not
a proof; say so in the code.
Tests: `TestBidiAndZeroWidthRejected`, `TestReviewerDirectedInstructionFlagged`,
`TestProducerTextIsDelimitedAsData`, `TestCleanOutputPasses`.

M9-T6 and M9-T8 are entry conditions of M5: a standing delegation leans on the
reviewer, so the reviewer's independence has to be real first.

**M9-T5 `DESIGN` then `BUILD`: one real micro-decision (epic #16).** Route one
existing decision through a restored `internal/decision` with a working
provider and provenance in a migrated table. Candidate: classifying a CI
failure as infrastructure or code (M5-T2), rule provider first. Only if the
engine makes that decision cheaper or more reliable than the current code;
otherwise the epic stays open.

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
5. Implement the minimum that makes them pass. Follow rules R1 to R12.
6. Run make verify. Paste the real output tail. Do not claim a pass you did
   not see.
7. Update the README "What runs today" row if the card made something run.
8. Open a draft pull request titled "<ID>: <card title>". In the description
   list: what changed, what you could not do, every deviation from the card.
   End it with "Closes #<card issue>" and, if section 9 names an epic for
   this card, "Part of #<epic>". Never write "Closes" for an epic.
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
| Governance makes the agents weaker than their bare harness | M1 to M9 | Section 2.6, milestone M1A, parity benchmark M1A-T6 repeated on every change |
| Agents coordinate outside the Collective's sight | every milestone with two model roles | Section 2.5, R12, M9-T6 to M9-T8 before any delegation |
| Delegation or mandate widens authority | M5, M6 | Spec lists the undelegable; suspension is automatic; M4-T4 first |
| The Owner key is readable by the Box user | M5 onward | M4-T4 is an entry condition of M5 |
| Projects migration breaks the one running installation | M2-T2 | Upgrade test on a copy of the live database; backup first |
| Company connectors built on guesses | M7 | Section 2.4 records the systems; a card stops (R10) when access or a fact it needs is missing |
| Customer data reaches a model without zero data retention, or private code leaves through a sandbox | M1A, M2A, M7 | Data class per repository and source in the manifest; refused before the call; no fetch outside `public`; customer data never mounted |
| Plan grows stale | all | A card that no longer matches the code is a stop condition (R10), not a licence to improvise |

## 8. What is deliberately not in this plan

Multi-Cube coordination and fleets, PostgreSQL, NATS, Dapr, inter-Collective
delegation, a full digital twin, and a web panel (the Owner surface is the
CLI and Discord). None is needed for one Owner, one product
and several Projects on one host. The issues and discussions that carry these
ideas stay open (section 9); each returns when a card needs it.

## 9. Issues and discussions

State on 11 November 2026, before this plan's own issues (#178 onward) and
pull request: ten open issues, one open pull request (#176, since closed), three
discussions. Each epic was reopened on 10 November with a closing criterion in
its last comment; the table quotes that criterion in short and names the cards
that meet it. A card that touches an epic links it (R11).

| Issue | Closing criterion (short) | Cards | After this plan |
| --- | --- | --- | --- |
| #124 Intake and board | Intake creates and updates cards; a command shows Held and Blocked per mission; acceptance test | M2-T6 | Closed by M2-T6 |
| #5 Rogue detection, quarantine, containment | Every control is called from a running Box or an Owner command, with an acceptance test | M4-T3, M3-T3, M9-T4; M9-T6 to M9-T8 add the signals of section 2.5 | Closable after M9-T4 |
| #18 Company OS profile | One real, Owner-funded obligation scheduled ahead of discretionary work; one command lists what waits for the Owner | M7-T3, M4-T2 | Closable after M7-T3 |
| #15 Learning, dreaming, process evolution | Reflection after a real Attempt yields a durable proposal; promotion through an Owner-visible gate | M6-T7 (on the M6 proposal path) | Closable after M6-T7; dreaming stays out |
| #12 Mission and goal research | The structured Mission is the one `summa42 mission` creates; goal proposals reach the Owner through a command | M2-T2 (Mission per Project), M7-T7 | Closable after M7-T7 |
| #16 Decision engine | One real micro-decision routed through the engine with a working provider and migrated provenance | M9-T5 | Open unless M9-T5 proves a benefit |
| #11 Digital twin | A scenario run started by a command passes through the real execution and operation services | M8-T2 | Closable after M8-T2 |
| #17 A2A and inter-Collective contracts | A second Collective exchanges a delegated Task and evidence through the gateway | none | Stays open; outside this plan (section 8) |
| #3 Semantic control plane over OSS runtimes | none recorded | M9-T1, M9-T2 use existing providers (Docker TEB, MCP) | Stays open as the architecture question |
| #60 Dapr seam and AgentLedger conformance; pull request #176 | Spike of #3 | none | #176 closed unmerged (Q38); the five conformance patterns are already kernel tests or belong in M8-T2 |

Epics #5, #11, #12, #15, #16, #17 and #18 were closed once on code nothing
called. They are closed again only by the Owner, against the criterion, after
the acceptance test named in the card exists.

| Discussion | State | Position of this plan |
| --- | --- | --- |
| #136 Constructors (pinned) | Exploration; nine open questions, no decision | Decided (Q43): an adopted automation (M6) is the minimal Constructor v1: a persistent, Owner-anchored responsibility with a ceiling it inherits, dormant until woken, with "no action required" as a valid result. The M6-T1 spec answers the nine questions as they apply; the Owner records the decision in the discussion. The note `docs/architecture/constructors-collective-architecture.md` describes Cubes as stateless nodes of a fleet; that contradicts the single-Cube MVC and is direction only |
| #137 Resurrector and island recovery | Proposed | M3-T5 is its single-host form: an external watchdog, restart by systemd, notification when the database is unreachable. The signed resurrection contract and cloud drivers wait for multi-Cube |
| #138 Dynamic fleet consensus and demobilization | Proposed | Fleets are outside this plan (multi-Cube). Its useful core, a plan fanning out into child Tasks that run in parallel, is M1A-T5 on one host |

New design questions go to a discussion first (R11, Q45); a decision reached
there is recorded in the spec of the `DESIGN` card that uses it, and the spec
links the discussion.
