# GitHub issue triage — design

Date: 2026-09-28
Status: design approved, not yet implemented
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
document of signals, and stage 3 computes the disposition from that document with a rule
table. The same model output therefore always yields the same disposition, and a stored
decision can be recomputed and audited without re-running the model.

## Scope

In scope:

- the three-stage decision pipeline, recorded as a durable evidence document
- the executor that consumes an already-registered `github.issue.triage` Task
- the driver that applies the case state transition once a triage lease has completed
- supersession of an older revision when a newer revision of the same issue is triaged
- a `state_reason` column so a `BLOCKED` case can say why
- the stage 4 reviewer as a second, separate loop
- capability advertisement so the already-registered triage Tasks become leasable

Out of scope, explicitly:

- any GitHub write: no comment, no label, no draft pull request
- planning, decomposition, or code writing
- closing a case whose source is `github`
- automatic retriage triggered by a mid-execution observation
- any interface for a human to label or override a decision
- the reviewer gating, blocking, or changing anything

## What intake already provides

These exist and are not rebuilt here:

- one case and one `ELIGIBLE` Task per issue, created atomically by `EnsureAndMaterialize`
- case identity `(mission_id, source, object_id, revision_id)`, where the revision is the
  issue's `updated_at` normalised to UTC RFC3339Nano
- the Task class `github.issue.triage`, requiring capability `github.issue.read`, with
  acceptance criterion `triage decision recorded for <revision>`
- the deterministic classifier `ClassifyTriage` returning `bug`, `feature` or
  `unclassified`
- the canonical issue snapshot as evidence, referenced by the Task payload rather than the
  issue body itself

Because intake already creates one Task per issue, triage is one Task per issue and there
is no parent task and no fan-out code. The scheduler is the distributor: `Next` selects
among all eligible tasks, and the worker loop leases them one at a time. A parent task
would have to reimplement work selection, ranking, budget isolation and dependency
ordering, and would lose per-revision idempotency and per-issue retry granularity.

## Architecture

Three processes, matching the existing ADO shape of observer, driver and verifier.

| Process | Role |
| --- | --- |
| `run-worker` | leases the registered triage Task and executes it |
| `run-gh-triage-driver` | applies the case state transition after a triage lease completes; marks superseded revisions |
| `run-gh-triage-review` | reads recorded decisions, runs stage 4, writes a verdict |

The triage executor is registered in `run-worker` under its own kind. Because
`workerCapacity` derives the advertised capability set from the registered executor kinds
and already has a precedent for a special case (`ado-publish` additionally advertises
`ado.pr.comment` and `ado.pr.approve`), registering the triage executor is what makes
`github.issue.read` advertised and therefore what makes the already-registered triage
Tasks leasable at all. The intake composition continues to register no executor, so
intake stays inert.

The state transition is deliberately not performed inside the executor. An executor runs
while a lease is held; transitioning the case there would change case state before the
successful attempt was recorded, which is the same partial-state shape that
`EnsureAndMaterialize` was built to prevent. The driver therefore runs after completion
and observes the ordering guarantee directly.

## Decision record

One evidence document, kind `github.issue.triage.decision`, following the
`ado.review.decision` precedent. Every field records its origin, because the whole point
of the record is that a decision can be explained, recomputed and later re-scored.

```json
{
  "schema": "github.issue.triage.decision.v1",
  "repository": "owner/name",
  "issue": 42,
  "revision": "2026-09-28T10:00:00Z",
  "stage1": {
    "triage": "bug|feature|unclassified|question|duplicate",
    "signals": ["label:bug", "title:[bug]", "has-repro", "duplicate-of:41"],
    "disposition": "not-actionable",
    "rule": "label-hold"
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
    "rule": "actionable-without-repro-and-small"
  }
}
```

`stage1` is always present. Its `disposition` and `rule` are set only when stage 1 resolved
the issue on its own, in which case `stage2` and `stage3` are both absent. When stage 1
did not resolve, `stage1.disposition` and `stage1.rule` are empty and `stage3` is the
authoritative outcome. The absence of `stage2` and `stage3` is therefore itself the record
that the model was not consulted, and `stage3.rule` names the rule that produced the
disposition, so replaying a decision needs no re-execution.

## Stages

**Stage 1, deterministic.** Receives the parsed issue. Extracts signals: labels, title
prefix, presence of a reproduction, cross-reference to another issue. Classifies into
`bug`, `feature`, `unclassified`, plus `question` and `duplicate`. When the signals are
decisive, stage 1 resolves the issue itself: it records its own disposition and the rule
that produced it, and **stages 2 and 3 are both skipped**. The model is consulted only for
what the rules could not settle, which is the "do not stuff the context" rule.

**Stage 2, model.** Receives only deterministically prepared, filtered context: the issue
body with the already-extracted signals removed, plus the question that remains open. It
never receives the full raw issue. It returns the `stage2` document, which is validated
against a fixed schema; a non-conforming response is rejected rather than repaired or
interpreted. Free text never influences the disposition.

**Stage 3, deterministic.** A pure function from the validated `stage2` document to a
disposition and the name of the rule that produced it. No model call appears in this
function. Example rule: actionable without reproduction needed and small scope yields
`ready-to-plan`; not actionable yields `not-actionable`; a cross-reference yields
`duplicate`.

## Case state transitions

A `state_reason` column is added to `workflow_cases` by a new migration. The table
currently carries only `state`, and the current CHECK constraint allows `ACTIVE`,
`BLOCKED` and `READY_FOR_VERIFICATION`. The reason is required when the state is
`BLOCKED` and must be empty otherwise, so an unexplained `BLOCKED` row cannot exist.

| Disposition | Case state | Reason |
| --- | --- | --- |
| `ready-to-plan` | stays `ACTIVE` | — |
| `not-actionable` | `BLOCKED` | `not-actionable` |
| `duplicate` | `BLOCKED` | `duplicate` |
| `needs-human` | `BLOCKED` | `needs-human` |
| superseded revision | `BLOCKED` | `superseded-by:<revision>` |

`ACTIVE` after triage is deliberate. A GitHub case cannot be closed today: `Close`
requires `READY_FOR_VERIFICATION`, the final verifier skips every case whose source is not
`ado`, and `Assess` is only called from ADO code. `BLOCKED` is the terminal state for
triage in this design, and `ACTIVE` means "triaged and waiting for the planning
subproject".

`BLOCKED` is the right state for the non-actionable dispositions because it already means
"waiting on external input" in this project and has precedent through `Reject`.

Leaving a `not-actionable` case in `ACTIVE` would be a hazard: `ListActive` is what the
planning subproject will scan, so a case that must never be planned would look like work.

## Supersession

`workflow_cases` is unique on `(mission_id, source, object_id, revision_id)` and the
revision is the issue's `updated_at`. A human relabelling or commenting on an issue in
GitHub therefore changes the revision, and intake registers a new case and a new triage
Task for the same issue at that revision. This is the free retriage path: the human
changes the facts at the source and triage re-runs with the new signals, while the older
case remains as a record of the earlier reading.

The cost is that two `ACTIVE` cases and two triage Tasks then exist for one issue. The
driver prevents the older revision from being planned: when it observes a case whose
issue has a newer revision that triage has already resolved, it marks the older case
`BLOCKED` with reason `superseded-by:<revision>`. The newer revision is the one that stays
`ACTIVE`.

Automatic retriage from a mid-execution observation — a suband discovering during bug
work that the issue is really a feature — is not built and not planned. Such a case may
already hold a lease, and rewinding a case that may have produced effects contradicts this
project's authority model. The intended path is observational: the observation is
attached to the existing decision, and the process is improved by revising the stage 1
rules or the stage 3 rule table in a later slice, after which past decisions can be
re-scored offline. That path is viable only because every decision records its
provenance, which is why the record keeps the stage 1 signals even though they have no
further use at triage time.

## Stage 4 reviewer

A separate loop, because the reviewer reads a decision that has already been recorded.
Running it inside the same lease would put a second model call in the critical path and
would fold a "not confident" signal into the acceptance criterion "triage decision
recorded for <revision>", which the record must keep meaning.

The reviewer resolves the body through the chain case, task, payload, snapshot evidence,
body: the intake Task payload carries a snapshot evidence id, not the issue body.

**Structural checks**, which are invariants rather than judgements:

- stage 2 is present if and only if stage 1 did not resolve the issue
- the record conforms to schema `v1`
- the rule named in stage 3 matches the rule that stage 3 actually computes from the
  stored stage 2 document, which the reviewer recomputes
- the case state matches the disposition, including the reason

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
    "rule_matches_recomputation": true,
    "state_matches_disposition": true
  },
  "plausible": true
}
```

The reviewer changes nothing, gates nothing and blocks nothing.

Its verdict is written as evidence, kind `github.issue.triage.review`, and also printed to
stderr for operator visibility. Writing it as evidence is what allows the reviewer itself
to be scored later, retroactively, once human labels exist. There are no human labels and
no prior decisions yet, so effectiveness cannot be evaluated now, and the reviewer does
not pretend to: it checks what can be checked without ground truth.

The reviewer is idempotent. Without dedupe it would append a verdict per tick, which is the
unbounded evidence growth that the intake observer was corrected for.

## Error handling

| Condition | Result |
| --- | --- |
| stage 1 | cannot fail: intake already excluded malformed issues as `unparseable-issue` |
| stage 2 transport or timeout | lease does not complete; the issue is retried in a later iteration |
| stage 2 schema violation | at most one retry with a stricter instruction, then `needs-human` and `BLOCKED` |
| stage 3 | cannot fail: it consumes a document stage 2 already validated |
| model credentials missing or unusable | startup error, not a tick error |
| driver sees an inconsistent case | reported, case left untouched |
| reviewer model call fails | verdict recorded as review unavailable, retried on a later tick, nothing else affected |

Retry counts are bounded. An unbounded retry turns one poisoned issue into an infinite
budget burn, which is the worst possible property here.

## Idempotency

Before calling the model, the executor checks whether a decision already exists for
`(mission, case, revision)`. If it does, the stored decision is returned rather than a new
one generated. This is the same pattern the intake observer uses to deduplicate evidence by
content hash, and it exists for the same reason: a model call inside a lease is
non-deterministic, so a retried lease must replay a result, never produce a new one.

The driver is idempotent by construction: a case already in its target state is not
re-transitioned. The reviewer is idempotent by deduplicating on `(case, revision, review
schema version)`.

## Testing

`internal/ghtriage` is tested with no network: a fake model adapter and a real store. The
executor is tested against a real Task, as intake was. The driver is tested for
transitions, reasons, idempotency and supersession. The reviewer is tested against
deliberately injected inconsistencies — a stage 3 rule that does not match a recomputation,
a case state that contradicts its disposition, a stage 2 present although stage 1 was
decisive, a missing schema field — and for producing exactly one verdict across two ticks.

The scheduler side of capability advertisement is already covered by the existing
`TestNextClaimsGitHubIssueTriageWithEnforcedReadCapability`, which must keep passing.

The full validation matrix from `CONTRIBUTING.md` applies, including the OCI profile.

## Deferred

- planning and decomposition of `ready-to-plan` cases
- any GitHub write capability, including a draft pull request
- closing a case whose source is `github`
- human labelling of decisions, and a `needs-human` queue or interface
- evaluating reviewer effectiveness, which requires labels
- automatic retriage from a mid-execution observation
- a routing table from task class to executor kind, already deferred by the publish
  executor design
