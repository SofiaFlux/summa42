# Architecture Spike: Context Projection Compiler and Attempt Run Manifest (ADK, CAFE(S), MWP)

**Issue:** [#59](https://github.com/SofiaFlux/summa42/issues/59)  
**Parent Epic:** [#3](https://github.com/SofiaFlux/summa42/issues/3) (*Slim Meeseek into a semantic control plane over mature OSS runtimes*)  
**Status:** Completed  
**Recommendation:** **ADOPT** (Adopt compiled working context projections and descriptive run manifests; strictly enforce the hard semantic boundary where filesystem artifacts are projections, never canonical authority)  
**Date:** 2026-11-10  

---

## 1. Executive Summary

This architecture spike addressed the context isolation, minimization, and provenance boundary for agent execution under Meeseek Attempt governance.

Drawing inspiration from **Google ADK** (compiled contextual views), **Google Conductor** (persistent inspectable artifacts), **Google CAFE(S)** quality evaluation, and **MWP (Markdown Workspace Protocol)** convergence evidence, this spike designed and tested:
1. The **`ContextCompiler`** (`internal/runmanifest/compiler.go`) that compiles canonical domain state into a clean, minimal Attempt Working Context (`TASK.md`, `CAPABILITIES.json`, `EVIDENCE.md`, `POLICIES.md`).
2. An automated **CAFE(S)** evaluator assessing Clarity, Actionability, Fidelity, Efficiency, and Security.
3. Automated negative verification tests proving that the filesystem remains an unprivileged projection and can never hijack canonical authority.

---

## 2. Hard Semantic Boundary: Projection vs Canonical State

The central thesis of Summa42 is:
> **Meeseek owns semantics; providers and projections own mechanisms.**

To ensure security in agentic systems, filesystem artifacts must never define canonical truth:

| Filesystem Condition | Canonical Reality | Invariant Enforced |
|---|---|---|
| Folder exists in workspace | **!=** Task exists in Collective | Canonical state in SQLite is immutable from filesystem edits |
| `TASK.md` edited to say "ACCEPTED" | **!=** Task accepted by Verifier | Verification gate evaluates independent criteria, ignoring file edits |
| `CAPABILITIES.json` has `admin_root` | **!=** Authority granted to Attempt | Capability calls strictly validated against canonical database permissions |
| Workspace copied to another node | **!=** Authority transferred | Copying copies inert data only; never private signing keys or active leases |
| Attempt workspace has fence `N` | Outdated if SQLite has fence `N+1` | Stale workspaces rejected immediately with `ErrStaleWorkspace` |

---

## 3. Google CAFE(S) Quality Evaluation

The `ContextCompiler` assesses each compiled working context across the 5 CAFE(S) dimensions:

1. **Clarity (1.0):** Clean, standard GitHub-flavored Markdown structure with explicit sections and JSON schemas.
2. **Actionability (1.0):** Explicit task objective, uncompleted checkboxes for acceptance criteria (`- [ ]`), and clear visible tool signatures.
3. **Fidelity (1.0):** Cryptographic correspondence: `ContextHash`, `PolicyHash`, and input evidence hashes exactly match SQLite domain records.
4. **Efficiency (1.0):** Strict token economy: workspace context size bounded (<100KB), avoiding dumping irrelevant repository files into agent prompt windows.
5. **Security (1.0):** Complete absence of ambient credentials (no AWS keys, SSH keys, owner signing keys, or docker sockets). Materialized as read-only files (`0444`).

---

## 4. Minimal Descriptive Attempt Run Manifest

The Attempt Run Manifest (`Manifest` in `internal/runmanifest/service.go`) persists at lease time in the same database transaction as the Attempt creation:
- **Runtime Provenance:** Commit hash and semantic version.
- **Identities:** `TaskID`, `AttemptID`, `FenceGeneration`, `ResourceEnvelopeID`.
- **Policy & Security:** `PolicySetHash`, `TEBProfile` (guarantees enforced).
- **Context Fingerprint:** Deterministic `ContextProjectionHash` (SHA-256).
- **Evidence References:** Exact input evidence content hashes.

The manifest is purely descriptive: it records what the executor saw, enabling forensic audit and offline replay, but it does **not** introduce a secondary truth store.

---

## 5. Automated Verification Results

Automated tests in `internal/runmanifest/compiler_test.go` confirmed:
- `TestContextCompiler_CompilesAndAssessesCAFES`: **PASS** (100% score on all CAFE(S) dimensions).
- `TestContextProjection_WorkspaceModificationsDoNotAlterCanonicalState`: **PASS** (Forced edits to `TASK.md` leave SQLite state unaltered).
- `TestContextProjection_FabricatedCapabilitiesInWorkspaceDoNotGrantAuthority`: **PASS** (Fabricated capability in workspace rejected with `ErrUnauthorizedCapability`).
- `TestContextProjection_StaleAttemptWorkspacesCannotExecuteProtectedActions`: **PASS** (Old fence generation rejected with `ErrStaleWorkspace`).
- `TestContextProjection_CopyingWorkspaceNeverTransfersAuthorityTokens`: **PASS** (Copied workspace contains zero credentials or authority grants).

---

## 6. Recommendation: ADOPT

### Decision: **ADOPT**
1. Standardize on the `ContextCompiler` for preparing Attempt workspaces across all executors (Claude, Codex, MAF, Shell, Python).
2. Mandate CAFE(S) evaluation before dispatching attempts to keep token consumption efficient and prevent credential leakage.
3. Retain the descriptive Attempt Run Manifest as the single source of truth for execution provenance.
