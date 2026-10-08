# Supervised local implementation

## Intent and scope

Continue the owner-authorized self-development roadmap after merged PR #19. A supervised command prepares an actual local candidate from an independently accepted v2 issue plan. It records the exact candidate and real validation results for subsequent code review. GitHub publication and task acceptance are separate milestones.

## Architecture and decisions

Use structured whole-file edits from the existing CLI model protocol, applied by deterministic code to a fresh pinned checkout. Prefer this to giving a coding shell the repository and authority: it makes the changed-file contract inspectable and testable. A full general coding-executor integration can follow without changing candidate evidence semantics.

A review-backed loader validates the exact assessment that created implementation work, ACCEPT record, Task/Attempt acceptance, original v2 plan, issue revision and source hash. It reuses existing plan validation against the preceding review Work ID. Both workspace.repo.write and workspace.test must remain in capabilities and actions. The supplied local repository must reproduce the cited context.

Materialize one implementation Task using the current Work idempotency key and accepted review Task's resource envelope. Claim a fenced 15-minute Attempt before model invocation. Total operation timeout is 14 minutes; model timeout is 2 minutes. Concurrent callers return busy. Execution evidence is atomically indexed by Attempt before completion. Completion-persistence failure leaves the Attempt leased; retry validates and completes the staged record without model or test calls under the same live fence. Expired staged work requires operator recovery and is never silently re-executed. Failure before durable staging retains the existing bounded retry semantics. Completed evidence is reusable without model or test calls. A stale revision or expired fence cannot complete work.

## Candidate preparation

Changes are 1–64 unique exact allowed regular file paths, safe UTF-8 content with no NUL, at most 256 KiB total; delete entries cannot contain content. Reject symlinks/submodules and symlinked parents before any edits. Validate all entries before modifying the checkout. Preserve existing executable mode, create new files as 0644. Stage only explicit changed paths, including explicitly allowed ignored new files, and create one local commit parented by base SHA with fixed bot identity, no hooks/signing/network. Reject empty changes. Evidence records base SHA, candidate SHA, tree SHA, diff SHA256 and sorted changed paths.

Run owner-selected command argv directly, without shell parsing, in that candidate checkout. Each command has a 60-second timeout and retains at most 16 KiB combined output with a truncation flag. Strip ambient environment and create private HOME/cache directories; commands inherit only PATH and explicit runtime defaults. Go module downloads disabled by default. Native commands are UNENFORCED trusted local execution, not a security sandbox. Timeout cancels the process group on Unix. Record exit code, timeout, output and passed status. Reject a changed HEAD, tracked/index modifications or untracked non-ignored files after validation; ignored build products may remain. Recompute candidate identity after commands. Observe source HEAD, index/worktree changes and non-ignored untracked contents before/after execution; reject source drift. Initial dirty source is permitted. Untracked observation is limited to 64 files and 256 KiB; ignored source data and arbitrary host metadata are not observed. Stream candidate diffs into SHA-256 without retaining whole patches in memory. Normalize captured output to valid UTF-8 and enforce the 16 KiB bound on the persisted representation. Test failure is recorded honestly and never means acceptance.

## Durable evidence and CLI

Implementation record binds schema, Case/Work/Task/Attempt, plan/context/review evidence, base/candidate/tree/diff, paths, command argv/results and all-passed. Completion stores only this exact evidence. Task remains awaiting verification; no automatic acceptance, workflow advancement, PR or merge. A restart reuses the manifest after validating all bindings and the current source/review contract. Failed preparation marks the Attempt failed and preserves its checkout for diagnosis.

Add run-gh-implement with required --case, --source-repo, --workspace-root and existing model flags. Workspace root and source are absolute; destination is root/Attempt ID. Configuration errors fail before model or canonical writes. This is a one-case supervised operation, not worker composition or monetary settlement.

## Verification

Real local Git fixtures prove edit/delete/new files, exact parent, unchanged source, path limits, ignored files, symlink rejection and no-change rejection. Real child commands prove success/failure, argv fidelity, credential stripping, timeout and post-test mutation detection. Workflow fixtures prove review binding, current grant/revision, source mismatch, duplicate claim, model input mutation isolation, manifest replay and expired lease rejection. CLI rejects missing/relative paths before startup.
