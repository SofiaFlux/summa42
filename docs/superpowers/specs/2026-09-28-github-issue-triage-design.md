# GitHub issue triage — design

Date: 2026-09-28
Status: revised after code review, awaiting approval
Supersedes nothing. Follows `2026-09-24-github-issue-intake-design.md`.

## Intent

Issue intake (`run-gh-intake`) only registers: for every open issue authored by an
allowlisted maintainer it creates a workflow case and one TEB Task, left `ELIGIBLE` and
never leased. This design turns those Tasks into a recorded triage decision and nothing
more. It introduces no code writing, no planning, no pull request, and no GitHub write of
any kind.

The pipeline has four stages. Three of them decide, one only reports.

```
stage 1  deterministic rules      -> classification and signals
stage 2  model                    -> signals for whatever stage 1 could not settle
stage 3  deterministic rules      -> final disposition
stage 4  reviewer                 -> structural and plausibility checks, advisory only
```

The governing rule of stages 1 to 3 is **the model proposes, deterministic code
disposes**. The model never returns a verdict. It returns a fixed, schema-validated JSON
document of signals, and stage 3 computes the disposition from that document with a
complete, versioned rule table. The same model output therefore always yields the same
disposition, and a stored decision can be recomputed and audited without re-running the
model.

## Scope

In scope:

- the three-stage decision pipeline, recorded as a durable evidence document
- the executor that consumes an already-registered `github.issue.triage` Task
- the case transition and Task acceptance performed by a driver after the lease completes
- supersession of an older revision, including its pending Task, when a newer revision of
  the same issue is triaged
- the classifier and reviewer model interfaces, their production adapter, and their fake
- capability advertisement and executor routing so the registered triage Tasks are leased
  by the triage executor
- the stage 4 reviewer, as a second and last slice

Out of scope, explicitly:

- any GitHub write: no comment, no label, no draft pull request
- planning, decomposition, or code writing
- closing a case whose source is `github`
- automatic retriage triggered by a mid-execution observation
- any interface for a human to label or override a decision
- the reviewer gating, blocking, or changing anything
- any new column on `workflow_cases`

## What intake already provides

These exist and are not rebuilt here:

- one case and one `ELIGIBLE` Task per issue, created atomically by `EnsureAndMaterialize`
  (`internal/workflowcase/ensure_materialize.go:16-49`)
- case identity `(mission_id, source, object_id, revision_id)`, where the revision is the
  issue's `updated_at` normalised to UTC RFC3339Nano (`internal/state/sqlite/migrations/00015_workflow_verifications.sql:23`,
  `internal/ghissue/source.go:61-63`)
- the Task class `github.issue.triage`, requiring capability `github.issue.read`, with
  acceptance criterion `triage decision recorded for <revision>`
  (`internal/ghissue/observe.go:205`, `:279`)
- the deterministic classifier `ClassifyTriage` returning exactly `bug`, `feature` or
  `unclassified` (`internal/ghissue/source.go:166-177`), already carried in the Task
  payload as `triage`
- the canonical issue snapshot as evidence, referenced by the Task payload rather than the
  issue body itself (`internal/ghissue/observe.go:268-272`)

Because intake already creates one Task per issue, triage is one Task per issue and there
is no parent task and no fan-out code. The scheduler is the distributor: `Next` returns
the first of the eligible candidates (`internal/scheduler/service.go:61-106`) and the
worker loop leases them one at a time (`internal/scheduler/worker.go:76-91`). A parent
task would have to reimplement work selection, ranking, budget isolation and dependency
ordering, and would lose per-revision idempotency and per-issue retry granularity.

## Architecture

Four processes, extending the existing ADO shape of observer, driver and verifier with
`run-worker`.

| Process | Role |
| --- | --- |
| `run-gh-intake` | unchanged; registers cases and Tasks, still registers no executor |
| `run-worker` | leases the registered triage Task and executes it |
| `run-gh-triage-driver` | accepts a completed triage Task, applies the case transition, blocks failed triage and supersedes older revisions with their pending Tasks |
| `run-gh-triage-review` | reads recorded decisions, runs stage 4, writes a verdict |

### Capability advertisement

`workerCapacity` builds its advertised set keyed **only** by registered executor kind
(`cmd/summa42-box/main.go:520-526`), plus one hardcoded special case that lets
`ado-publish` additionally advertise `ado.pr.comment` and `ado.pr.approve` (`:527-531`).
`capacityEligible` requires every `RequiredCapabilities` entry to be a key in that map
(`internal/scheduler/service.go:240-248`). A Task requiring `github.issue.read` is
therefore never eligible unless something advertises exactly that string.

So registration alone is not enough. The executor kind is `github-issue-triage`, and
`workerCapacity` gains a second explicit special case, mirroring the `ado-publish` one,
that additionally advertises `github.issue.read` when `github-issue-triage` is registered.
This is a named change to `workerCapacity`, not an inference from registration.

### Executor routing

`Worker.StepOnce` selects the executor through `ChooseExecutor`
(`internal/scheduler/worker.go:83`), which never inspects `TaskClass`: without a
preference it returns the alphabetically first registered kind
(`internal/scheduler/service.go:112-139`). `run-worker` sets no `ExecutorPreference`
(`cmd/summa42-box/main.go:585-595`), so the experience evaluator answers against its own
fixed vocabulary of task classes, which does not contain `github.issue.triage`
(`internal/domain/feedback.go:8-15`), the lookup misses, and the alphabetical fallback
applies. A real `run-worker` already registers `copilot` and `ado-publish`, either of
which would otherwise receive the triage Task.

This slice therefore pulls in the routing seam that the publish executor design deferred: a
`map[domain.TaskClass]ExecutorKind` on the scheduler config, consulted by
`ChooseExecutor` before the preference and baseline fallbacks. It is a single lookup with a
two-line fallthrough, and it is required for correctness here rather than for convenience.
The broader general routing table stays deferred.

### Why the transition is not in the executor

The executor runs while a lease is held. Transitioning the case there would change case
state before the successful attempt was recorded, which is the partial-state shape
`EnsureAndMaterialize` was built to prevent. The driver therefore runs after completion and
observes the ordering guarantee directly.

## Decision record

One evidence document, kind `github.issue.triage.decision`, following the
`ado.review.decision` precedent (`internal/adoreview/verify.go:231`). Every field records
its origin, because the whole point of the record is that a decision can be explained,
recomputed and later re-scored.

```json
{
  "schema": "github.issue.triage.decision.v1",
  "repository": "owner/name",
  "issue": 42,
  "revision": "2026-09-28T10:00:00Z",
  "snapshot_evidence_id": "evidence-...",
  "triage_rules_version": "ghtriage.rules.v1",
  "disposition_rules_version": "ghtriage.dispositions.v1",
  "stage1": {
    "triage": "bug",
    "signals": ["has-repro", "label:bug", "title:[bug]"]
  },
  "stage2": {
    "is_actionable": true,
    "needs_repro": false,
    "scope": "medium",
    "suggested_type": "bug",
    "rationale": "The report includes enough detail to plan a bounded fix."
  },
  "stage3": {
    "disposition": "ready-to-plan",
    "rule": "actionable-without-repro-small-or-medium"
  }
}
```

`stage1` is always present. Its `disposition` and `rule` are present only when stage 1
resolved the issue on its own; `stage2` and `stage3` are then absent. For example, an
explicit `duplicate-of:41` label produces `stage1` with `triage: "duplicate"`,
`disposition: "duplicate"` and `rule: "explicit-duplicate-label"`, and no later stages.
When stage 1 did not resolve, its `disposition` and `rule` are absent and `stage3` is the
authoritative outcome, as in the example above. The absence of `stage2` and `stage3` records
that the model was not consulted. A recorded decision can be recomputed from its stored
inputs without re-running the model.

Both rules versions are recorded because the stage 1 and stage 3 rule tables are expected
to be revised in later slices. A versionless record could not be re-scored after such a
revision: every historical decision would recompute to a different rule name and appear
violating. The reviewer checks `triage_rules_version` for stage 1 decisions and
`disposition_rules_version` for stage 3 decisions, and reports `not-applicable` when it does
not implement the relevant version.

`stage1.triage` widens `ClassifyTriage`, which returns only `bug`, `feature` and
`unclassified` (`internal/ghissue/source.go:166-177`). Start with that exact classifier.
Override it with `duplicate` only for an explicit valid duplicate label; widen an
`unclassified` result to `question` only for a `question` label or `[question]` title prefix.
Otherwise keep the intake classification. Stage 1 re-derives this from the snapshot; the
Task payload's `triage` value is retained for comparison. The reviewer recomputes the exact
widening rule from the snapshot rather than accepting any difference merely because the new
value is `question` or `duplicate`.

## Stages

**Stage 1, deterministic.** A pure function from the canonical snapshot to signals and a
classification. Signals are: the sorted label set, a title prefix, the presence of a
fenced reproduction block, an explicit `duplicate-of:<n>` label, and `#N` references in the
body. A duplicate label is decisive only when exactly one `duplicate-of:<n>` label is
present and `<n>` is a positive issue number other than the current issue number. Multiple
or malformed duplicate labels leave stage 1 unresolved. A valid label is an explicit
maintainer assertion about the same repository; stage 1 does not claim to verify the
target's current open state. A bare `#N` is context,
not proof of duplication. The canonical snapshot contains no other issue's state, so
neither target lookup nor live GitHub state is part of this pure function.

Stage 1 is deliberately conservative and resolves exactly one case decisively:

| Order | Condition | Disposition | Rule |
| --- | --- | --- | --- |
| 1 | a valid `duplicate-of:<n>` label | `duplicate` | `explicit-duplicate-label` |
| 2 | otherwise | unresolved; hand to stage 2 | — |

Keeping the decisive set this narrow is deliberate. Intake already excludes issues that
carry `box-hold`/`wontfix`, an assignee, or a non-maintainer author
(`internal/ghissue/source.go:195-200`), so those never become cases and stage 1 must not
pretend to re-derive them. Every other issue is genuinely ambiguous between "needs a
reproduction", "not actionable" and "ready to plan", and those are exactly the judgements
worth spending a model call on.

**Stage 2, model.** Receives a fixed, deterministically prepared input document and nothing
else. It never receives the raw issue. The input is exactly:

```json
{
  "schema": "github.issue.triage.stage2.input.v1",
  "title": "Crash on save with an empty workspace",
  "labels": ["bug"],
  "signals": ["label:bug"],
  "body_excerpt": "…",
  "question": "<the literal question below>"
}
```

- `title` is the full title, truncated to 200 runes.
- `labels` is the sorted label set, untruncated.
- `signals` is the sorted stage 1 signal list, untruncated.
- `body_excerpt` is the issue body with any leading fenced reproduction block removed, since
  stage 1 has already extracted `has-repro` from it, truncated to 4000 runes with
  `[…truncated]` appended when the remainder was dropped.
- `question` is a single fixed literal, so a test can assert on the exact prompt:

  > Classify this issue. State whether it is actionable without further information from
  > the reporter, whether it needs a reproduction before it can be planned, how large the
  > change would be, and what type of work it is. Answer with JSON only.

There is one literal because stage 1 has exactly one unresolved class. The prompt is a
constant, which is what makes stage 2 testable with a fake model that records its input.

The response is validated against a fixed schema. A non-conforming response is rejected
rather than repaired or interpreted. Free text never influences the disposition.

**Stage 3, deterministic.** A pure, total function from the validated `stage2` document to a
disposition and the rule that produced it. No model call appears in this function. The
disposition vocabulary is closed, and this table is exhaustive:

| Order | Condition on `stage2` | Disposition | Rule |
| --- | --- | --- | --- |
| 1 | `is_actionable` is false | `not-actionable` | `not-actionable` |
| 2 | `needs_repro` is true | `needs-human` | `needs-repro-required` |
| 3 | `scope` is `small` or `medium` | `ready-to-plan` | `actionable-without-repro-small-or-medium` |
| 4 | `scope` is `large` | `needs-human` | `scope-large-needs-decomposition` |
| 5 | `scope` is `unknown` | `needs-human` | `scope-unknown-needs-scoping` |

`duplicate` is deliberately absent from this table. An explicit duplicate label is a
stage 1 signal and resolves the issue decisively; an ordinary `#N` reference does not.
Stage 3 can therefore never produce `duplicate` and the
disposition is reachable from exactly one place. The closed set is
`ready-to-plan`, `not-actionable`, `duplicate`, `needs-human`, and every one of the four is
reachable.

## Model interface

The repository has no model client and no provider package. The only model-facing
executors shell out to the `codex` and `copilot` CLIs (`internal/executors/codex.go`,
`internal/executors/copilot.go`). This slice follows that precedent rather than inventing a
provider abstraction.

```go
// internal/ghtriage
type ClassifierModel interface {
    Classify(ctx context.Context, input Stage2Input) (Stage2Output, error)
}

type ReviewerModel interface {
    Review(ctx context.Context, input ReviewInput) (bool, error)
}
```

`internal/ghtriage` owns both interfaces, their input and output types, and schema
validation. `ReviewInput` contains the fixed reviewer question, the issue title and body
from the pinned snapshot, and the recorded decision; the response schema contains only
`plausible: bool`. The production adapter is `internal/ghtriage/climodel`, which execs the
configured CLI and requires exactly one conforming JSON object on stdout, with no second
object or trailing prose. The fake is `internal/ghtriage/fakemodel`, which returns scripted
outputs and records both kinds of input.

The CLI binary name and its credentials are configuration resolved at startup, validated
in the constructor, as the GitHub token file already is. A missing or unusable model
configuration is therefore a startup error, not a per-tick error.

## Case state transitions

There is **no new column on `workflow_cases`**. A `state_reason` column was considered and
rejected on review: the reason already exists, already persists, and is already audited.

`Assess` with `Verdict: Unknown` returns `Decision{Outcome: OutcomeBlocked, Reason: <reason>}`
(`internal/workflow/decision.go:80-81`), which `Assess` applies as `ACTIVE` → `BLOCKED`
(`internal/workflowcase/assessment.go:94-97`), and the full `AssessmentResult` including
the reason is written to `workflow_assessments.result_json` (`:105-117`). Idempotency comes
free: `Assess` keys its replay on `(case_id, work_id)` and returns the stored result for a
byte-identical request (`:41-48`).

A column would have added nothing, and a `CHECK` tying `BLOCKED` to a non-empty reason would
have broken two shipped paths that block with no reason: `Assess` on budget or progress
exhaustion (`internal/workflow/decision.go:95-103`) and `Reject`
(`internal/workflowcase/service.go:414-419`).

| Disposition | Case state | `Assessment.Reason` |
| --- | --- | --- |
| `ready-to-plan` | stays `ACTIVE` | — no assessment performed |
| `not-actionable` | `BLOCKED` | `not-actionable` |
| `duplicate` | `BLOCKED` | `duplicate` |
| `needs-human` | `BLOCKED` | `needs-human` |
| triage Task exhausted its retries | `BLOCKED` | `triage-failed` |
| superseded revision | `BLOCKED` | `superseded-by:<revision>` |

`ACTIVE` after triage is deliberate. A GitHub case cannot be closed today: `Close` requires
`READY_FOR_VERIFICATION` (`internal/workflowcase/service.go:261-263`), and the final
verifier skips every case whose source is not `ado`
(`internal/adoreview/verify.go:120-122`). `BLOCKED` is the terminal state for triage in
this design. After a successful `ready-to-plan` decision, `ACTIVE` means "triaged and
waiting for the planning subproject"; before that, intake also leaves an untriaged case
`ACTIVE`.

Leaving a `not-actionable` case in `ACTIVE` would be a hazard. `ListActive` also includes
cases that intake registered but triage has not resolved yet. The future planning
subproject must therefore require all three conditions: an `ACTIVE` case, a `SUCCEEDED`
triage Task backed by an accepted `ready-to-plan` decision, and no newer registered
revision of the same `(mission_id, source, object_id)`. `ListActive` alone is insufficient,
including during the gap between intake and a driver tick.

The driver also closes the Task. After a lease completes, `CompleteAttempt` moves the Task
to `AWAITING_VERIFICATION` (`internal/verification/service.go:103`) and only `AcceptTask`
moves it to `SUCCEEDED` (`:188`). A Task transition is not a case transition and is not
blocked by the `UNKNOWN →` case-state restriction above, so the driver accepts the triage
Task with `AcceptTask`, exactly as `FinalVerifier.finalizeTask` does
(`internal/adoreview/verify.go:447-467`), citing the decision evidence as completion. The
driver discovers the Task by the same ADO pattern: look it up by the case's
`current_work_id` and wait for `TaskAwaitingVerification`
(`internal/adoreview/driver.go:222`).
It requires exactly one `github.issue.triage.decision` in the completed attempt's output
evidence and checks its repository, issue, revision and snapshot against the case before
accepting it. Evidence not attached to a completed attempt is not a decision. After
validation, the driver calls `AcceptTask` **before** `Assess` for a blocking disposition.
If it crashes between those calls, the Task remains `SUCCEEDED`, the case remains `ACTIVE`
with its original work ID, and the next tick can finish the case transition. The driver
must handle both `AWAITING_VERIFICATION` and `SUCCEEDED` while processing an `ACTIVE` case.

## Supersession

`workflow_cases` is unique on `(mission_id, source, object_id, revision_id)` and the
revision is the issue's `updated_at`. A human relabelling or commenting on an issue in
GitHub therefore changes the revision, and intake registers a new case and a new triage
Task for the same issue at that revision. This is the free retriage path: the human
changes the facts at the source and triage re-runs with the new signals, while the older
case remains as a record of the earlier reading.

The cost is that two `ACTIVE` cases and two triage Tasks can exist for one issue. After the
driver accepts the newest revision's valid decision, it compares revision timestamps as
parsed times, not lexicographic strings, and visits older cases with the same mission and
object ID. The newest case's state follows its disposition; it is `ACTIVE` only for
`ready-to-plan`. An older case already `BLOCKED` needs no further transition. The
supersession scan includes accepted decisions on `BLOCKED` newer cases: after an assessment
clears `current_work_id`, its `workflow_assessments.work_id` still identifies the accepted
Task. Scanning only `ListActive` would lose this work after a driver restart, so the driver
needs a read path across GitHub cases and their assessment history.

For each older `ACTIVE` case, the driver handles the linked Task before clearing the
case's `current_work_id`:

| Older Task state | Driver action |
| --- | --- |
| `ELIGIBLE` or `EXECUTING` | Challenge it as superseded with a state-guarded operation that revokes an active lease and changes the Task to `CHALLENGED` atomically. Then block the case. |
| `AWAITING_VERIFICATION` | Validate and accept its completed decision first; apply its ordinary case disposition, then block the case if it is still `ACTIVE`. |
| `SUCCEEDED` | Block the still-`ACTIVE` case. |
| `BLOCKED`, `CHALLENGED`, `CANCELLED` or `EXPIRED` | Block the still-`ACTIVE` case; do not change the Task again. |

The existing `ChallengeTask` changes any Task state without a guard
(`internal/execution/service.go:396-440`), so this slice needs a conditional variant for
`ELIGIBLE`/`EXECUTING`; it must never turn an accepted Task back into `CHALLENGED`. The
challenge runs before `Assess`, so a crash between them leaves an inert Task and an
`ACTIVE` case that the next driver tick can finish. If the Task changed state before the
conditional challenge, the driver reloads it and follows the new row instead. The block
uses `Assess` with `Verdict: Unknown`, reason `superseded-by:<revision>`, and the newer
accepted decision as evidence. An already assessed, byte-identical request replays safely.
The planning guard above prevents a transient older `ACTIVE` case from becoming new work.

Automatic retriage from a mid-execution observation — a subagent discovering during bug
work that the issue is really a feature — is not built and not planned. Such a case may
already hold a lease, and rewinding a case that may have produced effects contradicts this
project's authority model.

The observation itself is not thrown away. The executor and the driver can attach evidence
of kind `github.issue.triage.observation`, with payload `{decision_evidence_id, observed,
corrected_type, note}`, against the existing decision. It is a separate, additive document:
it never mutates the original decision, and the decision record carries no field for it,
because a later process improvement needs the observation to sit beside the decision rather
than overwrite it. Past decisions are re-scored offline once the stage 1 or stage 3 rules
are revised. That path is viable only because every decision records its provenance, which
is why the record keeps the stage 1 signals even though they have no further use at triage
time.

## Stage 4 reviewer

A separate loop, because the reviewer reads a decision that has already been recorded.
Running it inside the same lease would put a second model call in the critical path and
would fold a "not confident" signal into the acceptance criterion "triage decision
recorded for <revision>", which the record must keep meaning.

The reviewer resolves the body through the chain case, task, payload, snapshot evidence,
body: the intake Task payload carries a snapshot evidence id, not the issue body
(`internal/ghissue/observe.go:271` → `internal/evidence/store.go:167`).

**Structural checks**, which are invariants rather than judgements:

- `stage2_only_if_unresolved`: stage 2 is present if and only if stage 1 did not resolve
- `schema_conformant`: the record conforms to schema `v1`
- `rule_matches_recomputation`: on a stage 1 decision, recompute from the snapshot; on a
  stage 3 decision, recompute from the stored validated stage 2 document. Compare both
  disposition and rule name. The result is `match`, `mismatch`, or `not-applicable` when
  the relevant recorded rule version is newer than the reviewer implements.
- `state_matches_disposition`: the case state matches the disposition and reason, treating a
  `superseded-by:` reason as an allowed terminal state so a correctly superseded case is not
  reported as a violation. A case blocked as `triage-failed` has no decision to review and
  is reported by the driver instead.
- `classification_agrees_with_intake`: recompute `ClassifyTriage` and the exact stage 1
  widening rule from the snapshot, then compare both with the recorded stage 1 value and
  the Task payload's intake value

**Plausibility check**: one model call asking whether the recorded classification follows
from the issue text. The result is a single boolean, `plausible`, and nothing else: the
reviewer is asked for a yes or a no and must not return a severity, a score or a remedy.

```json
{
  "schema": "github.issue.triage.review.v1",
  "reviewer_version": "ghtriage.reviewer.v1",
  "decision_evidence_id": "evidence-...",
  "repository": "owner/name",
  "issue": 42,
  "revision": "2026-09-28T10:00:00Z",
  "case_state": "ACTIVE",
  "latest_assessment_id": null,
  "structural": {
    "stage2_only_if_unresolved": true,
    "schema_conformant": true,
    "rule_matches_recomputation": "match",
    "state_matches_disposition": true,
    "classification_agrees_with_intake": true
  },
  "plausible": true
}
```

The reviewer changes nothing, gates nothing and blocks nothing.

Its verdict is written as evidence, kind `github.issue.triage.review`, and also printed to
stderr for operator visibility. Writing it as evidence is what allows the reviewer itself to
be scored later, retroactively, once human labels exist. There are no human labels and no
prior decisions yet, so effectiveness cannot be evaluated now, and the reviewer does not
pretend to: it checks what can be checked without ground truth.

The reviewer must not call the model on every tick for an unchanged decision and case
state. This slice adds `github_issue_triage_reviews`, a narrow SQLite table keyed by
`(decision_evidence_id, reviewer_version, state_fingerprint)` with
`verdict_evidence_id` as its value. The fingerprint is the canonical pair of the case
state and its latest assessment ID, or `none` when there is no assessment. Both evidence
IDs reference existing evidence objects. Before the model call the reviewer checks that
key; after writing a canonical verdict document it inserts the link with a unique
constraint. The verdict records the state and assessment ID it checked.
Two concurrent ticks may both pay for a model call, but only one verdict is linked and
reported. A crash after storing the evidence but before inserting the link may leave an
orphan document; the next tick retries. A later case transition, including supersession,
gets a new fingerprint and a new structural review; a deliberate re-review of an unchanged
case uses a new reviewer version. The verdict includes the decision evidence ID, so the
result remains attributable
without adding columns to `workflow_cases`. `FindByContentHash`
(`internal/evidence/store.go:140`) can reuse byte-identical evidence but cannot be the
review scheduling key because the model's boolean result is unknown before the call.

A failed model call records **no** verdict document or index row. It writes a line to stderr
and returns, so the next tick retries. Recording an "unavailable" verdict would mark the
decision reviewed without performing the plausibility check.

## Error handling

| Condition | Result |
| --- | --- |
| stage 1 | valid snapshots are processed without a network call; missing or corrupt snapshot evidence fails the attempt |
| stage 2 transport or timeout | the attempt fails and the lease does not complete |
| stage 2 schema violation | at most one retry with the same fixed `Stage2Input` in the same attempt; if both responses fail validation, fail the attempt without a decision |
| stage 3 | cannot fail: it consumes a document stage 2 already validated, and the rule table is total |
| model configuration missing or unusable | startup error, not a tick error |
| a Task fails twice with the same signature | `FailAttempt` moves it to `TaskBlocked` (`internal/execution/service.go:378-391`); the driver records failure evidence and blocks the untriaged case through `Assess` with reason `triage-failed` |
| driver sees an inconsistent case | reported, case left untouched |
| reviewer model call fails | no verdict document, stderr line, retried on a later tick |

Retry counts are bounded. An unbounded retry turns one poisoned issue into an infinite budget
burn, which is the worst possible property here. The driver handles a `TaskBlocked` even
though it has no decision evidence: it writes a canonical `github.issue.triage.failure`
document containing the case ID, Task ID, current attempt ID and failure signature, then
uses that evidence for `Assess`. This satisfies `workflow.Decide`'s non-empty evidence
requirement. The failure record is distinct from a valid `needs-human` disposition and is
reused on replay. It also prevents an untriaged `ACTIVE` case from appearing ready for
planning.

## Idempotency

Model invocation is **at least once**. The executor cannot inspect a completed decision in
its own attempt's provenance before returning: `Worker.completeExecution` persists the
returned evidence and calls `CompleteAttempt` only after `Executor.Start` returns
(`internal/scheduler/worker.go:163-181`). Every retry leases a new attempt ID
(`internal/execution/service.go:176-185`). A crash between the model response and attempt
completion may therefore cause another model call. No exact-once model-call guarantee is
claimed, and triage makes no external write whose repetition would duplicate an effect.

The durable boundary is `CompleteAttempt`. The driver reads
`manifests.Provenance(task.CurrentAttemptID).OutputEvidence` only after the Task reaches
`AWAITING_VERIFICATION`, validates the single decision attached to that completed attempt,
then accepts the Task. Evidence written by an abandoned or failed attempt cannot authorize
the case transition. A completed Task is not leased again, so there is one accepted decision
per Task even if earlier attempts produced different model responses. The acceptance record
and decision evidence ID are the replay path for driver restarts; orphan evidence may remain
after a crash before completion and is not treated as an accepted decision.

The driver checks for an existing acceptance before retrying `AcceptTask`, which is a
guarded `AWAITING_VERIFICATION → SUCCEEDED` transition. `Assess` returns the stored result
for a byte-identical request; the driver reuses the same evidence IDs and request fields on
replay. The reviewer skips decisions already linked to a verdict for its version and the
case state it observed.

## Testing

`internal/ghtriage` is tested with no network: `fakemodel` and a real store.

Stage 1 and stage 3 are pure functions and are tested as tables over snapshots. Stage 1
tests distinguish an explicit single duplicate label from a bare `#N`, a self-reference,
and conflicting duplicate labels; the bare reference must never resolve as `duplicate`.
Stage 2 is tested through `fakemodel`, which records the `Stage2Input` it received. The
tests assert on the exact prepared document and on the fixed question literal. The model
adapter is tested against a stubbed CLI emitting malformed, non-JSON and multi-object output.

The driver is tested for transitions, reasons, idempotency of both the `Assess` replay and
`AcceptTask`, Task acceptance, and supersession. The tests cover an old Task in each state
in the supersession table, a lease racing with the conditional challenge, and a restart
between Task acceptance and case assessment. They also verify that two invalid model
responses fail an attempt without fabricating a decision, repeated failures block both
Task and case with failure evidence, and only an accepted `ready-to-plan` decision leaves a
triaged case `ACTIVE`. The future planner must test its latest-revision guard when built.
The executor is tested against a real Task and a real store, following the publish executor
and ADO driver tests; note that intake registers no executor at all (`openGHIntakeBox` sets
`cfg.Executors = nil`), so there is no intake executor test to imitate.

The reviewer is tested against deliberately injected inconsistencies — a stage 3 rule that
does not match a recomputation, a case state that contradicts its disposition, a stage 2
present although stage 1 was decisive, a classification that disagrees with the payload, a
missing schema field, a correctly superseded case, a record with a future rules version — and
for making no second model call or verdict on a later tick while the case state is unchanged.
A later supersession creates a new fingerprint and review. A failed model call leaves no
index row and is retried; concurrent ticks link at most one verdict for the same decision,
reviewer version and state fingerprint.

Capability advertisement and routing are covered by extending
`TestNextClaimsGitHubIssueTriageWithEnforcedReadCapability`
(`internal/scheduler/ghissue_intake_test.go:77`), which must keep passing, with a new test
that the triage Task is routed to `github-issue-triage` and not to an alphabetically earlier
kind.

The full validation matrix from `CONTRIBUTING.md` applies, including the OCI profile.

## Delivery order

The reviewer is the natural cut line and is built last. It depends on the decision record,
the driver and the resolved case state, and it carries no state of its own, so shipping it
in a second slice changes nothing the first slice depends on. In one plan, it is the final
task.

## Deferred

- planning and decomposition of `ready-to-plan` cases
- any GitHub write capability, including a draft pull request
- closing a case whose source is `github`
- human labelling of decisions, and a `needs-human` queue or interface
- evaluating reviewer effectiveness, which requires labels
- automatic retriage from a mid-execution observation, and any writer for the observation
  evidence other than the executor and driver
- the general task-class to executor-kind routing table, of which this slice adds the single
  entry it needs
