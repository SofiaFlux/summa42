# Architecture Spike: Dapr Distributed Execution Seam and AgentLedger Conformance Patterns

**Issue:** [#60](https://github.com/SofiaFlux/summa42/issues/60)  
**Parent Epic:** [#3](https://github.com/SofiaFlux/summa42/issues/3) (*Slim Meeseek into a semantic control plane over mature OSS runtimes*)  
**Status:** Completed  
**Recommendation:** **ADAPT** (Adopt Dapr as an optional multi-Cube transport/activation carrier under the invariant "Meeseek decides; Dapr carries"; preserve SQLite local-first zero-dependency baseline; enforce AgentLedger reliability patterns)  
**Date:** 2026-11-10  

---

## 1. Executive Summary

This spike established the distributed execution seam for future multi-Cube expansion using **Dapr (Distributed Application Runtime)** as a transport and activation carrier, while validating all five canonical **AgentLedger** reliability and adversarial failure patterns.

The foundational design invariant is:
> **Meeseek decides; Dapr carries.**

Dapr provides commodity distributed primitives (pub/sub, actor routing, mTLS, service invocation). However, Dapr must **never** be granted authority over Meeseek's semantic runtime, task scheduling, fence generations, or external operation commit states.

---

## 2. Hard Semantic Boundary & Rejection Conditions

To protect the integrity of the Collective, three explicit rejection conditions were formalized and verified:

| Proposed Coupling | Rejection Condition | Enforced Invariant |
|---|---|---|
| Use Dapr Workflow as Task Scheduler | **REJECTED: Dapr workflow != Meeseek Task** | Meeseek retains sovereign ownership of the Task Graph, lease lifetimes, and verifier acceptance gates. |
| Use Dapr Actor as Attempt Lifecycle | **REJECTED: Dapr actor != Attempt lifecycle** | Attempt lifecycle is fenced and strictly bound to lease generations. Dapr actor activations are transient transport envelopes. |
| Use Dapr Automatic Retries for Tools | **REJECTED: Dapr retries != External Operation semantics** | Blind retries on unconfirmed external operations cause catastrophic duplicate side-effects. Retries on confirmed slots trigger `ErrDuplicateCommit`. |

---

## 3. Provider-Neutral Carrier Contract (`DistributedCarrier`)

In `internal/distributed/contract.go`, the provider-neutral execution seam is defined as:

```go
type DistributedCarrier interface {
    Name() string
    Available(ctx context.Context) bool
    Dispatch(ctx context.Context, env AttemptDispatchEnvelope) (DispatchAck, error)
    ReconcilePendingEffects(ctx context.Context, taskID domain.ID) ([]EffectReconciliationResult, error)
}
```

This interface enables:
- **`DaprCarrier` (`internal/distributed/dapr.go`):** Connects to Dapr sidecars via pub/sub or actor invocation for multi-Cube clustering.
- **`LocalFallbackCarrier` (`internal/distributed/dapr.go`):** Zero-dependency in-process carrier for local developer environments and single-Cube nodes, requiring zero Docker or Dapr overhead.

---

## 4. AgentLedger Conformance Verification

In `internal/distributed/conformance_test.go`, automated test suites adapted from AgentLedger failure modes verified the 5 critical reliability scenarios:

1. **`TestConformance_UnknownExternalResultReconcilesWithoutDuplicate`:**
   - **Pattern:** Network timeout leaves an external operation in `OUTCOME_UNKNOWN`.
   - **Result: PASS.** The reconciler contacts the external provider using the deterministic idempotency key, confirms execution, and marks the slot `CONFIRMED`. Subsequent dispatches reject duplicate execution with `ErrDuplicateCommit`.

2. **`TestConformance_UnresolvedBillingExposureBlocksOvercommit`:**
   - **Pattern:** An external operation is dispatched and remains unsettled.
   - **Result: PASS.** Unresolved financial exposure is counted against the task budget ceiling, blocking subsequent spending with `ErrBillingExposureBlocked` until reconciled.

3. **`TestConformance_StaleAttemptAndExpiredLeaseRejected`:**
   - **Pattern:** Attempt with an outdated fence generation or expired lease deadline attempts carrier dispatch.
   - **Result: PASS.** Stale fences are blocked with `ErrStaleFence`; expired leases are blocked with `ErrLeaseExpired`.

4. **`TestConformance_CrashBeforeCompletionRecordDoesNotFakeCompletion`:**
   - **Pattern:** Node crashes after workload execution but before the completion manifest is committed to SQLite.
   - **Result: PASS.** Upon recovery, the task remains in `EXECUTING` / incomplete state. No completion is faked.

5. **`TestConformance_ConfirmedEffectSurvivesAttemptReplacement`:**
   - **Pattern:** Attempt 1 commits an external migration effect slot, then fails before code review.
   - **Result: PASS.** Replacement Attempt 2 inherits and reuses the existing confirmed effect slot, resulting in zero duplicate side-effects.

---

## 5. Complexity Accounting: Dapr vs In-Process SQLite

| Dimension | Single-Cube SQLite (Current Baseline) | Dapr Distributed Carrier (Multi-Cube) |
|---|---|---|
| **Operational Dependencies** | Zero (pure Go + embedded SQLite) | Dapr sidecar (daprd), Redis/NATS pubsub, placement service |
| **Startup Overhead** | Instantaneous (<10 ms) | Requires sidecar initialization (~1–2 s) |
| **Cluster Scaling** | Single host / shared storage | Multi-host, multi-Cube peer routing |
| **Network Security** | Local IPC / UNIX domain sockets | Mutual TLS (mTLS) and SPIFFE identities |
| **Idempotency & Fencing** | In-database atomic SQLite transactions | Carrier dispatch mediated by Meeseek fence checks |

---

## 6. Recommendation: ADAPT

### Decision: **ADAPT**
1. Standardize on the provider-neutral `DistributedCarrier` contract (`internal/distributed/contract.go`).
2. Maintain `LocalFallbackCarrier` as the default runtime for single-Cube operation, keeping local development lean and dependency-free.
3. Enable `DaprCarrier` strictly as an optional multi-Cube activation transport.
4. Enforce the hard rule: **Dapr carries messages; Meeseek owns state, leases, fencing, and idempotency.**
