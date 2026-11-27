# Project Scope Design Specification (`M2-T1`)

**Card:** `M2-T1` (`DESIGN`) — [development plan](../plans/2026-11-11-development-plan.md#m2-projects)
**Date:** 2026-11-27
**Status:** Proposed for Owner approval (starts no `M2-T2..T7` build card until approved)
**Vision & Plan Alignment:** [Vision](../../VISION.md) (Articles 3, 4, 5, 6, 15, 20, 22, 27), [Plan Section 2.1](../plans/2026-11-11-development-plan.md#21-three-scopes-one-kernel)

---

## 1. Purpose and Scope

Today Summa42 holds **one** active Mission for the entire Collective (`missions_one_active` in `internal/state/sqlite/migrations/00002_purpose.sql`), a flat set of `resource_envelopes` (`00005_resources.sql`) with no parent enforcement in `internal/resources`, Collective-wide emergency controls (`00028_emergency_controls.sql`), and ~20 CLI flags across `run-gh-intake`, `run-gh-pipeline`, and `run-worker`.

This specification defines **Project** as a first-class kernel scope under the Collective (never above it):

```text
Collective (one Owner, one Box, one SQLite database, Collective root envelope, global Break Glass & Dead Man Switch)
  └── Project (slug, ACTIVE|PAUSED|ARCHIVED, child resource envelope, signed manifest hash)
        ├── Mission (at most one active Mission per Project)
        ├── Sources (GitHub & Azure DevOps repositories declared in the Project manifest)
        ├── Grants & standing delegations (scoped to the Project; never widening Collective policy)
        ├── Board (board_cards scoped via Mission)
        └── Cases, Tasks, Attempts, Evidence, External Operations
```

A Project is a **narrowing scope, never an independent authority source**. Every rule settled here preserves a single-Project Collective (`default`) whose observable behavior is identical to `main` today.

---

## 2. Schema and Migration Design (`M2-T2` / `M2-T3` / `M2-T4`)

> **Migration numbering note:** The development plan text for `M2-T2` mentions `00038_projects.sql`, written before `00038_envelope_grants.sql` and `00039_notification_outbox.sql` landed on `main`. Per Rule R3, `M2-T2` uses the next free sequential migration number when its branch is cut (`00040_projects.sql` against current `main`).

### 2.1 The `projects` table and audit tables

Added in `M2-T2` (`00040_projects.sql`):

```sql
CREATE TABLE projects (
    project_id       TEXT PRIMARY KEY,
    slug             TEXT NOT NULL UNIQUE,
    name             TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('ACTIVE', 'PAUSED', 'ARCHIVED')),
    envelope_id      TEXT NOT NULL UNIQUE REFERENCES resource_envelopes(envelope_id),
    manifest_sha256  TEXT NOT NULL DEFAULT '',
    autonomy_step    TEXT NOT NULL DEFAULT 'A1' CHECK (autonomy_step IN ('A0', 'A1', 'A2', 'A3')),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    archived_at      TEXT
);

CREATE TABLE project_events (
    event_id            TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(project_id),
    action              TEXT NOT NULL CHECK (action IN ('CREATE', 'PAUSE', 'RESUME', 'ARCHIVE', 'APPLY_MANIFEST')),
    from_state          TEXT NOT NULL DEFAULT '',
    to_state            TEXT NOT NULL,
    manifest_sha256     TEXT NOT NULL DEFAULT '',
    owner_principal_id  TEXT NOT NULL REFERENCES principals(principal_id),
    reason              TEXT NOT NULL,
    issued_at           TEXT NOT NULL,
    signature           BLOB NOT NULL,
    created_at          TEXT NOT NULL
);

CREATE INDEX project_events_project_created ON project_events(project_id, created_at);
```

- `slug` matches `^[a-z0-9][a-z0-9-]{1,47}$` (`default` is reserved for the adopted default Project).
- `project_events` is append-only (protected by immutable `BEFORE UPDATE` / `BEFORE DELETE` SQLite triggers matching `00028_emergency_controls.sql`), stores every Owner Ed25519 signature + nonce (`event_id`) with 5-minute replay/freshness checks (`GrantMaxAge` / `OwnerCommand`), and records state changes (`M2-T2`, `M2-T4`) plus manifest hashes (`M2-T3`).
- `autonomy_step` records the per-Project autonomy ladder (`Plan Section 2.3`, initialized to `'A1'`). It is informational governance metadata; it never bypasses policy, approvals, or resource checks.

### 2.2 Direct `project_id` vs. Mission-reached lineage

All durable tables fall into one of four explicit ownership classes:

| Ownership class | Tables | Schema mechanism & rationale |
| --- | --- | --- |
| **1. Direct `project_id` (`NOT NULL`)** | `projects`, `project_events`, `missions` | Anchor entities of a Project. `missions` gains `project_id TEXT NOT NULL DEFAULT 'project_default' REFERENCES projects(project_id)`. |
| **2. Direct nullable `project_id`** | `resource_envelopes`, `obligations`, `owner_decision_packets`, and future `owner_action_records` (`M4-T1`) | Entities that exist at **either** Collective scope (`project_id IS NULL`: `envelope_collective_root`, `envelope_default`, Collective obligations/maintenance, pre-M2 test envelopes) **or** Project scope (`project_id = <id>`). Leaving `project_id` nullable on `resource_envelopes` keeps all existing unit tests inserting `(envelope_id, hard_limit, created_at)` intact while letting Project child envelopes enforce Project binding. |
| **3. Reached through `Mission` (or `Task -> Purpose`) — no duplicate column** | **Via `mission_id`:** `workflow_cases`, `workflow_assessments`, `workflow_verifications`, `board_cards`, `board_card_moves`, `goals` (when `purpose_kind = 'MISSION'`).<br>**Via `task_id -> purpose`:** `tasks`, `task_dependencies`, `attempts`, `task_challenges`, `attempt_failures`, `execution_events`, `attempt_run_manifests`, `attempt_containments`, `attempt_containment_events`, `external_operations`, `effect_slots`, `approval_requests`, `approval_decisions`, `capability_sessions`, `capability_assessments`, `evidence_objects` / `evidence_subjects`, `model_calls` (when `case_id` / `task_id` present), `github_issue_triage_reviews`, `foreign_evidence_lineage`, `external_task_reconciliations`. | Avoids denormalized `project_id` drift. A Task's owning Project is resolved deterministically inside any kernel SQLite transaction via: `MISSION` -> `missions.project_id`, `OBLIGATION` -> `obligations.project_id`, and `NULL` for Collective-level `COLLECTIVE_MAINTENANCE` / `GOVERNANCE` / `RECOVERY`. (Non-Mission Project tasks such as Project maintenance/directives may resolve via an optional `tasks.project_id` added if needed later, or stay under an active Mission/Obligation.) |
| **4. Collective-global (no `project_id`)** | `principals`, `constitutions`, `collective_metadata`, `policy_sets`, `emergency_controls`, `dead_man_switch`, `emergency_audit_events`, `cube_quarantines`, `owner_attention_budgets`, `audit_events`, `notification_outbox`, `capability_definitions`, `experience_*`, `field_*`, `sanitized_feedback`, `company_*`, `business_invariants` | Collective constitutional roots, host identity, global break-glass/dead-man switch, global policy/quarantine/audit chain, and Company OS cross-project state (`Plan Section 2.1`). |

---

## 3. Default Project Adoption & One Active Mission per Project

### 3.1 One active Mission per Project (`M2-T2`)

Migration `00040_projects.sql` replaces the Collective-wide unique index in `00002_purpose.sql`:

```sql
DROP INDEX IF EXISTS missions_one_active;

ALTER TABLE missions
    ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default' REFERENCES projects(project_id);

CREATE UNIQUE INDEX missions_one_active_per_project
    ON missions(project_id)
    WHERE active = 1;

CREATE INDEX missions_project_created ON missions(project_id, created_at);
```

- `internal/purpose/service.go`:
  - `CreateMissionForProject(ctx, projectID, statement)` (and updated `CreateMission(ctx, projectID, statement)` / `ActiveMission(ctx, projectID)`): checks inside the transaction that `projectID` exists and its `state` is `ACTIVE` (rejecting `PAUSED` and `ARCHIVED` Projects for new active Mission creation), defaulting empty `projectID` only in compatibility helpers mapped to `domain.DefaultProjectID ("project_default")`.
  - `cmd/summa42/mission_cmd.go` accepts `--project <slug|id>`, defaulting to `default` (`project_default`) when omitted, preserving `summa42 mission create <statement>` and `summa42 mission show` unchanged for single-Project users.
  - `validatePurpose` in `internal/purpose/service.go` keeps validating `active = 1` on `missions`; leasing (`execution`) additionally gates on Project `state == 'ACTIVE'`.

### 3.2 Backfill and Adoption Rules in Migration `00040_projects.sql`

When upgrading any existing database (whether created by `summa42 init` or a `testutil.OpenStore` test fixture):

1. **Collective root envelope (`envelope_collective_root`)**:
   - Inserted via `INSERT OR IGNORE` if absent, with `parent_envelope_id = NULL`, `project_id = NULL`.
   - To avoid silently inventing new budget (`00037` / R6) or under-funding already Owner-granted envelopes on upgrade, its initial `hard_limit` is set to:
     `COALESCE((SELECT SUM(hard_limit) FROM resource_envelopes WHERE envelope_id <> 'envelope_collective_root' AND parent_envelope_id IS NULL), 0)`.
     On a freshly initialized Collective (or after `00037` zeroed `envelope_default`), this sum is `0`.
   - `bootstrap.Service.Init` (`summa42 init`) deterministically seeds both `envelope_collective_root` (`hard_limit = 0`, `parent_envelope_id = NULL`) and `project_default` (`slug = 'default'`, `state = 'ACTIVE'`, `envelope_id = 'envelope_project_default'` with `hard_limit = 0`, `parent_envelope_id = 'envelope_collective_root'`).
2. **Default Project (`project_default`)**:
   - Pre-existing rootless non-root envelopes (other than `envelope_collective_root`) are re-parented under `envelope_collective_root`:
     `UPDATE resource_envelopes SET parent_envelope_id = 'envelope_collective_root' WHERE envelope_id <> 'envelope_collective_root' AND parent_envelope_id IS NULL`.
   - `envelope_project_default` is created via `INSERT OR IGNORE` at `hard_limit = 0` under `envelope_collective_root` (`project_id = 'project_default'`) and linked as `projects.envelope_id` for `project_default`, while existing pre-M2 envelopes remain usable children of `envelope_collective_root` (so in-flight cases referencing `--envelope <id>` keep working without manual re-wiring and no pre-M2 limits change).
3. **Existing Mission, cases, and board**:
   - Every existing row in `missions` receives `project_id = 'project_default'` via the column `DEFAULT` (covering both active and deactivated Missions).
   - Every existing row in `workflow_cases`, `workflow_assessments`, `workflow_verifications`, `board_cards`, `board_card_moves`, `goals`, and `tasks` linked through those `mission_id`s is automatically adopted by `project_default` with zero row copies or ID rewrites.

---

## 4. Resource Envelope Hierarchy (`M2-T1`, `M2-T4`)

`00005_resources.sql` already defines `parent_envelope_id REFERENCES resource_envelopes(envelope_id)`, currently unenforced by `internal/resources`.

### 4.1 Hierarchy structure

```text
envelope_collective_root   (parent = NULL, hard_limit set by Owner via `summa42 envelope set-limit`)
  ├── envelope_default               (parent = envelope_collective_root, hard_limit = 0 unless funded)
  ├── envelope_project_default       (parent = envelope_collective_root, hard_limit = 0 until `project fund`)
  ├── envelope_project_<slug>        (parent = envelope_collective_root, hard_limit = 0 until `project fund`)
  └── (optional sub-envelopes of a Project, parent = project.envelope_id)
```

- Creating a Project (`summa42 project create <slug> ...`, Owner-signed) atomically creates its child envelope (`envelope_project_<slug>`, `parent_envelope_id = 'envelope_collective_root'`, `project_id = project.project_id`, `hard_limit = 0`) in the same transaction. It **cannot** allocate a positive `hard_limit` during creation.
- `summa42 project fund <slug> --limit N --reason "..."` issues an Owner-signed grant (`resource_envelope_grants`) updating `project.envelope_id`'s `hard_limit` to `N`.

### 4.2 Parent-child accounting invariant (two-layer enforcement)

To uphold **Vision Article 15** (budgets are hard boundaries) and **Vision Article 20** (no cross-Project starvation):

1. **Allocation ceiling (strict tree partitioning in `ApplyOwnerGrant`):**
   - For any parent envelope `P`:
     `direct_committed(P) + SUM(child.hard_limit WHERE child.parent_envelope_id = P) <= P.hard_limit`.
   - Raising a Project's `--limit N` fails in SQLite tx if it would push `SUM(children.hard_limit) + direct_committed(root) > root.hard_limit`.
   - Lowering a parent envelope (`SET_LIMIT` on `envelope_collective_root`) fails if the new limit is below `direct_committed(root) + SUM(children.hard_limit)`.
   - Thus a funded Project is guaranteed its own committed slice cannot be spent out from under it by another Project (`TestProjectCannotSpendBeyondItsEnvelope`).
2. **Runtime reservation & renewal check (`ReserveInTx`, `AllowRenewalInTx`):**
   - Walk ancestors from `envelopeID` up to root (`parent_envelope_id`, bounded depth `<= 8` against cycles): for the leaf `envelopeID`, apply the existing check (`availableIn` / `AllowRenewalInTx`), and for every non-nil ancestor `A` verify that `direct_committed(A) + SUM(direct_children committed/reserved) <= A.hard_limit`.
   - Because child allocations already partition parent limit under rule (1), healthy partitioned envelopes pass cleanly; ancestor verification provides fail-closed safety even if settled amounts overshoot a child reservation (`over_settled` in `00023`).
   - Envelopes without a parent (`parent_envelope_id IS NULL`, outside `envelope_collective_root`) retain identical single-envelope behavior.

---

## 5. Project Lifecycle States & Refusal Semantics (`ACTIVE`, `PAUSED`, `ARCHIVED`)

### 5.1 State machine

| Transition | Command (Owner-signed) | Allowed from | Effect in same SQLite transaction |
| --- | --- | --- | --- |
| `CREATE` | `summa42 project create <slug>` | *(new)* | Inserts `projects` row (`state = 'ACTIVE'`), child envelope (`hard_limit = 0`), and `project_events` signature record. |
| `PAUSE` | `summa42 project pause <slug> --reason ...` | `ACTIVE` | Sets `state = 'PAUSED'`, appends `project_events`, and immediately **revokes every `ACTIVE` Attempt lease** belonging to that Project via `execution.RevokeProjectLeasesTx` (appending `LEASE_REVOKED` execution events with reason `project_paused`). |
| `RESUME` | `summa42 project resume <slug> --reason ...` | `PAUSED` | Sets `state = 'ACTIVE'`, appends `project_events`. **Never** restores previously revoked Attempt leases (`TestResumeDoesNotRestoreRevokedLeases`); eligible interrupted work must be scheduled on a fresh Attempt. Fails if Project is `ARCHIVED`. |
| `ARCHIVE` | `summa42 project archive <slug> --reason ...` | `ACTIVE`, `PAUSED` | **Refuses** if the Project has any `ACTIVE` Attempt lease or any external operation in `DISPATCHED` or `OUTCOME_UNKNOWN` state (Vision Article 22: ambiguous outcomes must be reconciled first). Otherwise sets `state = 'ARCHIVED'`, `archived_at = now`, deactivates active Mission (`active = 0`), and appends `project_events`. Terminal: cannot transition back to `ACTIVE` or `PAUSED`. |

### 5.2 Per-state kernel enforcement matrix

| Action / Surface | `ACTIVE` | `PAUSED` | `ARCHIVED` |
| --- | --- | --- | --- |
| **Attempt lease (`StartAttempt`) & `RenewLease`** | Allowed if Collective `emergency` + envelope allow | **Refused** inside tx (`ErrProjectPaused`) | **Refused** inside tx (`ErrProjectArchived`) |
| **External operations (`Prepare`, `Dispatch`)** | Allowed if Collective `emergency`, quarantine & approval allow | **Refused** inside **Prepare and Dispatch transactions** (`ErrProjectPaused`) | **Refused** inside tx (`ErrProjectArchived`) |
| **Capability Session (`NewSession`, `Invoke`)** | Allowed if Collective `emergency` & policy allow | **Refused** (`ErrProjectPaused`) | **Refused** (`ErrProjectArchived`) |
| **`wake.RecoverExpiredLeases` & read-only op reconciliation (`Reconcile` / `LookupOutcome`)** | Allowed | **Allowed** (durable state recovery continues per Vision Articles 9 & 22) | **Allowed** (reconciliation only) |
| **Intake / case creation (`workflowcase`, `ghissue`)** | Allowed | **Refused / paused** (`serve` skips tick; direct calls rejected when Project != `ACTIVE`) | **Refused** |
| **Owner `project fund`, `project apply`, `approve`/`reject`** | Allowed | `fund`/`apply`/`reject` allowed; `approve` records decision while `Dispatch` stays blocked until `resume` | **Refused** (`project fund` & `apply` refused; pending approvals cannot dispatch) |
| **`serve` round-robin scheduling (`M2-T5`)** | Served (requires applied manifest) | Skipped (`TestPausedProjectRefusesLeaseOtherProjectContinues`) | Skipped (`TestArchivedProjectIsNotServed`) |
| **Read queries (`project show|list`, `cases`, `board`, `case report`)** | Allowed | Allowed | Allowed |

### 5.3 Transaction-Boundary Fixes for `M2-T4`

As identified in card `M2-T4`:
- `internal/emergency/service.go`: `CheckLeaseAllowedTx(ctx, tx)` already runs inside `execution.StartAttempt` and `execution.RenewLease` transactions. It gains a companion Project-scoped check (`CheckProjectLeaseAllowedTx(ctx, tx, projectID)` or `projectID` parameter via lease gate; empty `projectID` checks Collective controls only).
- `internal/operations/service.go`: `CheckExecutionAllowed(ctx, "external_operation")` currently runs **outside** the SQLite transaction at `PrepareWithGuard` and `DispatchWithGuard`, leaving a concurrency gap if `project pause` (or `emergency freeze`) lands concurrently before commit. `M2-T4` moves/adds **in-transaction** checks (`CheckProjectOperationAllowedTx(ctx, tx, projectID, "external_operation")`) inside both the `PrepareWithGuard` commit transaction and the `DispatchWithGuard` commit-boundary claim transaction (`DISPATCHED` transition), ensuring `pause` and `dispatch` cannot interleave. Collective `emergency freeze` continues to override every Project regardless of Project state (`TestCollectiveFreezeStillStopsAllProjects`).

---

## 6. Signed Project Manifest (`M2-T3`)

### 6.1 File location and canonical verification flow

- Path: `$SUMMA42_HOME/projects/<slug>/project.json`.
- `summa42 project apply <slug>`:
  1. Reads `$SUMMA42_HOME/projects/<slug>/project.json` (refusing symlinks leaving the project directory and non-regular files).
  2. Parses with `json.NewDecoder(...).DisallowUnknownFields()` (`TestManifestRejectsUnknownField`).
  3. Validates schema semantics (Section 6.2), verifies policy-set subset containment (`TestManifestGrantsCannotExceedPolicy`), and verifies `private` data-class model constraints.
  4. Displays a structured summary (slug, repos, hosts, base branches, data classes, model profiles, capabilities, grants, limits, SHA-256) and asks the Owner to confirm (`y/N`; `--yes` skips TTY prompt for automated tests while still requiring a live Owner Ed25519 signature).
  5. Signs canonical payload `(manifestDomain, collective_id, project_id, slug, manifest_sha256, issued_at, principal_id)` with the Owner Ed25519 key and records `manifest_sha256` on `projects` + an immutable row in `project_events` (`APPLY_MANIFEST`).
- `summa42-box serve --project <slug>` (or all `ACTIVE` Projects when `--project` is omitted in `M2-T5`) and `run-* --project <slug>`:
  1. Loads `projects.manifest_sha256` from SQLite. Refuses to start if `manifest_sha256 == ""` (`TestUnsignedManifestRefused`).
  2. Reads `$SUMMA42_HOME/projects/<slug>/project.json`, computes SHA-256, and refuses if missing, unreadable, invalid, or hash != recorded `projects.manifest_sha256` (`TestEditedManifestRefusedUntilReapplied`).

### 6.2 Multi-repository, multi-host schema (`project.json`)

One Project (`daas`, for instance) may declare **multiple repositories** across both **GitHub** and **Azure DevOps** in `repositories[]`, each with independent identity, base branch, allowed paths, validation commands, credential file path, and `data_class`:

```json
{
  "schema_version": 1,
  "project": "summa42",
  "mission_statement": "Maintain Summa42 safely",
  "data_class": "public",
  "workspace_root": "/var/lib/summa42/workspaces/summa42",
  "repositories": [
    {
      "id": "summa42-main",
      "host": "github",
      "repository": "SofiaFlux/summa42",
      "local_path": "/var/lib/summa42/repos/summa42",
      "base_branch": "main",
      "credential_file": "/etc/summa42/creds/github_token",
      "data_class": "public",
      "allowed_prefixes": ["internal/", "cmd/"],
      "allowed_paths": [],
      "context_files": ["AGENTS.md"],
      "context_max_bytes": 262144,
      "plan_read_mode": false,
      "validation": {
        "commands": [["go", "test", "./..."]],
        "user": "summa42-validate",
        "timeout": "10m",
        "toolchain_paths": ["/usr/local/go/bin"]
      },
      "sandbox": {
        "toolchains": ["go1.27"],
        "fetch_allowlist": ["pkg.go.dev", "go.dev"],
        "registry_allowlist": { "mode": "lockfile", "prefixes": [] },
        "greenfield": false
      },
      "intake": {
        "maintainers": ["owner-login"],
        "bot_login": "summa42-bot",
        "auto_grant_label": "summa42-pilot",
        "poll_interval": "30s"
      },
      "case_grants": {
        "capabilities": ["github.issue.read", "workspace.repo.write", "workspace.test"],
        "actions": ["github.issue.read", "workspace.repo.write", "workspace.test"],
        "work_capabilities": ["workspace.repo.write", "workspace.test"]
      }
    }
  ],
  "models": {
    "producer": {
      "profile": "agy",
      "model": "gemini-flash",
      "user": "summa42-model",
      "timeout": "30m",
      "allow_paid": false,
      "zdr": false
    },
    "reviewer": {
      "profile": "claude",
      "model": "claude-opus-4-6",
      "user": "summa42-review",
      "timeout": "15m",
      "allow_paid": false,
      "zdr": false
    },
    "fallback_reviewers_file": ""
  },
  "limits": {
    "max_steps": 3,
    "remaining_budget": 10,
    "max_repairs": 3,
    "lease_duration": "5m",
    "heartbeat_interval": "1m",
    "max_wall_time": "2h"
  }
}
```

#### Validation rules per repository & model:

1. **Multi-host repositories (`host`: `"github"` | `"ado"`):**
   - `"github"`: `repository` as `owner/name` (equivalent to `--repo` / `--repository`), mapped deterministically to pipeline flags (`TestPipelineFlagsAndManifestAreEquivalent`).
   - `"ado"`: `organization`, `ado_project`, `repository`, `reviewer_id`, alongside `base_branch`, `local_path`, `credential_file`, `validation`, `allowed_*`, and `data_class` (ready for `M2A`).
2. **Path hygiene:** per repository, `allowed_paths` and `allowed_prefixes` are mutually exclusive (matching `groundingFlags` in `cmd/summa42-box/ghplan_source.go`), all paths are clean relative paths without `..`, and protected paths (`Rule R5`) remain forbidden for normal tasks (`greenfield: false` default).
3. **Credentials never in manifest:** only absolute `credential_file` paths (mode `0600` verified at runtime) are allowed. Inline secret keys do not exist in the struct and fail `DisallowUnknownFields`.
4. **Data classification & ZDR / local model enforcement (`public` | `private`):**
   - A repository inherits `project.data_class` unless overridden monotonically (`public` -> `private` allowed; `private` -> `public` rejected).
   - If any repository (or Project) has `data_class == "private"`:
     - Every model profile referenced for it (producer, primary reviewer, and every entry in `fallback_reviewers_file`) MUST prove either `zdr == true` (verified remote zero-data-retention flag, e.g. Command Code `CMD_ZDR=1` per `internal/ghtriage/climodel/commandcode.go`) OR a local-only host profile (no external endpoint upload). Non-ZDR remote profiles (including free external tiers where `ZDR` is disallowed in `commandcode.go`) are rejected at both `project apply` and runtime preflight (`TestPrivateDataClassRefusesNonZDRRemoteModel`).
     - Cross-reference `M1A-T1`: `"private"` also disables `/s42/fetch` and live package-registry proxy egress for Attempts on that repository.
5. **Independent reviewer separation:** enforces existing `modelrun.ResolveReviewerUser` (`producer.user != reviewer.user != validation.user`) and `producer model != reviewer model`.

---

## 7. Vision Compatibility Analysis

| Vision Article | Requirement | How this design complies |
| --- | --- | --- |
| **Art. 3 (Mission is durable; agents are ephemeral)** | Purpose, tasks, budgets, approvals, and state live in durable kernel records; workers/models remain stateless. | `projects`, `project_events`, `missions.project_id`, and `projects.envelope_id` persist in SQLite. Pause/crash revokes only ephemeral leases; Mission, cases, and board state endure. |
| **Art. 4 (Mission, purpose, legitimate work)** | Collective holds bounded Mission/obligations; every task traces to a purpose; dormancy when empty. | Replacing Collective-global `missions_one_active` with **one active Mission per Project** lets multiple Projects coexist without merging or losing purpose traceability. Each Task still requires a valid Mission/Obligation/Collective purpose. Idle/paused Projects stay dormant. |
| **Art. 5 (Authority is scoped, not a global score)** | Explicit domain-scoped permissions per resource, deadline, budget, and enforcement profile. | `project.json` explicitly scopes repositories, branches, paths, capabilities, actions, data classes, and model restrictions per repository. `case_grants` must remain a subset of the Collective policy ceiling. |
| **Art. 6 (Authority only narrows, never expands)** | Collective/Project may contract autonomously, never enlarge authority without an Owner signature. | New Projects start at `hard_limit = 0` (`envelope_project_<slug>`) and `manifest_sha256 = ''` (cannot run until Owner-signed `apply` and `fund`). `PAUSE` and `ARCHIVE` contract immediately; only Owner-signed `RESUME` or `fund` restores/raises limits inside `envelope_collective_root`. No manifest can exceed Collective policy. |
| **Art. 15 (Economics are governance)** | Resource ledger as authority boundary tracking settled and unresolved exposure. | Child Project envelopes hang under `envelope_collective_root` with strict partition + ancestor checks. Projects cannot spend each other's allocation or collectively exceed Collective root limit. |
| **Art. 20 (Scheduling is arbitration)** | Scheduler checks eligibility (mission-aligned, authorized, funded, safe) before arbitrations; fair across work. | `StartAttempt` and `operations` check Project `ACTIVE` state + Project envelope + Collective break glass inside tx before execution. Strict child envelope budgets prevent noisy-neighbor budget exhaustion, preparing `M2-T5` round-robin scheduling across Projects. |

---

## 8. Proof of Backwards Compatibility (Single Default Project Equivalence)

A Collective initialized cleanly or upgraded from `main` with **only** `project_default` (`slug = "default"`) behaves identically to today:

1. **Migration on existing DB:**
   - Pre-M2 DB has at most 1 active `missions` row (`M0`), existing `workflow_cases` / `board_cards` pointing to `M0`, and existing `resource_envelopes` (`E0...` + `envelope_default`).
   - `00040_projects.sql` inserts `project_default` (`state = 'ACTIVE'`) and sets `M0.project_id = 'project_default'`.
   - `envelope_collective_root` adopts pre-existing rootless limits verbatim (never changing an existing envelope's `hard_limit` or `Available()`).
2. **Existing CLI (`cmd/summa42` and `cmd/summa42-box`):**
   - `summa42 mission create` / `show` without `--project` target `project_default`, hitting the same unique active constraint (`missions_one_active_per_project` for `project_default`) and identical error paths.
   - `emergency freeze`, `dead_man_switch`, `envelope`, `approvals`, `cases`, `case report`, and `board` behave unchanged because only `project_default` exists and is `ACTIVE`.
   - Until a manifest is opted into with `--project <slug>` in `M2-T3`, existing `run-gh-intake`, `run-gh-pipeline`, `run-worker`, and flag-driven `serve` continue using the exact flag contract (`TestPipelineFlagsAndManifestAreEquivalent`).
   - Round-robin scheduling across `[project_default]` reduces to length 1 — identical scheduling order to single-Project today.

---

## 9. Handoff Notes for Downstream Cards

- **`M2-T2` (`BUILD`):** Migration `00040_projects.sql` (note updated number from `00038`), `internal/project/service.go`, update `internal/purpose/service.go` (`projectID` scoping), `internal/runtime/box.go`, `cmd/summa42/mission_cmd.go` (`--project`), `cmd/summa42/project_cmd.go` (`create|list|show`). Named tests: `TestUpgradeAdoptsExistingMissionIntoDefaultProject`, `TestTwoProjectsEachHaveOneActiveMission`, `TestSecondActiveMissionInSameProjectRefused`, `TestProjectCreateRequiresOwnerSignature`.
- **`M2-T3` (`BUILD`):** `$SUMMA42_HOME/projects/<slug>/project.json` schema + validator (`DisallowUnknownFields`, GitHub + ADO repos, per-repo `data_class` ZDR/local check, policy subset check, no inline secrets), `summa42 project apply <slug>` (confirmation + Ed25519 signature), `--project <slug>` hash check in `serve` & `run-*`. Named tests per plan + ZDR/private check (`TestPrivateDataClassRefusesNonZDRRemoteModel`).
- **`M2-T4` (`BUILD`):** Child envelope `parent_envelope_id = envelope_collective_root` + strict partition check in `internal/resources`, `summa42 project fund|pause|resume|archive`, **in-transaction** gates in `internal/execution` (`StartAttempt`, `RenewLease`, `RevokeProjectLeasesTx`) and `internal/operations` (`PrepareWithGuard`, `DispatchWithGuard`), scoped `internal/emergency/` helper (marked for Owner review per R5).
- **`M2-T5` / `M2-T6`:** `serve` multi-project loop (skip `PAUSED`/`ARCHIVED`, round-robin `ACTIVE` projects with applied manifest) and `internal/board` wiring to intake (`mission_id` -> `project_id`).

---

## 10. Open Questions / Confirmed Defaults for Owner Review

1. **Child vs. root envelope budgeting (Section 4.2):** Strict tree partitioning (`sum(children.hard_limit) + direct_committed(root) <= root.hard_limit`) plus ancestor checks in `ReserveInTx`, rather than over-committing child limits above `root.hard_limit`. *Default: strict partitioning (prevents noisy-neighbor starvation).*
2. **Legacy non-`envelope_default` funded envelopes during upgrade (Section 3.2):** Pre-existing rootless envelopes stay intact and are re-parented under `envelope_collective_root` whose initial `hard_limit` equals their sum, while `envelope_project_default` starts at `0` (preserving `--envelope <id>` workflows until `project.json` replaces flags). *Default: re-parent intact without altering existing limits.*
3. **`archive` behavior with in-flight state (Section 5.1):** Refuse `project archive` while active leases or non-final `DISPATCHED`/`OUTCOME_UNKNOWN` operations exist (require `pause` + reconciliation first). *Default: refuse while active leases or ambiguous operations remain.*
