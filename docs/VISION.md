# Summa42 Vision

> **Keep the question. Govern the answer.**

Summa42 is a vision for a durable Collective: a system that can pursue a
bounded mission over time, through changing people, models, tools, machines and
individual agents, without confusing activity with authority or output with
truth.

The name is a reminder that an answer is never the end of the work. A good
answer preserves its question, its evidence, its limits, and the right to be
challenged.

This document describes the intended direction of the project. It is a public
constitution and product vision, not a statement that every property already
exists in the Minimal Viable Collective (MVC). The current implementation is a
small, local-first, single-Cube experiment. The detailed, normative engineering
design lives in the [design specification](superpowers/specs/2026-09-14-summa42-design.md).

## A compact statement

Summa42 exists to make long-running agentic work governable.

It treats a mission as more durable than the agents that serve it; authority as
bounded, explicit, and revocable; knowledge as evidence with provenance rather
than accumulated assertion; and uncertainty as a valuable result when it leads
to a safe next step.

It is not a system for making an AI increasingly unconstrained. It is a system
for making delegated work increasingly legible, accountable, and corrigible.

## The constitutional order

Every part of the Collective has a place in an order of meaning and authority:

```text
Constitution
  └── Constitutional Root
        └── Owner
              └── Mission and obligations
                    └── Goals and task graph
                          └── Cube and Box
                                └── Summand (task-scoped cognitive worker)
                                      └── Attempts, executors, tools, and deterministic workers
```

The order matters.

- The **Constitution** states the invariants a Collective may not bypass for
  convenience, speed, popularity, or apparent success.
- The **Constitutional Root** protects the constitutional identity and the
  ceremony by which its highest authority is maintained. It is not a runtime
  convenience credential.
- The **Owner** is the accountable human or authorized principal that gives
  operational direction and grants authority within the Constitution.
- A **Mission** supplies durable purpose. It bounds what the Collective may
  optimize for and what it must preserve.
- A **Goal** is a reviewable proposition for advancing a mission or satisfying
  an obligation.
- A **Task** is durable work. An **Attempt** is one fallible execution of that
  work. Neither is the mission itself.
- A **Cube** is a sovereign logical unit of the Collective; a **Box** is the
  trusted control plane that holds canonical state and enforces its rules.
- A **Summand** is a task-scoped cognitive worker operating over a compiled, task-specific
  view of the Collective (Context Projection). It is explicitly not synonymous with an LLM or model.
  An Attempt may use heterogeneous executors and deterministic workers. Summands propose learning
  and evidence; they do not write canonical Collective knowledge directly or widen authority.
- **Executors**, models, tools, and deterministic workers are replaceable execution mechanisms.
  They do not become constitutional actors merely by being useful.

No lower layer may reinterpret or silently widen the authority of a higher one.

## Twenty-six articles of the Collective

### 1. Constitution before optimization

The Constitution comes before performance, revenue, autonomy, and convenience.
It defines the non-negotiable rules for identity, authority, evidence,
containment, accountability, and amendment. A Collective may be useful without
being trusted; it must never demand trust by claiming usefulness.

Constitutional change is deliberate, attributable, reviewable, and harder than
ordinary operational change. A runtime cannot amend the rules that constrain
the runtime.

### 2. Root, Owner, and accountable stewardship

The Root preserves constitutional continuity; the Owner supplies accountable
operational stewardship. They are distinct roles because emergency power,
normal administration, and day-to-day execution should not collapse into one
ambient credential.

The Owner may delegate narrowly. Delegation must name its principal, scope,
purpose, duration, conditions, and revocation path. A delegate receives only
the authority explicitly granted, never the Owner's identity or all of the
Owner's future choices.

### 3. Mission is durable; agents are ephemeral

An agent may time out, hallucinate, be replaced, lose context, be compromised,
or simply disappear. A model may change. A machine may fail. These events must
not erase purpose, fabricate completion, or mint new authority.

The Collective therefore keeps mission, task identity, state, evidence,
approvals, budget, and audit in durable governed records. An agent is a
temporary participant in a process, not the process's memory, sovereign, or
source of truth.

This is the heart of the project: **the worker may end; the mission must remain
intelligible.**

### 4. Mission, purpose, and legitimate work

A Collective may hold one active mission at a time, together with explicit
obligations that protect people, assets, commitments, and its own integrity.
Every goal and task must trace to a mission, obligation, or authorized
maintenance purpose.

Missionless work is not harmless background activity. Discovery, curiosity,
maintenance, and research can be legitimate, but only inside a defined envelope
with budget, boundaries, and an accountable reason to exist. When no legitimate
work remains, dormancy is a success state.

### 5. Authority is scoped, not a global score

Authority is not intelligence, confidence, reputation, tool access, a task
label, or a count of successful runs. It is an explicit permission to take a
particular class of action in a particular domain under stated conditions.

Every authority envelope is therefore domain-scoped: for example, it may allow
one operation on one resource for one mission before one deadline under one
budget and one enforcement profile. Possessing authority in one domain says
nothing about another.

Capability answers “can this be done?” Authority answers “may this actor do it
now?” The answers must remain separate.

### 6. Authority may contract autonomously, never expand autonomously

The Collective may reduce, suspend, or surrender its own authority. It can
choose a stricter policy, lower its budget, disable a capability, request human
review, or enter safe dormancy. These are safe because the new envelope is a
subset of the old one.

It may never autonomously enlarge an authority envelope. No successful task,
observed competence, model upgrade, self-modification, accumulated memory, or
economic opportunity turns into additional permission by itself. Expansion
requires an explicit, authorized, scoped, and revocable grant under the
Constitution.

### 7. Proof earns a proposal, not permission

Evidence matters. Reproducible success, independent verification, reliable
operation, and demonstrated restraint can justify proposing a future authority
grant. They never create that grant automatically.

This is what “earned autonomy” means in Summa42: proof can make a request
credible; it cannot silently change who is allowed to decide. Any promotion
must remain explicit, bounded, attributable, and reversible.

### 8. Deterministic guardrails around cognitive work

Reasoning systems are useful precisely because they are non-deterministic,
creative, and incomplete. That makes them unsuitable as the final arbiter of
their own authority, expenditure, or consequential effects.

Summa42 keeps cognitive work at the edge and deterministic governance at the
center. Policy, identity, leases, fencing, approvals, budgets, and commitment
boundaries must be evaluated by durable, inspectable mechanisms rather than by
a prompt asking an agent to behave.

### 9. Containment is a condition of trust

An execution path must state what it can actually enforce. A promise of
isolation is not isolation; a model's assurance is not a security boundary.

The Collective prefers least privilege, mediated operations over ambient
secrets, revocable task-scoped capabilities, constrained egress, resource
envelopes, and explicit enforcement levels. If a guarantee cannot be proved,
the system must downgrade its claim and act accordingly.

Failure and network partition do not suspend these rules. An isolated part of a
Collective may continue only within a pre-authorized, fenced, budgeted envelope
whose safety does not depend on fresh global agreement. It must quarantine or
stop when it cannot establish that its authority is still valid. Recovery is
reconciliation of durable facts, not a vote to forget an ambiguous history.

### 10. Memory is evidence, not a diary

The Collective remembers events, artifacts, claims, decisions, costs,
approvals, and outcomes with provenance. It distinguishes observation from
interpretation, verified fact from hypothesis, current knowledge from
superseded knowledge, and local context from transferable learning.

Memory must be temporal and revisable. A later correction does not erase the
fact that an earlier belief existed; it records why the belief was revised.
Sensitive information is minimized, compartmentalized, retained only as long
as justified, and subject to governed redaction and forgetting.

### 11. “I do not know” is a first-class result

The Collective must be able to say: *I do not know*, *the evidence is
insufficient*, *I cannot do this safely*, or *I cannot establish that outcome*.
These are not failures of presentation. They are honest results.

But uncertainty must lead somewhere governed: request clarification, defer,
refuse, observe safely, create a bounded research or validation task, test a
candidate in a safe environment, reconcile an external state, or request an
explicit grant. “Unknown” is never a permission to retry a consequential action
or to invent a confident story.

### 12. Cognition is plural, bounded, and challengeable

No single model, agent, source, or consensus ritual is treated as oracle.
Independent first passes reduce anchoring; targeted collaboration resolves
material disagreement; adversarial challenge remains legitimate after apparent
success.

The Collective spends cognitive effort in proportion to stakes, uncertainty,
reversibility, and value of information. Consensus is evidence about agreement,
not proof of truth. Dissent, minority reports, and unresolved questions are
durable outputs when they improve a later decision.

### 13. Goals are hypotheses with accountability

Goals may arise from a mission, an obligation, a detected gap, a safety concern,
or authorized maintenance. They are not self-justifying. A consequential goal
requires a purpose, expected value, cost and risk envelope, acceptance criteria,
and a way to challenge or stop it.

The Collective may notice opportunities; it may not turn every opportunity into
work. A goal market without constitutional limits becomes a machine for
manufacturing reasons to expand itself.

### 14. Improvement is governed inquiry, not self-rule

Learning from experience is essential. The Collective may compare outcomes,
identify gaps, compile reliable procedures, create skills, test challengers,
and propose better workflows.

It may not silently rewrite code, policy, authority, evidence standards, or
security boundaries because it believes it learned something. Improvements are
versioned proposals with evidence, evaluation, promotion criteria, rollback,
and the same authority checks as any other consequential change.

### 15. Economics are governance

Cost is broader than an invoice. It includes compute, time, attention, risk,
resource contention, unresolved commitments, external exposure, and the cost of
being wrong. The Collective keeps a resource ledger and treats budget as an
authority boundary rather than a reporting afterthought.

It distinguishes settled cost from unresolved exposure. It does not spend gross
revenue as though it were free execution budget. It may account for value of
information, but cannot use a promising return to bypass approval or risk
limits.

### 16. Sufficient effort, surplus, and dormancy

The right amount of work is the minimum effort that makes a justified decision
or satisfies an obligation with the required confidence. More model calls are
not automatically more intelligence.

When legitimate capacity remains after essential work, it may be used for
bounded, low-risk preparation: health checks, reproducible tests, documentation,
or safe research. It may not create an empire of speculative activity. Sleeping,
being idle, and declining to optimize are normal and sometimes necessary.

### 17. Information is not authority

External content can be wrong, stale, malicious, or instruction-shaped. A web
page, repository, model response, email, issue comment, or retrieved document
is evidence to assess—not a command to obey.

The Collective keeps a knowledge firewall between information acquisition and
authority evaluation. Source identity, provenance, freshness, corroboration,
and contextual trust are assessed independently. No imported instruction gains
power by being persuasive or by resembling a system message.

### 18. Sovereignty, federation, and swarms

Each Collective is sovereign under its own Constitution, Root, Owner, and
authority model. Collectives may cooperate through explicit task contracts,
negotiated envelopes, evidence exchange, and accountable economic terms. They
do not merge authority by accident.

A swarm is a temporary coordination pattern, not a loophole around governance.
Its membership, scope, budget, evidence rules, and exit conditions remain
explicit. No number of cooperating agents can collectively acquire a permission
that none of them received.

### 19. Capability is assessed, acquired, and localized

The Collective discovers what resources and executors can actually do, under
which enforcement conditions and with which limitations. Capability assessment
is evidence-bearing, time-bound, and distinct from authority.

When a capability is missing, acquisition follows the least-escalating path:
reuse an approved operation, request a limited grant, create a safe test,
delegate under a contract, or report the gap. It does not reach for broad
credentials, ambient network access, or irreversible installation by default.

### 20. Scheduling is arbitration, not mere throughput

The scheduler first asks whether work is eligible: mission-aligned, authorized,
capable, funded, safe enough, and not superseded. Only then does it arbitrate
deadlines, scarcity, dependencies, locality, aging, preemption, and parallelism.

The deterministic core owns these rules. Cognitive agents may help estimate,
plan, and execute, but a clever suggestion cannot leapfrog another task's
constraints. Global intent and local dispatch stay separable so that scaling
does not erase accountability.

### 21. Task, attempt, verification, and challenge are separate

A Task can outlive many Attempts. An Attempt can produce output without
completing the Task. A completed Attempt can still fail independent verification.
And an accepted result can later be challenged when new evidence appears.

The Collective records lifecycle, lineage, leases, fence generations,
checkpoints, failure class, verification, and acceptance separately. This makes
restart, retry, handoff, and disagreement understandable rather than magical.

### 22. External effects have a commitment boundary

Before an external consequential operation, the Collective records an intent
and validates current policy, authority, approval, budget, lease, fence,
idempotency, and enforcement conditions. The transition to dispatch is a durable
commitment boundary: after it, an effect may have escaped the system.

An ambiguous result is **UNKNOWN**, not success and not permission to replay.
The Collective reconciles durable external-operation records with the outside
world before deciding whether further action is safe. It preserves unresolved
exposure in its budget and audit until the state is established or escalated.

### 23. Audit must explain causes, not merely events

The system's audit trail is causal: what was known at decision time, who or what
was authorized, which policy and evidence were used, what changed, which effect
was attempted, and why the outcome was accepted, rejected, or left unknown.

Observability is not a decorative dashboard. It is how the Owner, contributors,
and future agents can reconstruct responsibility without trusting a narrated
summary. Operational telemetry may be lossy; canonical governance records may
not be.

### 24. Humans remain participants, not emergency props

Human collaboration is designed into the system: clear intent, reviewable
proposals, attention routing, explanations at the right level, meaningful
approval, safe refusal, and break-glass paths with durable accountability.

The Collective should ask humans for decisions only where human authority,
context, or value judgment is genuinely needed. It should not offload every
uncertainty to them, nor should it hide meaningful uncertainty behind an
illusion of autonomy.

### 25. Evolution preserves reversibility

Upgrades, extensions, new executors, new models, and future self-modification
are evolutionary changes to a governed system. They require compatibility
contracts, staged evaluation, provenance, promotion gates, rollback, and a
clear account of what authority they do and do not receive.

The project prefers replaceable implementations behind stable semantic contracts.
`Agent != model`, `model != executor`, `executor != authority`, and `artifact !=
acceptance`. This is how the Collective can change without losing its identity.

### 26. MVC proves semantics before scale

The Minimal Viable Collective deliberately begins with one local Cube and one
trusted Box. It is not a miniature distributed empire. Its purpose is to prove a
semantic vertical slice:

```text
Owner → Constitution → Mission / obligation → goal → task
      → lease → attempt → verification → evidence / memory
      → outcome, audit, and economics
```

The MVC must make this chain real enough to test failure paths: restart,
revocation, stale workers, incomplete attempts, unknown external outcomes,
budget exhaustion, evidence review, and safe refusal. Multi-Cube coordination,
federation, richer memory, and broader automation are later work only when the
one-Cube semantics justify them.

## Summand as the task-scoped cognitive worker

The Collective establishes **Summand** as the canonical name for the task-scoped cognitive
worker, resolving ambiguity between the project/runtime (Summa42) and individual workers.

### 1. Summand lifecycle (Normative)

A Summand is explicitly instantiated per **Attempt** (an ephemeral cognitive worker leased
for one fenced attempt window). Durable context, identity, obligations, and state bind
strictly to the **Task**, not to the Summand. When an attempt completes, times out, or fails,
the Summand ceases execution; subsequent attempts instantiate a fresh Summand with an updated,
hash-bound Context Projection.

### 2. Task-scoped Context Projection

A Summand does not operate over an unconstrained ambient context window. Instead, a
Context Projection compiles the best bounded view for the task from governed inputs:

- relevant Skills (procedural knowledge);
- relevant Knowledge and Evidence (provenance-backed facts);
- allowed Tools (operational interfaces / actuators);
- granted Capabilities (permission / access scope);
- deterministic workers and scripts;
- executor and model selection;
- policy and authority envelopes;
- constraints and acceptance criteria.

The compiled projection is recorded as an immutable audit record in `evidence_objects`, is
hash-bound, and remains strictly immutable for the duration of an attempt.

### 3. Heterogeneous execution and least cognitive mechanism

A task is not assumed to require an expensive general-purpose reasoning model when a cheaper
or deterministic mechanism suffices. The governing principle is:

> **Use the least cognitive mechanism sufficient for the work.**

When choosing among mechanisms, the tiebreaker priority order is:

1. **Deterministic scripts / SQL** — maximum verifiability, lowest cost, deterministic execution;
2. **Specialized models / Tools** — bounded cognitive complexity, predictable outputs, lower latency and cost;
3. **General-purpose reasoning models** — reserved for open-ended synthesis, architecture, and novel decomposition.

This ordering optimizes for formal verifiability, containment, and resource accounting. An
Attempt may compose deterministic scripts, SQL queries, HTTP calls, specialized models,
and general reasoning models under the orchestration of a Summand.

### 4. Distinct taxonomy: Skills, Tools, Capabilities, Executors, and Authority

The architecture strictly demarcates:

- a **Skill**: reusable procedural knowledge (how to perform a class of work);
- a **Tool**: an operational interface / actuator that performs actions;
- a **Capability**: the granted permission or access scope to use a tool or resource;
- an **Executor**: a runtime harness that runs work;
- **Evidence / Knowledge**: provenance-backed information that informs work;
- **Authority**: explicit, revocable constitutional permission to act.

**Normative distinction:** Possessing a Tool does not grant Capability or Authority. Tools
define operational mechanisms; Capabilities define admissible operational envelopes; Authority
defines whether an actor may execute them. Stable, repeated reasoning compiles into validated
Skills, and where practical, into tested deterministic workers.

### 5. Blue-Green / Zero-Downtime canary handover for Cube updates

When a Cube evolves or updates its trusted Box binary, self-modification must preserve
reversibility without creating service interruption or widening authority:

1. **Spawn Green process**: Launch a parallel Box process on an isolated control channel with strict liveness checks.
2. **Verify state and health**: Confirm database integrity against SQLite state, audit logs, and operational health endpoints.
3. **Atomic socket / metadata handover**: Atomically transfer `control.sock` or update active processor metadata.
4. **Drain and terminate Blue**: Drain pending non-blocking operations from the Blue process, then gracefully shut it down.

If health checks fail at any step, the handover rolls back immediately to Blue. Authority is
never expanded by an update.

### 6. Decomposition feedback and reflection

Observed execution produces planning evidence and ProcessChangeProposals, not automatic
rewrites of the planner or task graph. A Summand reflects upon completion to produce
findings (KnowledgeDelta, SkillCandidate, DecompositionFinding, ProcessChangeProposal),
which are submitted as proposals:

> **Summands contribute evidence and proposals to the sum; only governed Collective processes promote them into canonical knowledge, Skills, or process changes.**

## What Summa42 refuses to be

Summa42 is not:

- an agent swarm whose members grant one another power;
- an autonomous business that treats revenue as a mandate;
- a prompt-based policy system that mistakes instructions for enforcement;
- a universal truth engine or an oracle for difficult decisions;
- a justification for hidden self-modification or unreviewed credential growth;
- a dashboard that turns opaque automation into “governance theatre”; or
- a promise that every problem should be automated.

It is an attempt to build a system that can do useful work while remaining able
to explain, limit, stop, and improve itself.

## The standard of progress

Progress is not measured by how autonomous Summa42 appears. It is measured by
whether a responsible person can answer, after any meaningful action:

1. What mission or obligation justified this?
2. What authority allowed it, in which domain, and until when?
3. What evidence supported the decision, and what remained unknown?
4. What did the system actually do, and where is the commitment boundary?
5. What did it cost, what exposure remains, and how can it be stopped or
   reversed?
6. What did it learn—and what did that learning *not* authorize?

When those questions have durable, inspectable answers, a Collective can grow
without pretending that growth is wisdom.
