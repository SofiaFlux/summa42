# Local implementation plan

> **For agentic workers:** Use superpowers:executing-plans inline with TDD and one final independent Astra review.

**Goal:** Prepare a real bounded candidate and validation evidence from accepted grounded plans.
**Architecture:** Review-backed input → fenced Task/Attempt → structured edits → fresh pinned Git checkout → local commit → deterministic validation → completion evidence awaiting independent code review.
**Tech Stack:** Go 1.27, Git >=2.45, existing SQLite/evidence and model CLI.
**Spec:** docs/superpowers/specs/2026-10-07-local-implementation-design.md

## Global Constraints

- 1–64 exact allowed files; UTF-8/no NUL; <=256 KiB edits; no symlinks/submodules.
- Lease 15 minutes; total timeout 14 minutes; model 2 minutes; validation each 60 seconds, <=16 KiB output.
- Native execution UNENFORCED; no Task acceptance or external GitHub effects.
- Existing v1 and plan review behavior remain compatible.

## Review Focus

- Duplicate callers and interrupted completion must not duplicate a completed model/test run.
- An accepted-looking record without exact Task/Attempt acceptance must not authorize implementation.
- Model mutation of input cannot enlarge allowed paths or alter commands.
- Validation changing source or candidate must not produce accepted-looking evidence.
- A moved issue revision or expired lease must not complete stale work.

### Task 1: Candidate and validation primitives

**Files:** internal/repoworkspace/candidate.go, candidate_test.go, validation.go, validation_test.go and platform process helpers.
**Interfaces:** Edit{Path,Content,Delete}; Candidate{BaseSHA,CandidateSHA,TreeSHA,DiffHash,ChangedPaths}; PrepareCandidate(ctx,dir,base,allowed,edits); ValidateCandidate(ctx,dir,candidate,commands) -> []CommandResult.
- [x] Write real Git edit/delete/new/ignored-file, scope/symlink/empty tests and real command success/failure/mutation/timeout/env tests.
- [x] Run tests and observe missing behavior; implement strict candidate and bounded command APIs.
- [x] Run go test ./internal/repoworkspace -count=1 -timeout 3m; expected PASS; commit.

### Task 2: Claimed workflow implementation

**Files:** internal/ghtriage/implementation.go, implementation_input.go and tests; transactional completion/current-work guards in verification and workflowcase.
**Interfaces:** ImplementationModel.Implement(ctx,ImplementationInput) -> ImplementationOutput; Implementer.Prepare(ctx,caseID,sourceRepo,workspaceRoot) -> ImplementationResult.
- [x] Write tests for authorized candidate, exact review bindings, grants/revision/source, replay/concurrency, expired lease and model mutation; observe RED.
- [x] Implement loader and claimed execution. Completion evidence leaves task awaiting verification.
- [x] Run go test ./internal/ghtriage -count=1 -timeout 3m; expected PASS; commit.

### Task 3: CLI and delivery

**Files:** internal/ghtriage/climodel/adapter.go/tests, cmd/summa42-box/ghimplement.go/tests/main.go, README.md.
- [x] Write strict output adapter/CLI configuration regressions; observe RED.
- [x] Add Implement adapter and run-gh-implement command with documented local supervised scope.
- [x] Run affected tests, full available suite with known socket exclusion, affected race, vet/build/diff; expected PASS. Run final Astra review and fix important findings with RED→GREEN. Publish identical tested tree as a new draft PR and observe CI. Do not merge this new PR without owner approval.


## Delivery and review record

All three implementation tasks are complete. Final review by gpt-6-astra found two Important issues: completion errors requeued already executed work, and validation could mutate the original source without observation. Fixes use atomic Attempt-subject evidence indexing, staged completion recovery under a live fence, and source state fingerprints. Expired staged work requires operator recovery.

Two initially Minor findings were regraded Important because permitted edits could fail and recorded output could not replay: stream diff hashing and valid UTF-8 output bounds fix both. Investigation additionally found that embedded bytes.Buffer.ReadFrom bypassed the bounded writer during subprocess output copying; the buffer now exposes only its bounded writer interface. Each fix has a behavioral RED→GREEN regression. No final re-review was requested.

Rulings: choose structured edits as the first supervised implementer (limited tasks may later need a full coding executor); continue with recommended/native execution under owner authorization (owner can redirect the reviewable PR); use transactional work/revision/grant completion guards (future callers must preserve correct invariants); regrade the two output/diff issues by their effect on valid work and recovery (more validation was required); preserve UNENFORCED trusted native execution, future code acceptance/publication, and Linux runtime validation scope (containment/integrations and other-platform runtime behavior need separate milestones).

Local full-command failure is limited to the two unchanged Unix socket environment tests. The available full suite excludes precisely those tests; CI gates remain unchanged. Final verification commands and CI results are recorded in the PR.
