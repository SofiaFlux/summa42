# GitHub candidate publication implementation plan

> **For agentic workers:** Use superpowers:executing-plans. Native implementation selected per owner/AGENTS.md; Astra reviews the branch.

**Goal:** Publish exactly accepted candidates as managed branches and draft PRs through protected effects.
**Architecture:** Bounded immutable export; two GitHub operation providers; accepted-chain publisher with transactional dispatch guards and durable receipt recovery.
**Tech Stack:** Go 1.27, Git, SQLite, standard HTTP, existing Operations/policy/resource services.
**Spec:** docs/superpowers/specs/2026-10-08-github-publication-design.md

## Global constraints

- No actual Summa participation/live publication today. Tests use HTTP transport fixtures.
- 64 changed paths, 256 KiB new contents, single-parent unsigned commit, exact SHAs.
- Managed branch, create-only ref, draft PR, deterministic evidence marker; no merge or issue closure.
- Ten-minute lease/nine-minute run; HTTPS, no redirects, 20-second HTTP request, 1 MiB response.
- Explicit Case grant/actions `github.repo.publish` and `github.pr.create`; policy approvals remain required.

## Review focus

- A commit API can normalize metadata: mismatch must block ref creation.
- Revocation can occur between preparation and dispatch: canonical guard must reject before commit boundary.
- Lost HTTP acknowledgement must not duplicate effects: lookup is read-only and missing results remain unknown.
- Existing branch/marker may belong to different content: reject conflicts and ambiguous matches.
- Paid/effectful work can finish before completion save: resume staged receipt under exact live fence.

### Task 1: Export and canonical operation guards
Files: internal/repoworkspace/export.go/tests; internal/operations/service.go and guard_test.go.
Interfaces: `ExportCandidate(ctx,dir,Candidate) (Export,error)`; `Export.Validate() error`; `PrepareWithGuard(ctx,PrepareRequest,execution.TaskGuard)` and `DispatchWithGuard(ctx,operationID,attemptID,execution.TaskGuard)`.
- [x] Write exact export/modes/delete/drift and preparation/dispatch rollback tests; run RED.
- [x] Implement bounded immutable export and optional guards at canonical mutation boundaries, preserving old APIs.
- [x] Run package tests GREEN and commit.

### Task 2: GitHub effect providers
Files: internal/ghpublish/client.go, providers.go, providers_test.go.
Interfaces: Config with APIBaseURL, Repository, CredentialSource, HTTPClient; `NewBranchProvider(Config)` and `NewPRProvider(Config)` implementing operations.Provider; BranchIntent with Export/base/head; PRIntent with exact head/base SHA, title/body.
- [x] Write HTTP RoundTripper tests for exact SHAs, create-only refs, draft identity, pagination, lost acknowledgement, mismatch, sanitized credentials and redirect rejection; run RED.
- [x] Implement bounded client, strict typed intent contracts, canonical costs, single effect writes and read-only lookups.
- [x] Run package tests GREEN and commit.

### Task 3: Accepted-chain publisher and CLI
Files: internal/ghtriage/publication.go/tests; cmd/summa42-box/ghpublish.go/tests and main.go; README.md.
Interfaces: `NewPublisher(cases,execution,verification,evidence,operations)`; `Publish(ctx,caseID,sourceRepo,baseBranch) (PublicationResult,error)`; run-gh-publish requires case/source/base/repository/credential-file.
- [x] Write real workflow tests for success, grant/acceptance rejection, exact chain, concurrent calls, unknown operations and interrupted completion; CLI invalid config tests; run RED.
- [x] Implement deterministic intents, guarded publication Task/effect slots, staged receipt/replay and explicitly composed provider CLI.
- [x] Run targeted/full/race/vet/build/diff; Astra review and one regression-backed fix pass.
- Delivery evidence: draft PR #20 records the exact published tree/head and full CI links; no merge.

## Rulings

Choose native execution, two protected slots and explicit publication grants. Cost: more canonical checks and separate recovery state; worker/CI/merge composition still needs later work. No live Summa execution today.

## Astra review and regression-backed fix pass

Fresh gpt-6-astra review of a5691673..165ef0f9 found no Critical and three Important findings. Each was independently reproduced RED before the fix and GREEN afterward:

1. Approval-required publication could not resume after owner approval. Add explicit Held/operation/approval IDs and nonzero CLI status; permit only the same live Attempt to resume its approval-bound Prepared slot when no effect is Dispatched/Unknown. Operations still consumes the exact approval and commits a single dispatcher claim.
2. Upstream triage/plan-review acceptance was not checked against completion evidence. Require the accepted triage decision in its exact completion, exactly one accepted plan-review completion verdict, and validate the original decision's repository/issue/revision/snapshot/disposition before claiming publication.
3. GitHub PR lookup accepted JSON null as exhaustive absence. Reject non-array/null pages, including null after a full page containing an exact match, before any write.

Minor test coverage follow-ups retained: unsupported commit headers, ambiguous/conflicting PR markers, and upstream challenge specifically between preparation and dispatch. Existing paths reject these cases conservatively; dedicated negative fixtures remain follow-up work. No final re-review; one independent review and one fix pass.

Ruling: keep two separately approved protected slots and ten-minute live-fence approval resumption. Cost: approvals must be acted on within the lease; asynchronous human approval continuation beyond lease expiry is future work. Full broad ghtriage race gate exceeded a five-minute package timeout after hundreds of tests; increase its explicit timeout to ten minutes rather than changing tests or CI.

## Final local validation

Final available suite: 43 packages passed, excluding exactly the two baseline Unix-socket tests forbidden by this host. Original full suite failed only those unchanged tests. Final affected package suite, go vet ./..., both CLI builds, SQLite persistence spike and git diff --check passed. Full affected race gate passed with explicit ten-minute timeout (ghtriage 426.773 s; ghpublish, operations and repoworkspace also passed). No live Summa provider was invoked. Published-head full CI, including Unix sockets and OCI, is tracked in PR #20.
