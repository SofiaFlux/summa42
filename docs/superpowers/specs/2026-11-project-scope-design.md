# Project Scope Design Specification (`M2-T1`)

**Card:** `M2-T1` (`DESIGN`) — [development plan](../plans/2026-11-11-development-plan.md#m2-projects)
**Date:** 2026-11-27 (amended 2026-11-27 per Owner decision: one active Collective Mission spanning multiple Projects, Owner-gated Mission switching)
**Status:** Approved (Owner, 2026-11-27; reviewed `ACCEPT` by Claude Opus, 2026-11-27)
**Vision & Plan Alignment:** [Vision](../../VISION.md) (Articles 3, 4, 5, 6, 15, 20, 22, 27), [Plan Section 2.1](../plans/2026-11-11-development-plan.md#21-three-scopes-one-kernel)

---

## 1. Purpose and Scope

Today Summa42 holds **one** active Mission for the entire Collective (`missions_one_active` in `internal/state/sqlite/migrations/00002_purpose.sql`), a flat set of `resource_envelopes` (`00005_resources.sql`) with no parent enforcement in `internal/resources`, Collective-wide emergency controls (`00028_emergency_controls.sql`), and ~20 CLI flags across `run-gh-intake`, `run-gh-pipeline`, and `run-worker`.

Per the Owner's architectural decision (2026-11-27), **Missions remain Collective-scoped**: a Collective may hold many stored Missions (historical or standby), with **at most one active Mission at a time** (`missions_one_active`), and switching which Mission is active requires explicit **Owner approval / signature**. One active Mission may span **multiple Projects** (for example: managing a product line or the technical organization end-to-end).

This specification defines **Project** as a first-class narrowing kernel scope under the Collective and its Mission hierarchy (never above it):

```text
Collective (one Owner, one Box, one SQLite database, Collective root envelope, global Break Glass & Dead Man Switch)
  ├── Missions (many stored Missions; at most one active Collective Mission at a time, switched only by Owner signature)
  └── Project (slug, ACTIVE|PAUSED|ARCHIVED, child resource envelope, signed manifest hash)
        ├── Sources (GitHub & Azure DevOps repositories declared in the Project manifest)
        ├── Grants & standing delegations (scoped to the Project; never widening Collective policy)
        ├── Board (board_cards scoped directly by project_id)
        └── Cases, Tasks, Attempts, Evidence, External Operations (scoped to project_id)
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

ALTER TABLE resource_envelopes
    ADD COLUMN project_id TEXT REFERENCES projects(project_id);

CREATE INDEX resource_envelopes_project ON resource_envelopes(project_id);

ALTER TABLE workflow_cases
    ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default';

CREATE INDEX workflow_cases_project ON workflow_cases(project_id, state);

ALTER TABLE board_cards
    ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default';

CREATE INDEX board_cards_project_column ON board_cards(project_id, column_name);

ALTER TABLE tasks
    ADD COLUMN project_id TEXT REFERENCES projects(project_id);

CREATE INDEX tasks_project_state ON tasks(project_id, state);

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
- `autonomy_step` records the per-Project autonomy ladder (`Plan Section 2.3`, initialized to `'A1'`). It is displayed by `summa42 project show|list` (`M2-T2`) and updated only by Owner-approved autonomy promotion; it never bypasses policy, approvals, or resource checks.

### 2.2 Direct `project_id` vs. inherited lineage

All durable tables fall into one of four explicit ownership classes:

| Ownership class | Tables | Schema mechanism & rationale |
| --- | --- | --- |
| **1. Direct `project_id` (`NOT NULL`)** | `projects`, `project_events`, `workflow_cases`, `board_cards` | Because a single active Mission spans multiple Projects, work-queue and case boundaries live directly on Projects (`project_id TEXT NOT NULL DEFAULT 'project_default'`; SQLite `ALTER TABLE` disallows `REFERENCES` on non-NULL `DEFAULT` columns, so existence is checked by the kernel services). |
| **2. Direct nullable `project_id`** | `resource_envelopes` (`00040_projects.sql` in `M2-T2`), `tasks` (`00040_projects.sql` in `M2-T2`: populated from workflow cases / Project intake, `NULL` for Collective-global maintenance/governance), `obligations` (added when per-Project obligations wire in `M7-T3`; Collective-global `NULL` until then), `owner_decision_packets` (added when per-Project packets filter in `M4-T2`), plus planned `owner_action_records` (*not yet in migrations; added in `M4-T1`*) | Entities that exist at **either** Collective scope (`project_id IS NULL`: `envelope_collective_root`, `envelope_default`, Collective obligations/maintenance, pre-M2 test envelopes) **or** Project scope (`project_id = <id>`). Leaving `project_id` nullable preserves backwards compatibility for existing unit tests while enforcing Project containment where `project_id` is set. |
| **3. Reached through `workflow_case`, `board_card`, or `task` — no duplicate column** | **Via `case_id`:** `workflow_assessments`, `workflow_verifications`.<br>**Via `card_id`:** `board_card_moves`.<br>**Via `task_id`:** `task_dependencies`, `attempts`, `task_challenges`, `attempt_failures`, `execution_events`, `attempt_run_manifests`, `attempt_containments`, `attempt_containment_events`, `external_operations`, `effect_slots`, `approval_requests`, `approval_decisions`, `capability_sessions`, `capability_assessments`, `evidence_objects` / `evidence_subjects`, `model_calls` (when `case_id` / `task_id` present), `github_issue_triage_reviews`, `foreign_evidence_lineage`, `external_task_reconciliations`. | Avoids denormalized `project_id` drift. Attempts, operations, and capability sessions resolve their Project atomically through `tasks.project_id` inside kernel SQLite transactions. |
| **4. Collective-global (no `project_id`)** | `missions`, `goals`, `principals`, `constitutions`, `collective_metadata`, `policy_sets`, `emergency_controls`, `dead_man_switch`, `emergency_audit_events`, `cube_quarantines`, `owner_attention_budgets`, `audit_events`, `notification_outbox`, `capability_definitions`, `experience_*`, `field_*`, `sanitized_feedback`, `company_*`, `business_invariants` | Collective purpose (`missions_one_active`: one active Collective Mission across all Projects), constitutional roots, host identity, global emergency/quarantine/audit controls, and Company OS cross-project state (`Plan Section 2.1`). |

---

## 3. Default Project Adoption & Collective Mission Switching

### 3.1 One active Mission per Collective (`missions_one_active`) & Owner-gated switching (`M2-T2`)

- **Index unchanged:** `missions_one_active` on `missions(active) WHERE active = 1` (`00002_purpose.sql`) stays **intact**. The Collective can hold many stored Mission rows (retired or standby), while SQLite enforces at most **one** `active = 1` Mission at any moment.
- **No autonomous Mission switching:** Attempting `CreateMission` while another Mission already has `active = 1` stays refused (`UNIQUE constraint failed` / active Mission already exists).
- **Owner-signed switching (`M2-T2`):** Switching which Mission is active requires an explicit **Owner Ed25519 signature** (`summa42 mission switch <mission-id>`, plus optional `summa42 mission list`, wired over Owner-signed control endpoints `POST /missions/switch/challenge` + `POST /missions/switch`). Inside one SQLite transaction (`purpose.Service.SwitchMission`), the currently active Mission (if any) is set to `active = 0, deactivated_at = now` and the target Mission is set to `active = 1, deactivated_at = NULL`. Automated/model-driven mission switching is not enabled; future delegation remains an explicit future card.
- `internal/purpose/service.go` preserves `CreateMission(ctx, statement)` and `ActiveMission(ctx)` at Collective scope, and adds `CreateInactiveMission` / `ListMissions` / `SwitchMission(ctx, targetMissionID)`.

### 3.2 Backfill and Adoption Rules in Migration `00040_projects.sql`

When upgrading any existing database (whether created by `summa42 init` or a `testutil.OpenStore` test fixture):

1. **Collective root envelope (`envelope_collective_root`)**:
   - Inserted via `INSERT OR IGNORE` if absent, with `parent_envelope_id = NULL`, `project_id = NULL`.
   - To avoid silently inventing new budget (`00037` / R6) or under-funding already Owner-granted envelopes on upgrade, its initial `hard_limit` is set to:
     `COALESCE((SELECT SUM(hard_limit) FROM resource_envelopes WHERE envelope_id <> 'envelope_collective_root' AND parent_envelope_id IS NULL), 0)`.
     On a freshly initialized Collective (or after `00037` zeroed `envelope_default`), this sum is `0`.
   - `bootstrap.Service.Init` (`summa42 init`) inherits both `envelope_collective_root` (`hard_limit = 0`) and `project_default` (`slug = 'default'`, `state = 'ACTIVE'`, `envelope_id = 'envelope_project_default'`) seeded cleanly by migrations (`00040_projects.sql`).
2. **Default Project (`project_default`)**:
   - Pre-existing rootless non-root envelopes (other than `envelope_collective_root`) are re-parented under `envelope_collective_root`:
     `UPDATE resource_envelopes SET parent_envelope_id = 'envelope_collective_root' WHERE envelope_id <> 'envelope_collective_root' AND parent_envelope_id IS NULL`.
   - `envelope_project_default` is created via `INSERT OR IGNORE` at `hard_limit = 0` under `envelope_collective_root` (`project_id = 'project_default'`) and linked as `projects.envelope_id` for `project_default`. Note: `envelope_default` (seeded by `00032` and zeroed in `00037` for synthesized Collective obligations) remains a separate Collective-scoped child of `envelope_collective_root` (`project_id IS NULL`), distinct from `envelope_project_default`. Existing pre-M2 envelopes also remain usable children of `envelope_collective_root` (so in-flight cases referencing `--envelope <id>` keep working without manual re-wiring and no pre-M2 limits change).
3. **Existing cases and board**:
   - Every existing row in `workflow_cases` and `board_cards` receives `project_id = 'project_default'` via `ALTER TABLE ... ADD COLUMN project_id TEXT NOT NULL DEFAULT 'project_default'` with zero row copies or ID rewrites.
   - Existing `missions` stay untouched at Collective scope under `missions_one_active`, shared across Projects.

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
   - Walk ancestors from `envelopeID` up to root (`parent_envelope_id`, bounded depth `<= 8` against cycles, failing closed with an explicit error if exceeded): for the leaf `envelopeID`, apply the existing check (`availableIn` / `AllowRenewalInTx`), and for every non-nil ancestor `A` verify that `direct_committed(A) + SUM(direct_children committed/reserved) <= A.hard_limit`.
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
| `ARCHIVE` | `summa42 project archive <slug> --reason ...` | `ACTIVE`, `PAUSED` | **Refuses** if the Project has any `ACTIVE` Attempt lease or any external operation in `DISPATCHED` or `OUTCOME_UNKNOWN` state (Vision Article 22: ambiguous outcomes must be reconciled first). Otherwise sets `state = 'ARCHIVED'`, `archived_at = now`, and appends `project_events` (Collective's active Mission remains active for other Projects). Terminal: cannot transition back to `ACTIVE` or `PAUSED`. |

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

0. **Schema version:** `schema_version` must equal `1`; any other value fails closed until a future card defines a migration path.
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
| **Art. 3 (Mission is durable; agents are ephemeral)** | Purpose, tasks, budgets, approvals, and state live in durable kernel records; workers/models remain stateless. | `missions`, `projects`, `project_events`, `workflow_cases.project_id`, `board_cards.project_id`, and `projects.envelope_id` persist in SQLite. Pause/crash revokes only ephemeral leases; Mission, cases, and board state endure. |
| **Art. 4 (Mission, purpose, legitimate work)** | Collective holds one active Mission at a time, plus obligations; every task traces to a purpose; dormancy when empty. | Matches `VISION.md` Art. 4 **literally**: `missions_one_active` keeps at most one active Collective Mission, switched only by explicit Owner signature, while spanning multiple Projects (`1 Mission -> N Projects`). Idle/paused Projects stay dormant. |
| **Art. 5 (Authority is scoped, not a global score)** | Explicit domain-scoped permissions per resource, deadline, budget, and enforcement profile. | `project.json` explicitly scopes repositories, branches, paths, capabilities, actions, data classes, and model restrictions per repository. `case_grants` must remain a subset of the Collective policy ceiling. |
| **Art. 6 (Authority only narrows, never expands)** | Collective/Project may contract autonomously, never enlarge authority without an Owner signature. | New Projects start at `hard_limit = 0` (`envelope_project_<slug>`) and `manifest_sha256 = ''` (cannot run until Owner-signed `apply` and `fund`). `PAUSE` and `ARCHIVE` contract immediately; only Owner-signed `RESUME` or `fund` restores/raises limits inside `envelope_collective_root`. No manifest can exceed Collective policy. |
| **Art. 15 (Economics are governance)** | Resource ledger as authority boundary tracking settled and unresolved exposure. | Child Project envelopes hang under `envelope_collective_root` with strict partition + ancestor checks. Projects cannot spend each other's allocation or collectively exceed Collective root limit. |
| **Art. 20 (Scheduling is arbitration)** | Scheduler checks eligibility (mission-aligned, authorized, funded, safe) before arbitrations; fair across work. | `StartAttempt` and `operations` check Project `ACTIVE` state + Project envelope + Collective break glass inside tx before execution. Strict child envelope budgets prevent noisy-neighbor budget exhaustion, preparing `M2-T5` round-robin scheduling across Projects. |

---

## 8. Proof of Backwards Compatibility (Single Default Project Equivalence)

A Collective initialized cleanly or upgraded from `main` with **only** `project_default` (`slug = "default"`) behaves identically to today:

1. **Migration on existing DB:**
   - Pre-M2 DB has at most 1 active `missions` row (`M0`), existing `workflow_cases` / `board_cards`, and existing `resource_envelopes` (`E0...` + `envelope_default`).
   - `00040_projects.sql` inserts `project_default` (`state = 'ACTIVE'`) and sets `workflow_cases.project_id = 'project_default'` and `board_cards.project_id = 'project_default'`. `M0` remains the Collective's active Mission.
   - `envelope_collective_root` adopts pre-existing rootless limits verbatim (never changing an existing envelope's `hard_limit` or `Available()`).
2. **Existing CLI (`cmd/summa42` and `cmd/summa42-box`):**
   - `summa42 mission create` / `show` operate on the Collective's single active Mission unchanged; `summa42 mission switch` / `list` add explicit Owner-signed switching across stored Missions.
   - `emergency freeze`, `dead_man_switch`, `envelope`, `approvals`, `cases`, `case report`, and `board` behave unchanged because only `project_default` exists and is `ACTIVE`.
   - Until a manifest is opted into with `--project <slug>` in `M2-T3`, existing `run-gh-intake`, `run-gh-pipeline`, `run-worker`, and flag-driven `serve` continue using the exact flag contract (`TestPipelineFlagsAndManifestAreEquivalent`).
   - Round-robin scheduling across `[project_default]` reduces to length 1 — identical scheduling order to single-Project today.

---

## 9. Handoff Notes for Downstream Cards (including 2026-11-27 Owner Amendment)

- **`M2-T2` (`BUILD` — amended per Owner 2026-11-27 decision, keeping protected `development-plan.md` untouched per R5):**
  - Migration `00040_projects.sql` (updated number from `00038`; adds `projects`, `project_events`, `workflow_cases.project_id`, `board_cards.project_id`, nullable `resource_envelopes.project_id` & `tasks.project_id`, `envelope_collective_root`, and `project_default` backfill while **preserving `missions_one_active`**).
  - `internal/project/service.go` (Owner-signed creation + zero-limit child envelope under `envelope_collective_root`, list/show/resolve), `internal/purpose/service.go` (`ListMissions`, `CreateStandbyMission`, and Owner-gated `SwitchMission` while keeping single active Collective Mission), `internal/runtime/box.go`, `internal/control/` (Project endpoints + Mission `list`/`switch`), `cmd/summa42/mission_cmd.go` (`create|show|list|switch`), `cmd/summa42/project_cmd.go` (`create|list|show`, showing `autonomy_step`).
  - Named tests replacing the old per-project-mission assumption: `TestUpgradeAdoptsExistingCasesAndBoardIntoDefaultProject`, `TestTwoProjectsShareCollectiveActiveMissionWithIsolatedCasesAndBoard`, `TestSecondActiveMissionRefusedAndOwnerSwitchReplacesActiveMission`, `TestProjectCreateRequiresOwnerSignature`.
- **`M2-T3` (`BUILD`):** `$SUMMA42_HOME/projects/<slug>/project.json` schema + validator (`schema_version == 1`, `DisallowUnknownFields`, GitHub + ADO repos, per-repo `data_class` ZDR/local check, policy subset check, no inline secrets), `summa42 project apply <slug>` (confirmation + Ed25519 signature), `--project <slug>` hash check in `serve` & `run-*`. Named tests per plan + ZDR/private check (`TestPrivateDataClassRefusesNonZDRRemoteModel`).
- **`M2-T4` (`BUILD`):** Child envelope `parent_envelope_id = envelope_collective_root` + strict partition check & bounded ancestor walk (`TestEnvelopeHierarchyRejectsCycleAndDepthLimit`) in `internal/resources`, `summa42 project fund|pause|resume|archive`, **in-transaction** gates in `internal/execution` (`StartAttempt`, `RenewLease`, `RevokeProjectLeasesTx`) and `internal/operations` (`PrepareWithGuard`, `DispatchWithGuard`), scoped `internal/emergency/` helper (marked for Owner review per R5). (Nullable `obligations.project_id` is added in `M7-T3` or `M2-T4` when Project-scoped obligations are wired.)
- **`M2-T5` / `M2-T6`:** `serve` multi-project loop (skip `PAUSED`/`ARCHIVED`, round-robin `ACTIVE` projects with applied manifest) and `internal/board` wiring to intake (`project_id`).

---

## 10. Open Questions / Confirmed Defaults for Owner Review

1. **Child vs. root envelope budgeting (Section 4.2):** Strict tree partitioning (`sum(children.hard_limit) + direct_committed(root) <= root.hard_limit`) plus ancestor checks in `ReserveInTx`, rather than over-committing child limits above `root.hard_limit`. *Default: strict partitioning (prevents noisy-neighbor starvation).*
2. **Legacy non-`envelope_default` funded envelopes during upgrade (Section 3.2):** Pre-existing rootless envelopes stay intact and are re-parented under `envelope_collective_root` whose initial `hard_limit` equals their sum, while `envelope_project_default` starts at `0` (preserving `--envelope <id>` workflows until `project.json` replaces flags; `envelope_default` remains the separate Collective obligations envelope). *Default: re-parent intact without altering existing limits.*
3. **`archive` behavior with in-flight state (Section 5.1):** Refuse `project archive` while active leases or non-final `DISPATCHED`/`OUTCOME_UNKNOWN` operations exist (require `pause` + reconciliation first). *Default: refuse while active leases or ambiguous operations remain.*
4. **Collective Mission model (Confirmed by Owner, 2026-11-27):** Multiple stored Missions in `missions`, strictly one `active = 1` per Collective (`VISION.md` Art. 4 kept verbatim), Owner-signed switching (`mission switch`), and `1 Active Mission -> N Projects` via `project_id` on Projects' cases, board cards, envelopes, and tasks.
