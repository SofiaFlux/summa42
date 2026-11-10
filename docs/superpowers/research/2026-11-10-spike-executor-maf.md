# Architecture Spike: Evaluation of Executor Abstraction and Microsoft Agent Framework (MAF) Integration

**Issue:** [#58](https://github.com/SofiaFlux/summa42/issues/58)  
**Parent Epic:** [#3](https://github.com/SofiaFlux/summa42/issues/3) (*Slim Meeseek into a semantic control plane over mature OSS runtimes*)  
**Status:** Completed  
**Recommendation:** **ADAPT** (Standardize provider-neutral `Executor` interface; treat MAF as unprivileged guest execution engine; preserve Meeseek's independent acceptance gate and capability mediation)  
**Date:** 2026-11-10  

---

## 1. Executive Summary

This architecture spike evaluated the provider-neutral `Executor` boundary underneath a Meeseek Attempt and assessed the integration of **Microsoft Agent Framework (MAF)** (the convergence of Semantic Kernel and AutoGen multi-agent runtime patterns).

The primary question addressed:
> **Can Meeseek leverage mature agent frameworks like MAF for workflow execution without surrendering semantic sovereignty over Task acceptance, capability mediation, or attempt lifecycle?**

The verdict is an emphatic **YES**, provided that a strict semantic boundary is enforced:
1. **Executor-internal completion != Meeseek Task acceptance.** An agent reporting `status: COMPLETED` or outputting self-congratulatory text does not satisfy a Meeseek Task. Independent verification gates evaluate objective criteria before acceptance.
2. **Capability Mediation is mandatory.** External agent SDKs cannot perform ambient tool execution. All external operations must pass through Meeseek's mediated capability gateway.
3. **Cancellation must be clean.** Attempt context cancellations must terminate guest agent workflows immediately without leaving zombie processes or runaway background loops.
4. **Epistemic proposals remain non-authoritative.** Executors may output proposed `KnowledgeDelta` objects, but they have zero authority to directly mutate canonical knowledge.

---

## 2. Minimal Provider-Neutral `Executor` Interface

In `internal/executors/executor.go`, the core execution boundary is formalized as:

```go
type Executor interface {
    Start(ctx context.Context, envelope AttemptEnvelope) (ExecutionResult, error)
}
```

With `ExecutionResult` enriched to capture canonical evidence and epistemic proposals:
```go
type ExecutionResult struct {
    ExitCode      int
    Stdout        string
    Stderr        string
    Evidence      []Evidence
    Usage         Usage
    ProposedDelta *epistemic.KnowledgeDelta
}
```

Key guarantees:
- **`AttemptEnvelope`:** Contains only Attempt-relevant inputs (`TaskID`, `AttemptID`, `Objective`, `Workspace`, `VisibleCapabilities`, `AcceptanceCriteria`).
- **Canonical Evidence Collection:** Captures agent messages (`EvidenceAgentMessage`) and mediated command calls (`EvidenceCommand`).
- **Proposed Epistemic Deltas:** Encapsulates newly observed facts or claims as a candidate `epistemic.KnowledgeDelta` awaiting evaluation by epistemic consolidation engines.

---

## 3. Microsoft Agent Framework (MAF) Adapter Architecture

In `internal/executors/maf.go`, the `MAFAdapter` wraps multi-agent workflows under Meeseek Attempt governance:

```
  ┌────────────────────────────────────────────────────────┐
  │                 Meeseek Attempt Envelope               │
  │  (TaskID, AttemptID, Objective, VisibleCapabilities)   │
  └───────────────────────────┬────────────────────────────┘
                              │ Start(ctx, envelope)
                              ▼
  ┌────────────────────────────────────────────────────────┐
  │                      MAFAdapter                        │
  │  ┌──────────────────────────────────────────────────┐  │
  │  │         MAF Multi-Agent Workflow Engine          │  │
  │  │   (PlannerAgent -> CoderAgent -> ReviewerAgent)  │  │
  │  └────────────────────────┬─────────────────────────┘  │
  │                           │ tool invocation            │
  │                           ▼                            │
  │             Capability Mediation Check                 │
  │    (Is capability in envelope.VisibleCapabilities?)    │
  └───────────────────────────┬────────────────────────────┘
                              │ mediated call
                              ▼
  ┌────────────────────────────────────────────────────────┐
  │         Summa42 Capability Mediation Gateway           │
  │        (PREPARED -> DISPATCHED -> Verified)            │
  └────────────────────────────────────────────────────────┘
```

### Key Invariant Validations:
- **`TestMAFAdapter_Invariant_WorkflowSuccessNotEqualTaskAcceptance`:**
  Proves that when MAF reports `status: COMPLETED`, the independent verification evaluator (`IndependentTaskAcceptanceEvaluator`) rejects the task if required acceptance criteria (e.g. cryptographic audit signatures) are absent.
- **`TestMAFAdapter_CancellationCleanlinessWithoutOrphans`:**
  Proves that cancelling the attempt context halts the workflow within milliseconds and terminates background loops without leaking goroutines or processes.
- **`TestMAFAdapter_CapabilityMediationEnforcement`:**
  Proves that visible capabilities route through the mediator, invisible capabilities are blocked with `ErrCapabilityDenied`, and ambient SDK executions without a mediator fail closed with `ErrCapabilityUnmediated`.
- **`TestMAFAdapter_ProducesProposedKnowledgeDeltaWithoutDirectStateMutation`:**
  Proves that proposed epistemic claims are cleanly packaged in `ExecutionResult.ProposedDelta` without altering the canonical database.

---

## 4. Complexity Accounting: Dependencies vs Value

| Dimension | Native Meeseek Shell / OCI | Microsoft Agent Framework (MAF) |
|---|---|---|
| **Dependency Weight** | Standard Go library / minimal runtime | Heavy SDK footprint (C#/.NET, Python, or Go wrapper; multi-MB dependencies) |
| **Workflow Flexibility** | Linear or graph-based task dispatch | Native multi-agent loops, reflection, and conversational handoffs |
| **Tool Calling Overhead** | Direct JSON/stdio capability protocol | Opinionated function calling schemas requiring adaptation |
| **Execution Sandboxing** | Tightly integrated with TEB (Landlock/OCI) | Assumes ambient host access unless explicitly mediated |
| **Maintainability** | 100% owned, deterministic Go code | Bound to Microsoft SDK deprecation and release cycles |

---

## 5. Recommendation: ADAPT

### Decision: **ADAPT**
- **Do NOT Adopt MAF as Core Runtime (REJECT hard adoption):**
  Mandating MAF would compromise Summa42's lean, zero-dependency Go architecture and violate the local-first, minimal-complexity mandate of Ponytail principles.
- **ADAPT the Architecture:**
  1. Standardize on the provider-neutral `Executor` interface (`internal/executors/executor.go`).
  2. Implement adapters (like `MAFAdapter`) to enable external agent teams to run inside Meeseek Attempts as unprivileged guest runtimes.
  3. Enforce the hard semantic wall: **No external framework completion status can ever bypass Meeseek's independent task acceptance verification.**
  4. Ensure all external capability interactions are strictly mediated through Meeseek's capability boundary.
