# Safety controls and vision alignment implementation plan

> **For agentic workers:** native execution chosen under owner/AGENTS.md
> delegation; the Owner reviews the resulting draft pull request.

**Goal:** Make the emergency controls reachable and effective, stop untrusted
comments from moving issue revisions, park code no running path executes, and
align the vision and documentation with the Owner's destination.

**Architecture:** Owner-signed CLI commands writing canonical state directly; a
lease gate checked inside the leasing transaction; lease revocation through
`execution`; intake revision derived from maintainer activity plus recorded
cases.

**Tech Stack:** Go 1.27, existing SQLite and goose migrations, Ed25519, cobra.

**Spec:** docs/superpowers/specs/2026-11-10-safety-controls-and-vision-alignment-design.md

## Global constraints

- No new dependency. No change to the policy, approval or operation commit
  boundary.
- An emergency control never widens authority; `resume` and `heartbeat` require
  the Owner signature.
- Applied migrations are never edited; new behavior uses new migrations.
- Tests first; each task ends green and committed.

## Review focus

- A freeze and a lease must not interleave: the gate runs in the leasing
  transaction.
- A signed command must not be replayable, and a stale one must be refused.
- Reopening the state must not change the Owner's horizon.
- An untrusted comment must never regress an issue to an older revision.
- Removing packages must not remove an applied migration or a wired import.

### Task 1: Dead man horizon

Files: internal/emergency/service.go, types.go, service_test.go;
internal/runtime/box.go, box_test.go;
internal/state/sqlite/migrations/00035_dead_man_owner_horizon.sql.

- [x] Tests: default is seven days; reopening with another lease duration keeps
  the stored timeout; `attempt_lease` and `external_operation` are refused in
  safe mode; RED.
- [x] `EnsureDefaults` inserts once; default constant; migration; runtime passes
  no derived value; GREEN.

### Task 2: Lease gate and audited revocation

Files: internal/execution/service.go, lease_gate_test.go;
internal/emergency/service.go, service_test.go; internal/containment/service.go;
internal/runtime/box.go, box_test.go.

- [x] Tests: `StartAttempt` refused while FROZEN and in safe mode, allowed after
  resume; break glass appends `LEASE_REVOKED` per Attempt; RED.
- [x] `LeaseGate`, `RevokeActiveLeasesTx`, `RevokeLeaseTx`;
  `CheckLeaseAllowedTx`; wire in runtime; GREEN.

### Task 3: Owner-signed emergency commands

Files: internal/emergency/owner_command.go, owner_command_test.go;
internal/state/sqlite/migrations/00036_emergency_audit_signature.sql;
cmd/summa42/emergency_cmd.go, emergency_cmd_test.go, root.go.

- [x] Tests: valid command applies and stores the signature; wrong key, stale
  time, replayed event ID and unknown action are refused; CLI freeze, status,
  resume, heartbeat with `--timeout`; RED.
- [x] Implement and register; GREEN.

### Task 4: Maintainer-only issue revisions

Files: internal/ghissue/client.go, client_test.go, source.go, observe.go,
observe_test.go; cmd/summa42-box/main.go.

- [x] Tests: outsider comment keeps the recorded revision; maintainer comment
  moves it; first sight keeps `updated_at`; bot rule unchanged; RED.
- [x] Client flags the untrusted tail; observer resolves the revision; GREEN.

### Task 5: Park spikes

Files: remove internal/{mission,decision,dtu,gateway,epistemic,replay,delegation},
internal/evidence/{lineage.go,acceptance.go,acceptance_test.go},
internal/teb/{openshell.go,openshell_test.go},
internal/executors/{maf.go,maf_test.go}; edit internal/executors/executor.go;
add docs/superpowers/research/2026-11-10-parked-spikes.md.

- [ ] Index with issue, pull request, commits and restore command per item.
- [ ] Remove; build, vet and full tests green.

### Task 6: Company OS library fixes

Files: internal/runtime/box.go, box_test.go;
internal/state/sqlite/migrations/00037_default_envelope_unfunded.sql;
internal/obligation/service_test.go.

- [ ] Tests: `Box.Planner.PlanNext` reaches the scheduler; default envelope
  limit is zero after migration; RED.
- [ ] Construct the planner after the scheduler; migration; GREEN.

### Task 7: Vision and documentation

Files: docs/VISION.md, docs/COMPANY_OS.md, README.md,
docs/superpowers/plans/2026-10-07-self-development-roadmap.md,
docs/incidents/2026-10-21-canned-model-reviews.md.

- [ ] Write; `python3 scripts/verify_docs.py` on every changed Markdown file.

### Task 8: Delivery

- [ ] Validation matrix from CONTRIBUTING.md.
- [ ] Push; draft pull request; reopen affected epics; comment on PR #176.

## Decisions

See the spec. Suite tests that need a non-root user are skipped when run as
root, with the reason in the skip message.
