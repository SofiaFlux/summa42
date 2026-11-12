# Governed work classes: code, investigation and operations on one path

**Status:** proposal for the Owner. Nothing here is approved, and no build card
in section 9 starts before the Owner approves it. It proposes changes to the
[development plan](../plans/2026-11-11-development-plan.md) (section 8) and
does not edit the plan.

**Date:** 11 November 2026. **Base:** `main` @ `2db7d4a`.

It builds on the agentic workspace executor spec (card M1A-T1, pull request
#185) and does not repeat it: the sandbox, the gateway, collection and the data
classes are defined there.

## 1. Problem

The Owner asked two questions on 11 November 2026: whether Summa42 restricts
agents too much, and whether the project earns its cost when a coding agent in
a cloud sandbox with branch protection already takes an issue to a pull
request.

The honest reading of the repository:

1. **The rule is right; today's implementation is too narrow.** Agents hand
   evidence to the Box and never address each other. That is a blackboard with
   a coordinator, and it is what keeps a producer and a reviewer independent.
   But every model call today is one read-only question, and an implementation
   comes back as whole files in one JSON document. The plan says so itself
   (section 2.6).
2. **The path is shaped like one use.** Five `run-gh-*` commands take a GitHub
   issue to a draft pull request. Reading logs or changing infrastructure has
   no path at all, although the kernel underneath (Tasks, Attempts, evidence,
   capabilities, external operations, approvals) is not specific to code.
3. **Every stage pays the same control.** Work that cannot cause an effect
   outside the Collective goes through the same claims, reviews and holds as
   work that publishes.
4. **What is unique is the kernel.** Authority that only narrows, approvals
   bound to one exact operation, evidence, budgets, standing delegations, and
   one model of authority across code, support and money. A sandbox, a harness
   and a task loop exist elsewhere and are better there.

## 2. Principle

> Summa42 governs work; it does not perform it. Inside a sandbox an
> off-the-shelf agent works with every tool. Summa42 decides what goes in,
> mediates every tool that reaches outside, and controls what comes out.

Three consequences:

- **Control follows the effect, not the stage.** Work without an outside
  effect needs no review and no approval. Control starts where something
  leaves the Collective.
- **Tools without credentials.** An agent reaches a system only through a
  capability Session of the Box. The Box holds the credential and applies
  policy to each call.
- **An agent never performs a change to a live system.** It prepares the
  change; trusted code performs exactly what was approved.

Nothing in the Vision changes. Articles 6, 7, 9, 17, 18 and 27 are what this
applies.

## 3. Work classes

A work class is a named combination of a sandbox profile, the capabilities
offered, and an exit gate. It is data on the Task, not a new pipeline.

| Class | Inside the sandbox | What leaves | Exit gate |
| --- | --- | --- | --- |
| `code` | Full harness, shell, toolchains, hours (M1A) | One commit collected by the Box | Independent validation, independent review, publication through the commitment boundary; the Owner or a delegation merges |
| `investigate` | Full harness; read-only capabilities (logs, metrics, work items, repositories) | A report whose every claim cites a recorded capability call | None. The report is evidence; it causes nothing |
| `operate` | As `investigate`, plus tools that compute a plan | A change plan: an ordered list of external operations, each with its exact arguments and its way back | The Box performs the approved plan, operation by operation, through the existing external operation path |

### 3.1 `code`

Unchanged from the M1A spec. The one-shot plan and implementation stages stop
being the producer once M1A-T3 runs; they remain as a fallback on a host
without a container runtime, at their present enforcement level. Triage, plan
review and code review stay one-shot: a reviewer needs no tools that write.

Agents inside one sandbox may cooperate freely (a harness that starts its own
sub-agents is one Attempt and one blast radius). Rule R12 is about roles and
Attempts, not about what happens inside one sandbox.

### 3.2 `investigate`

The cheapest class and the one that shows the kernel earns its place outside
code. Example: "why did ingestion of dataset X stop on Tuesday".

- The Task declares the read capabilities it needs (`RequiredCapabilities`
  and `AuthorityCeiling`, as today for `ado.*`). The agent calls them through
  the capability Session; the Box records each call and result as evidence.
- The data class of every capability decides the models allowed (M1A spec,
  section 5.1). Logs of a production system are `customer-data` unless the
  Project manifest says otherwise: zero-data-retention or local models only,
  no fetch.
- The report is checked deterministically before it is stored: every cited
  call exists in this Attempt's evidence, and the cited text occurs in the
  recorded result. A claim without a citation is marked as the agent's
  inference. There is no model review.
- This is the plan's card M2-T9 ("work that is not code") moved earlier, with
  one difference: M2-T9 has the independent reviewer read the artefact; here
  the citation check replaces the model review for read-only work. If the
  Owner prefers the review, it is one more stage in the class's data.
- The result is delivered to the Owner (M3-T3). It can be the input of a
  later `code` or `operate` Task only as evidence that passed output hygiene
  (M9-T8), like any text one role hands another.

### 3.3 `operate`

Example: "scale the ingestion workers to four" or "rotate the storage key".

- The agent never holds a credential that can change a system and never runs
  the change. It reads state through capabilities and produces a plan.
- A plan is a list of external operations in the form `internal/operations`
  already has: provider, action, exact arguments, effect slot, risk class,
  and for each operation the inverse operation or the statement that there is
  none.
- Where a tool has its own plan format (`terraform plan`, a pipeline run with
  parameters), the Box computes the plan itself from the agent's change to
  the infrastructure repository, through the `code` class. The agent's own
  rendering of a plan is never what gets approved.
- The Box performs the plan through `operations` (policy, approval, resource
  reservation, effect slot, uncertain-outcome reconciliation, all existing).
  Read operations need nothing. A reversible LOW-risk operation can run under
  a standing delegation (M5). Everything else needs the Owner's approval of
  that exact operation.
- After each operation the Box reads the state back through a capability and
  records it. A mismatch stops the plan and raises a containment signal; the
  remaining operations are not performed.

## 4. One path instead of five commands

Today: `run-gh-intake`, `run-gh-plan`, `run-gh-plan-review`,
`run-gh-implement`, `run-gh-code-review`, `run-gh-publish`, with about twenty
flags. Proposed: the worker loop that card M1-T4 (`serve`) already introduces
runs one sequence for every class.

```text
intake -> triage -> [plan review] -> attempt in sandbox -> collect -> exit gate of the class
```

- The stage set per class is data: `investigate` has no plan review and no
  code review; `operate` has plan approval instead of code review.
- The `run-gh-*` commands stay as they are until the pilot tasks are done
  (M1-T5), so that the A1 measurements are comparable. They are not extended.

## 5. Capabilities are the tools

Card M9-T2 (capability Sessions in execution) is at the end of the plan.
Without it `investigate` and `operate` cannot exist, so it moves up (section
8). What it has to deliver for this design:

- The sandbox gateway exposes the Task's capability Session to the harness as
  an MCP server. The agent sees exactly the declared capabilities.
- Each call passes the emergency check and policy that
  `internal/capabilities/session.go` already applies, and becomes evidence.
- First providers, read-only, in this order: the existing Azure DevOps reads;
  Azure Monitor log queries; Azure resource state. Each is one connector
  under the contract of card M7-T2.

## 6. One point of control for model processes

Taken from OpenAI's Control Room (announced November 2026): the inference
engine asks a central service for permission before it produces output; if the
service cannot be reached, the answer is no; a bound on the time from "stop"
to "stopped" is stated; and a sweep looks for engines that run without the
integration.

Where Summa42 stands, read from the code at the base commit:

| Idea | Today | Gap |
| --- | --- | --- |
| Ask before work | Break glass and the dead man switch are checked when an Attempt is leased, on every external operation and on every capability call (`emergency.CheckLeaseAllowedTx`, `CheckExecutionAllowed`) | `internal/modelrun` does not ask. A model call under a lease taken before the freeze still starts, and commands that call a model without a Task (`model-probe`, `model-profile-draft`) are not gated |
| No answer means no | The checks fail closed on a database error | Same gap: the launcher has no check to fail |
| Bounded time to stop | A freeze revokes leases | A model process that is already running is not killed; it ends at its own timeout, which is minutes |
| Sweep for what bypasses the control | `TestProcessesStartOnlyFromKnownPackages` (added with this spec's pull request) fails when a new package starts processes | Static only; it does not look at running processes |

**Built on 11 November 2026 at the Owner's instruction** (`internal/modelrun/gate.go`,
`cmd/summa42-box/emergencygate.go`); the table above describes the state before
it. What was built follows the card below, with one refinement: a gate that
cannot answer stops a running process only after three checks in a row, so one
busy database read does not kill an hour of work, while a definite refusal
(break glass FROZEN) stops it at the next check.

The card as proposed (section 9, M9-T9): the launcher takes a gate from its caller.
Before `Start` it asks the gate and refuses on any error. While the process
runs it asks again at a fixed interval and kills the process group on refusal,
with the existing kill path. The interval is a constant, default 5 seconds,
and one test measures the time from freeze to the death of a running process.
A caller that passes no gate is refused, except in tests. This only narrows
(rule R6) but it changes `internal/modelrun`, a protected path, so the Owner
reviews it.

## 7. What this does not do

- It does not let agents address each other across Attempts or roles.
- It does not put a credential in a sandbox.
- It does not let an agent's statement about its own work count as a result.
- It adds no dependency and no service. Everything is in the one Box.
- It does not claim parity before the benchmark (section 8) shows it.

## 8. Proposed changes to the development plan

For the Owner to accept, change or refuse. The plan is a protected path; an
accepted change is made in the plan by a pull request the Owner approves.

1. **Order.** After M0: M9-T6 and M9-T8, then M2-T1 to M2-T3 (the Project
   record and its signed manifest, which M1A-T3 depends on), then M1A (T1,
   T2, T8, T3), then M9-T9 and M9-T2, then the `investigate` class, then
   M3-T2 and M3-T3 (money and notifications), then the `operate` class. The
   rest of M2 (several Projects side by side) moves after M1A: one Project is
   enough to learn whether the design holds.
2. **M1A-T6, the parity benchmark,** gains one `investigate` task and one
   `operate` task next to the coding tasks, each also done by a bare agent
   with ordinary repository protection, and compares Owner interventions,
   wall time and cost. This is the measurement that answers whether the
   project earns its cost.
3. **A stop rule.** If after the ten pilot tasks and the benchmark Summa42 is
   not within a factor the Owner names of the bare agent on interventions and
   wall time, M6, M7 and M8 are not started and the scope is cut to the
   kernel plus the sandbox executor.
4. **Deferred until the three classes run:** M6 (automation loop), M7 beyond
   M7-T2 and M7-T3, M8, M3-T6 and M3-T7 (direct API providers, routing), and
   new harness profiles beyond the ones confirmed live.
5. **Rule R5** gains `.github/CODEOWNERS` and `azure-pipelines*.yaml`, which
   the protected path gate already covers.

## 9. Build cards

None starts before the Owner approves this spec. Each is one branch and one
draft pull request under the rules of the plan.

| Card | Content | Depends |
| --- | --- | --- |
| M9-T9 `BUILD` `agent` | Launcher gate (section 6). Tests: `TestLauncherRefusesWhenFrozen`, `TestLauncherRefusesWhenGateFails`, `TestFreezeKillsRunningModelWithinBound`, `TestLauncherWithoutGateRefused` | Owner review of `internal/modelrun` |
| W-T1 `BUILD` `agent` | Work class as a field of the Task and the stage set per class as data; `code` behaves exactly as today | M1-T4 |
| W-T2 `BUILD` `agent` | Capability Session offered to the sandbox as an MCP server through the gateway (the part of M9-T2 this design needs) | M1A-T8a, M1A-T3a |
| W-T3 `BUILD` `agent` | `investigate`: report format, the citation check, delivery. Tests: `TestReportCitationMustExistInEvidence`, `TestUncitedClaimIsMarkedInference`, `TestInvestigateHasNoExternalOperation` | W-T1, W-T2, M3-T3 |
| W-T4 `BUILD` `agent` | Azure Monitor log query connector, read-only, `customer-data` by default | W-T2, M7-T2 |
| W-T5 `DESIGN` | `operate`: the plan format, read-back, the first three operations for the first product, which are reversible | W-T3, M3-T2 |
| W-T6 `BUILD` `agent` | `operate` as designed in W-T5 | W-T5 approved, M5-T1 for delegated runs |

## 10. Questions for the Owner

Each with the default this spec assumes.

1. Is the order in section 8.1 accepted, in particular several Projects side
   by side only after M1A? Default: yes.
2. Which factor is the stop rule's threshold (section 8.3)? Default: within
   two times the bare agent on interventions and three times on wall time,
   measured over the benchmark tasks.
3. Are production logs `customer-data` by default? Default: yes; a Project
   manifest can declare a log source free of customer data.
4. Which three operations come first for `operate`? Default: restart a
   service, scale a worker pool, re-run a failed pipeline stage.
5. Should a model call without a Task (`model-probe`, `model-profile-draft`)
   obey break glass? Default: yes; a freeze means no model runs.
