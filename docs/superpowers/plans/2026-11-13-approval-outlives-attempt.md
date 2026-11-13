# Owner approval outlives the Attempt that asked for it

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** an Owner can approve a held external operation (publication) hours after it was requested, without chasing a new approval ID every ~11 minutes, while keeping every guarantee the approval gate gives today.

**Status:** plan, revised after one independent review (Claude Opus via agy, 2026-11-13: REVISE, 2 high and 3 medium, all taken in below). This is the "asynchronous human approval continuation beyond lease expiry" left as future work by the ruling in `2026-10-08-github-publication.md`, and it reverses K-M11 ("an approval expires with the lease", `2026-10-21-review-remediation.md` Task 21, README line 312).

## Evidence (sandbox, 2026-11-13)

Case `case_72bde829…` (SofiaFlux/summa42-sandbox#1), PR publication task `task_086e1169…`, effect slot `effectslot_b0696903…`:

- 11 approval requests in 2 h, **all with the same `request_digest`** `26dbd963…`, each with a ~10-minute expiry.
- They belong to operations with sequence 1–11 of that **one effect slot**: 1–10 `CANCELLED`, 11 `CONFIRMED_EFFECT`. Each operation has its own Attempt.
- The Owner missed two windows. PR #2 was opened only on the third try.

Cause: `operations.Service.PrepareWithGuard` (`internal/operations/service.go`, the previous-operation switch) cancels the previous `PREPARED` operation of the slot when the Attempt differs, and `prepareApprovalInTx` (`internal/operations/approval_gate.go`) makes a new approval for the new operation with `ExpiresAt = guarded.LeaseExpiresAt`. The approval's subject is the operation, and the operation dies with its Attempt.

What the Owner signs, `OperationApprovalDigest`, covers provider, descriptor type, effect slot, intent fingerprint, intent revision and canonical intent hash. **It names neither the operation nor the Attempt.** So the signature already means "this exact effect in this slot". Only the request row is tied to something short-lived.

## Decision

**Recommended (confidence: high on direction, medium on the exact seam until the failing test runs):** a new operation in the same effect slot **adopts** the slot's live approval instead of making a new one. The approval gets its own Owner-facing lifetime instead of the lease.

Rejected:
- *Longer TTL only*: each new Attempt still mints a new operation and a new approval. It changes nothing.
- *`summa42 approve --case`*: hides an ID that still dies every 11 minutes. It is a stopgap.
- *Approval subject = effect slot (schema change)*: matches the digest, but `UNIQUE(subject_kind, subject_id, request_digest)` together with the no-replace trigger forbids a second request for the same slot and digest after an expiry or rejection. It also changes `control/server.go` (loads the operation by `SubjectID`) and both CLIs. That is more change for the same result.
- *Re-fence the cancelled operation to the new Attempt*: touches the dispatch fencing path, which is the riskiest code in the Box.

Adoption keeps `subject_id` = the operation that first asked. The schema does not change.

## Invariants (each one gets a test)

1. One signature dispatches at most one effect. `ConsumeInTx` moves `APPROVED` to `CONSUMED`, and a consumed, rejected or expired approval is never adopted.
2. A different intent needs a new approval. The first gate is the slot itself: `ResolveEffectSlot` refuses a different fingerprint in an existing slot (`ErrIntentConflict`). Adoption's equal-digest check is defense in depth. It also catches equal fingerprint with different canonical JSON, which would be a canonicalization bug.
3. A changed policy or set of required approvers invalidates the approval. Adoption needs `requiredApproversFor(current decision)` to equal the record's approvers, as `preparedApprovalMatchesInTx` checks today.
4. Expiry stays bounded. `ExpiresAt = now + ApprovalTTL`, with a default of 24 h and a hard cap of 7 days in config. It is not unbounded. `expires_at` is immutable (trigger `approval_requests_binding_immutable`, migration 00012), so adoption never extends it. An approval with less than `minAdoptionRemaining` left (fixed at 15 min) is not adopted, and a fresh one is made, so the Owner is never shown a request that dies before they can act.
5. Adoption stays within the slot. The subject operation must have the same `effect_slot_id` (and so the same task). No approval crosses tasks or cases.
6. Rejection is not sticky per slot. This is unchanged: a `REJECTED` approval is not adopted, so the next Attempt asks again, as today.
7. Two Attempts adopting the same approval is benign. A live lease fences out a second Attempt, and even past that `ConsumeInTx` is a compare-and-swap from `APPROVED` to `CONSUMED`, so only one dispatches. The loser's next Prepare sees `CONSUMED` and makes a new approval.

## Tasks

### Task 1: failing test for the real-life sequence
- [ ] In `internal/ghtriage/publication_test.go` add `TestPublicationApprovalSurvivesLeaseExpiry`:
  1. publish until held and capture `first.ApprovalID`;
  2. advance the fake clock past the Attempt lease and recover the lease (as serve does);
  3. publish again: assert `Held` with **the same** `ApprovalID`, and that `approval_requests` still has 1 row;
  4. approve it and publish again: assert exactly one PR write;
  5. before approving, recover and publish a third time: the same approval is re-adopted (idempotent);
  6. publish again after dispatch: assert no second write, and that the approval is `CONSUMED`.
- [ ] Run it and see it fail at step 3 (a new approval ID).

### Task 2: approval lifetime is its own
- [ ] Add `ApprovalTTL time.Duration` to `operations.Service` construction. The default is 24 h. Validate it against a cap of 7 days. Wire it from serve config or flags (`--approval-ttl`).
- [ ] `prepareApprovalInTx`: `ExpiresAt = now + ApprovalTTL` instead of `guardedLeaseExpiry`.
- [ ] Test: an approval created by an Attempt with a 10-minute lease expires after the TTL, not with the lease.

### Task 3: adopt the slot's live approval
- [ ] In `PrepareWithGuard`, before `prepareApprovalInTx` makes a new request, look up the most recent approval in this slot: the newest operation of `slot.ID` with a non-null `approval_id`. Adopt it when the state is `PENDING` or `APPROVED`, `expires_at > now`, the digest equals the new digest and the required approvers are equal. Otherwise make a new one as today.
- [ ] `preparedApprovalMatchesInTx` and `validateAndConsumeApprovalInTx`: replace `record.SubjectID != op.ID` with "the subject operation exists and has `effect_slot_id = op.EffectSlotID`". Keep every other check.
- [ ] Adoption floor: do not adopt when `expires_at - now < minAdoptionRemaining`. Test: an approval 10 minutes from expiry is not adopted.
- [ ] Tests for invariants 1, 2 (same fingerprint, different canonical JSON → no adoption), 3, 5, 6 and 7 (the loser of a consume race gets a new approval): a consumed approval is not adopted; a new candidate SHA gets a new approval; a policy change gets a new approval; another task's slot never adopts; a rejected approval is not adopted.

### Task 4: what the Owner sees
- [ ] `control/server.go` `approvalDTO` loads the operation by `SubjectID`. After adoption that is a `CANCELLED` operation. Its intent equals the live one by digest, but its state misleads. Load the latest operation of that operation's effect slot that carries this `approval_id`, and fall back to the subject. Test: the DTO of an adopted approval shows the live `PREPARED` operation.
- [ ] `summa42 approvals` lists an adopted approval once. It is keyed by approval, so check that it is not duplicated.
- [ ] `casereport`: an approval can now span several operations. Count it with `DISTINCT a.approval_id` in `Interventions` (the `approval_decisions` join) and in `readPending` (PR #190).
- [ ] README lines 164 and 312: replace "within the live ten-minute lease" and "an approval expires with its lease" with the new rule.

### Task 5: verification
- [ ] `make verify`; `go test -race ./internal/operations ./internal/ghtriage -timeout 15m`.
- [ ] Sandbox: label a second issue, let it be held, approve after more than 15 minutes, and see one PR.
- [ ] Independent review pass by another model before merge, since this is governance core.

## Follow-up (not in this plan)

A held publication is logged as `pipeline pass: 1 stage(s) failed` and keeps cycling Attempts every ~11 minutes. Holding is a normal wait state. Parking the task without a live lease until the approval is decided would stop the cycling at its source and make adoption less important. It is a separate change in the execution and lease layer.
