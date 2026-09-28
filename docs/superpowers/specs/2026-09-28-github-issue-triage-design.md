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
- supersession of an older revision when a newer revision of the same issue is triaged
- the model interface, its production adapter, and its fake
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
| `run-gh-triage-driver` | applies the case transition and Task acceptance after a triage lease completes; marks superseded revisions |
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
  "triage_rules_version": "ghtriage.rules.v1",
  "disposition_rules_version": "ghtriage.dispositions.v1",
  "stage1": {
    "triage": "bug|feature|unclassified|question|duplicate",
    "signals": ["cross-reference:41", "label:bug", "title:[bug]", "has-repro"],
    "disposition": "duplicate",
    "rule": "cross-reference-duplicate"
  },
  "stage2": {
    "is_actionable": true,
    "needs_repro": false,
    "scope": "small|medium|large|unknown",
    "suggested_type": "bug|feature|question|other",
    "rationale": "..."
  },
  "stage3": {
    "disposition": "ready-to-plan",
    "rule": "actionable-without-repro-small-or-medium"
  }
}
```

`stage1` is always present. Its `disposition` and `rule` are set only when stage 1 resolved
the issue on its own, in which case `stage2` and `stage3` are both absent. When stage 1 did
not resolve, `stage1.disposition` and `stage1.rule` are empty and `stage3` is the
authoritative outcome. The absence of `stage2` and `stage3` is therefore itself the record
that the model was not consulted, and `stage3.rule` names the rule that produced the
disposition, so replaying a decision needs no re-execution.

Both rules versions are recorded because the stage 1 and stage 3 rule tables are expected
to be revised in later slices. A versionless record could not be re-scored after such a
revision: every historical decision would recompute to a different rule name and appear
violating. The reviewer scopes its recomputation check to records whose
`disposition_rules_version` it actually implements, and reports `not-applicable` otherwise.

`stage1.triage` widens `ClassifyTriage`, which returns only `bug`, `feature` and
`unclassified` (`internal/ghissue/source.go:166-177`), with `question` and `duplicate` for
the cases intake's classifier does not model. Stage 1 is authoritative for triage and
re-derives the classification from the snapshot; the `triage` value already in the Task
payload is intake's earlier reading and is carried only for comparison. The reviewer's
structural check reconciles the two, so a stage 1 that silently disagrees with the
classifier is caught rather than hidden.

## Stages

**Stage 1, deterministic.** A pure function from the canonical snapshot to signals and a
classification. Signals are: the sorted label set, a title prefix, the presence of a
fenced reproduction block, and a cross-reference to another open issue in the same
repository, from either a `duplicate-of` label or a `#N` reference to a known open issue.

Stage 1 is deliberately conservative and resolves exactly one case decisively:

| Order | Condition | Disposition | Rule |
| --- | --- | --- | --- |
| 1 | a `duplicate-of:<n>` label, or a `#n` reference resolving to an open issue | `duplicate` | `cross-reference-duplicate` |
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
| 3 | `scope` is `small`, `medium` or `unknown` | `ready-to-plan` | `actionable-without-repro-small-or-medium` |
| 4 | `scope` is `large` | `needs-human` | `scope-large-needs-decomposition` |

`duplicate` is deliberately absent from this table. A cross-reference is a stage 1 signal,
and stage 1 resolves it decisively, so stage 3 can never produce `duplicate` and the
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
type Model interface {
    Classify(ctx context.Context, input Stage2Input) (Stage2Output, error)
}
```

`internal/ghtriage` owns the interface, the input and output types, and schema validation.
The production adapter is `internal/ghtriage/climodel`, which execs the configured CLI and
decodes the first JSON object from its stdout. The fake is `internal/ghtriage/fakemodel`,
which returns a scripted output and records every input it was given.

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
| superseded revision | `BLOCKED` | `superseded-by:<revision>` |

`ACTIVE` after triage is deliberate. A GitHub case cannot be closed today: `Close` requires
`READY_FOR_VERIFICATION` (`internal/workflowcase/service.go:261-263`), and the final
verifier skips every case whose source is not `ado`
(`internal/adoreview/verify.go:120-122`). `BLOCKED` is the terminal state for triage in
this design, and `ACTIVE` means "triaged and waiting for the planning subproject".

Leaving a `not-actionable` case in `ACTIVE` would be a hazard: `ListActive` is what the
planning subproject will scan, so a case that must never be planned would look like work.

The driver also closes the Task. After a lease completes, `CompleteAttempt` moves the Task
to `AWAITING_VERIFICATION` (`internal/verification/service.go:103`) and only `AcceptTask`
moves it to `SUCCEEDED` (`:188`). A Task transition is not a case transition and is not
blocked by the `UNKNOWN →` case-state restriction above, so the driver accepts the triage
Task with `AcceptTask`, exactly as `FinalVerifier.finalizeTask` does
(`internal/adoreview/verify.go:447-467`), citing the decision evidence as completion. The
driver discovers the Task by the same ADO pattern: look it up by the case's
`current_work_id` and wait for `TaskAwaitingVerification`
(`internal/adoreview/driver.go:222`).

## Supersession

`workflow_cases` is unique on `(mission_id, source, object_id, revision_id)` and the
revision is the issue's `updated_at`. A human relabelling or commenting on an issue in
GitHub therefore changes the revision, and intake registers a new case and a new triage
Task for the same issue at that revision. This is the free retriage path: the human
changes the facts at the source and triage re-runs with the new signals, while the older
case remains as a record of the earlier reading.

The cost is that two `ACTIVE` cases and two triage Tasks then exist for one issue. The
driver prevents the older revision from being planned: when it observes a case whose issue
has a newer revision that triage has already resolved, it blocks the older case through
`Assess` with reason `superseded-by:<revision>`. The newer revision stays `ACTIVE`.

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
- `rule_matches_recomputation`: recompute the disposition from the stored stage 2 document
  and compare both the disposition and the rule name. Three-valued, because on the
  stage-1-decisive path there is no stage 2 to recompute from:
  `match`, `mismatch`, or `not-applicable` when the record's `disposition_rules_version` is
  not the version the reviewer implements.
- `state_matches_disposition`: the case state matches the disposition and reason, treating a
  `superseded-by:` reason as an allowed terminal state so a correctly superseded case is not
  reported as a violation
- `classification_agrees_with_intake`: `stage1.triage` agrees with the `triage` value in the
  Task payload, or differs only by a widening to `question` or `duplicate`

**Plausibility check**: one model call asking whether the recorded classification follows
from the issue text. The result is a single boolean, `plausible`, and nothing else: the
reviewer is asked for a yes or a no and must not return a severity, a score or a remedy.

```json
{
  "schema": "github.issue.triage.review.v1",
  "repository": "owner/name",
  "issue": 42,
  "revision": "2026-09-28T10:00:00Z",
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

The reviewer is idempotent, and the mechanism is the one already available:
`FindByContentHash` (`internal/evidence/store.go:140`). The verdict document is canonical —
fields in fixed order, the decision's content hash included — so two ticks that reach the
same verdict produce byte-identical bytes and the second is suppressed. Two ticks that reach
different verdicts each record one, which is honest rather than a growth bug. The evidence
store has no case or revision column, so content hashing is the only available key, and it
is the correct one here because the verdict is a function of the decision.

A failed model call records **no** verdict document. It writes a line to stderr and returns,
so the retry on a later tick is not suppressed by the idempotency rule. Recording an
"unavailable" verdict as a document would collide with the real verdict on the next tick and
make the retry permanently impossible.

## Error handling

| Condition | Result |
| --- | --- |
| stage 1 | cannot fail: intake already excluded malformed issues as `unparseable-issue` (`internal/ghissue/source.go:76-122`) |
| stage 2 transport or timeout | the attempt fails and the lease does not complete |
| stage 2 schema violation | at most one retry with a stricter instruction, then the executor returns `needs-human` as a terminal result |
| stage 3 | cannot fail: it consumes a document stage 2 already validated, and the rule table is total |
| model configuration missing or unusable | startup error, not a tick error |
| a Task fails twice with the same signature | `FailAttempt` moves it to `TaskBlocked` (`internal/execution/service.go:378-391`); the driver reports it and leaves the case `ACTIVE` and untriaged |
| driver sees an inconsistent case | reported, case left untouched |
| reviewer model call fails | no verdict document, stderr line, retried on a later tick |

Retry counts are bounded. An unbounded retry turns one poisoned issue into an infinite budget
burn, which is the worst possible property here. The `TaskBlocked` row matters for the same
reason: a Task the driver never inspects and never reports is a silent loss, so the driver
surfaces it rather than leaving it to time out unnoticed.

## Idempotency

The executor does not dedupe by content hash before calling the model, because that cannot
work here. A decision contains a model rationale, so its bytes are not a deterministic
function of the issue and their hash cannot be known before the model runs. The intake
observer gets away with it only because its snapshot is
`CanonicalSnapshot`-derived (`internal/ghissue/source.go:274-298`).

The real replay path is the attempt's own provenance: read
`manifests.Provenance(attempt.CurrentAttemptID)` and check its `OutputEvidence`
(`internal/runmanifest/service.go:103-105`) for an object of kind
`github.issue.triage.decision`. If the completed attempt already produced the decision, the
executor returns that stored result and does not call the model. This is exactly how the ADO
driver replays a published artifact (`internal/adoreview/driver.go:218-231`, `:343-349`).

The driver is idempotent by construction: `Assess` returns the stored result for a
byte-identical request, so a case already in its target state is not re-transitioned, and
`AcceptTask` is a guarded `AWAITING_VERIFICATION → SUCCEEDED` transition. The reviewer is
idempotent by content hash, as described above.

## Testing

`internal/ghtriage` is tested with no network: `fakemodel` and a real store.

Stage 1 and stage 3 are pure functions and are tested as tables over snapshots. Stage 2 is
tested through `fakemodel`, which records the `Stage2Input` it received, so the tests assert
on the exact prepared document and on the fixed question literal. The model adapter is
tested against a stubbed CLI emitting malformed, non-JSON and multi-object output.

The driver is tested for transitions, reasons, idempotency of both the `Assess` replay and
`AcceptTask`, Task acceptance, and supersession. The executor is tested against a real Task
and a real store, following the publish executor and ADO driver tests; note that intake
registers no executor at all (`openGHIntakeBox` sets `cfg.Executors = nil`), so there is no
intake executor test to imitate.

The reviewer is tested against deliberately injected inconsistencies — a stage 3 rule that
does not match a recomputation, a case state that contradicts its disposition, a stage 2
present although stage 1 was decisive, a classification that disagrees with the payload, a
missing schema field, a correctly superseded case, a record with a future rules version — and
for producing exactly one verdict across two ticks, a second verdict when the first differs,
and no verdict at all when the model call fails.

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
