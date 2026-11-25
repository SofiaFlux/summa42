# M9-T6 Design: Separate OS Users for Producer and Reviewer

**Card:** `M9-T6` (`DESIGN` then `BUILD`) — [development plan](../plans/2026-11-11-development-plan.md)
**Date:** 2026-11-25
**Protected paths touched in subsequent BUILD PR(s):** `internal/modelrun/`, `scripts/setup-model-user.sh` (Rule R5: Owner review line by line).

## 1. Problem

Today, producer and reviewer model calls share one OS account (`summa42-model` / `SUMMA42_MODEL_USER`) and one `0700` home directory:

1. `registerModelFlags` (`cmd/summa42-box/main.go`) defaults both `--model-user` and `--review-model-user` to `SUMMA42_MODEL_USER`, and `run-gh-pipeline` (`cmd/summa42-box/ghpipeline.go`) never checks that the two resolved users differ.
2. Single-stage review commands (`run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review`, and `run-repo-*` aliases), replacement reviewers (`cmd/summa42-box/fallbackreviewers.go`), and the Copilot review executor (`run-worker` -> `buildCopilotExecutorFromEnv`) also read `SUMMA42_MODEL_USER`.
3. `deploy/summa42-serve.sh` falls back to `${SUMMA42_REVIEW_MODEL_USER:-$SUMMA42_MODEL_USER}` instead of requiring a distinct reviewer user.
4. `climodel.Adapter.runOnce` (`internal/ghtriage/climodel/adapter.go`) only enables `Scratch: true` when `a.commandCode || a.profile != nil`; bare `--model-binary` calls run directly in the user's home directory.
5. `internal/modelrun` only signals `-pgid` on cancellation (`Cmd.Cancel`), not on normal completion (`Cmd.Wait`). A model call that spawns a background child and exits `0` leaves that process alive, and a `SIGKILL`-ed call leaves its scratch directory until the day-old sweep.
6. `scripts/setup-model-user.sh` creates only `summa42-model` and `summa42-validate`, and imports Copilot's GitHub OAuth credentials (`gho_`) without auditing whether the copied logins carry repository-write scopes.

Because both roles share one home directory and CLI state, a prompt-injected producer call can inspect or tamper with reviewer logins, configs, or temporary files, violating Vision Articles 6, 9, 17, 18 and plan Section 2.5.

## 2. Decisions

### 2.1 Distinct OS users and flag defaults

Summa42 introduces a dedicated reviewer OS user (`SUMMA42_REVIEW_MODEL_USER`, default `summa42-review` in `scripts/setup-model-user.sh`), pairwise distinct by **username, numeric UID, and `HomeDir`** from:
- the producer user (`--model-user` / `SUMMA42_MODEL_USER`),
- the validation user (`--validation-user` / `SUMMA42_VALIDATION_USER`),
- the Box owner user, and
- `root` (UID `0`).

User resolution and validation helpers (`internal/modelrun/modelrun.go`):
- Add `ResolveReviewerUser(name string, others ...*user.User) (*user.User, error)`: wraps `ResolveRunAsUser(name)` and rejects any match on `Uid`, `Username`, or `HomeDir` against `others`.
- Update `ResolveValidationUser` to also reject a `HomeDir` collision with `modelUser` (and accept optional extra `*user.User` exclusions so `reviewerUser` can be excluded too).

Flag, env, and subcommand behavior (`cmd/summa42-box/`):
- **Do not add `--review-model-user` again:** `registerModelFlags(flags, "review-model", ...)` in `cmd/summa42-box/main.go` already registers it. Change `registerModelFlags` so `prefix == "review-model"` defaults to `os.Getenv("SUMMA42_REVIEW_MODEL_USER")`, and update `prefixedModelConfig` to emit role-specific errors naming `--review-model-user` / `SUMMA42_REVIEW_MODEL_USER` (keeping `climodel.New` compatibility with `internal/ghtriage/climodel/adapter_test.go`).
- **Single-stage review commands** (`run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review` and their `run-repo-*` aliases): keep taking `--model-user` (so `run-gh-pipeline`'s `modelFlagArgv(reviewer)` and explicit CLI callers continue to use `--model-user`), but default it to `os.Getenv("SUMMA42_REVIEW_MODEL_USER")` with reviewer-specific error messages. For standalone CLI invocations, refuse startup if `SUMMA42_MODEL_USER` or `SUMMA42_VALIDATION_USER` is set and resolves to the same UID/username/home as the reviewer user.
- **Producer commands** (`run-worker`, `run-gh-plan`, `run-gh-implement`, `run-repo-*` aliases): keep defaulting `--model-user` to `os.Getenv("SUMMA42_MODEL_USER")`. For standalone CLI invocations, refuse startup if `SUMMA42_REVIEW_MODEL_USER` is set and resolves to the same UID/username/home.
- **In-process `run-gh-pipeline` stage calls (`cmd/summa42-box/ghpipeline.go`):** `run-gh-pipeline` resolves `producer`, `reviewer`, and `validation` users up front and refuses startup unless all three are pairwise distinct by username, UID, and `HomeDir`. Because `run-gh-pipeline` calls `runGHTriageReview`, `runGHPlanReview`, `runGHCodeReview`, `runWorker`, `runGHPlan`, and `runGHImplement` **in-process** (where `os.Getenv("SUMMA42_MODEL_USER")` / `os.Getenv("SUMMA42_REVIEW_MODEL_USER")` may hold defaults that differ from explicit CLI overrides like `--model-user=custom-prod --review-model-user=summa42-model`), pass an internal pipeline-validated context marker (or unexported stage options) so in-process stages rely on `run-gh-pipeline`'s pairwise validation instead of re-reading env defaults.
- **Replacement reviewers (`cmd/summa42-box/fallbackreviewers.go`):** `fallbackConfig` copies `reviewer`, so every fallback reviewer automatically runs as the validated reviewer user.
- **Validation (`cmd/summa42-box/ghimplement.go`, `internal/repoworkspace/validation.go`):** `NewValidator` and `run-gh-implement` also verify `validationUser` differs from `SUMMA42_REVIEW_MODEL_USER` (when set) and include `reviewerHome` in `Spec.Unreadable`.
- **Copilot review executor (`cmd/summa42-box/main.go:978`, `internal/executors/copilot.go`):** `buildCopilotExecutorFromEnv` executes `ado.pr.review` (an independent PR review), so when `SUMMA42_COPILOT_PATH` is set it must resolve `SUMMA42_REVIEW_MODEL_USER` and refuse startup if unset or identical to `modelConfig.RunAsUser` (producer).
- **Deploy scripts (`deploy/summa42-serve.sh`, `deploy/serve.env.example`):** require `SUMMA42_REVIEW_MODEL_USER` in `need`, pass `--review-model-user "$SUMMA42_REVIEW_MODEL_USER"` (removing the `:-$SUMMA42_MODEL_USER` fallback), and update `cmd/summa42-box/serve_deploy_test.go`.

### 2.2 Setup script and login scope audit (`scripts/setup-model-user.sh`)

1. Add `REVIEW_USER="${SUMMA42_REVIEW_MODEL_USER:-summa42-review}"` and `SUDOERS_REVIEW_FILE="/etc/sudoers.d/summa42-review"`.
2. Validate `REVIEW_USER` with `valid_model_user` and verify `MODEL_USER`, `REVIEW_USER`, `VALIDATE_USER`, `OWNER`, and `root` are pairwise distinct before creating anything, and again by UID and home directory after `ensure_user`.
3. Create `summa42-review` as a `nologin` system user with home `/var/lib/summa42-review` (`0700`) and `$REVIEW_HOME/tmp` (`0700`), and install `/etc/sudoers.d/summa42-review` using `install_sudoers` (`!log_allowed`).
4. **Role-separated CLI auth import:** run `import_cli` separately for producer (`$MODEL_HOME`) and reviewer (`$REVIEW_HOME`) — asking per CLI and role interactively, and under `--yes` copying non-Copilot CLI auth files to both homes while continuing to skip Copilot.
5. **Scope audit (`audit_login_scopes`) before copying and on re-run:**
   - Keep `scan_secrets` (refusing `ghp_`, `github_pat_`, `ghu_`, `ghs_`, `ghr_`, ADO PATs, `Bearer`/`Basic`, and private keys).
   - Walk JSON auth files recursively (via `python3`) for `scope` / `scopes` string or array values (covering `.gemini/oauth_creds.json`, `.claude/.credentials.json`, `.codex/auth.json`, etc.) and reject any file declaring repository-write or CI-write scopes (`repo`, `public_repo`, `workflow`, `delete_repo`, `write:*`, `admin:*`, `vso.code_write`, `vso.code_manage`, `vso.build_execute`).
   - For Copilot (`.config/github-copilot/{hosts,apps}.json`, `.copilot/config.json`, which hold `gho_` GitHub OAuth tokens whose scopes live server-side on GitHub): extract any `gho_` token; check `X-OAuth-Scopes` via a testable `check_github_oauth_scopes` helper using `curl -sS -I -K -` (passing `header = "Authorization: token <tok>"` on stdin so the token never appears in `ps` / `/proc/<pid>/cmdline`). If a `gho_` token is present and scopes include any write scope or cannot be verified read-only (offline or non-200), **drop only the Copilot token files** (`hosts.json`, `apps.json`, `config.json` containing `gho_`) with a clear warning, without aborting unrelated CLIs or blocking `.copilot/mcp-config.json` (which contains no `gho_` token).
   - Drop/skip any over-scoped CLI with a warning rather than aborting other CLIs, and on re-run audit existing login files inside `$MODEL_HOME` and `$REVIEW_HOME` to remove any previously copied write-scoped files.
6. **Script-level isolation verification:** prove `review` cannot read Box secrets, `review` cannot read `$MODEL_HOME` or its login files, `model` cannot read `$REVIEW_HOME` or its login files, and `validate` cannot read either `$MODEL_HOME` or `$REVIEW_HOME`.

### 2.3 Startup cross-home isolation probe (`internal/modelrun/preflight.go`)

Extend `modelrun` probe registration with role-to-home cross checks:
- Add `modelrun.RegisterRoleHomes(producerHome, reviewerHome string)` (resettable by passing `""`, and independent of `RegisterProbe` so unit tests that clear `RegisterProbe` or register only role homes still trigger `probeIsolation` — update `preflight.go:52` accordingly).
- Resolve `producerHome` / `reviewerHome` once role users are known in `cmd/summa42-box` (after flag parsing, not only in `main()` before flags; `run-gh-pipeline` registers both, single-stage review commands register `SUMMA42_MODEL_USER`'s home when set, and `run-gh-implement` / `NewValidator` pass both homes in `Spec.Unreadable` for `summa42-validate`).
- When launching as the producer user and `reviewerHome != ""`, probe `reviewerHome` as required-unreadable; when launching as the reviewer user and `producerHome != ""`, probe `producerHome` as required-unreadable.
- Check **both** `test -r <path>` and `test -x <path>` (a `0711` directory fails `test -r` while still allowing traversal to files inside it), plus optional known login files under the opposite role's home. Both `RegisterRoleHomes` and explicit `Spec.Unreadable` flow through the `probeIsolation` cache keyed by `(username, path)`.

### 2.4 Per-call scratch, cleanup, and survivor detection (`internal/modelrun/`)

#### Decision: Per-call cgroup v2 (when delegated) + per-call process group fallback

**Every model call runs with `Scratch: true`:**
- In `internal/ghtriage/climodel/adapter.go`, set `Scratch: true` unconditionally in `modelrun.Spec` (covering bare `--model-binary`, `--model-commandcode`, and `--model-profile`). `CopilotExecutor` and `modelprofile` already set `Scratch: true`; internal probes (`probeTest`) remain `Scratch: false`.
- Instead of choosing the scratch directory name inside the model shell via `mktemp -d` alone, the Box generates a 128-bit random call ID (`s42.<hex>`) and passes it to the model-side prologue, which rejects symlinked `$__s42_home/tmp` and creates `$__s42_home/tmp/run.s42.<hex>` via `mkdir -m 0700` (failing closed if it already exists). The Box knows the exact scratch path without trusting model output.
- Add `chmod -R u+rwX` before `rm -rf` in the day-old sweep (`modelrun.go:245`) so read-only files created by a crashed call can always be removed.

**Mechanism choice (cgroup v2 per call vs process group per call):**
- **Primary mechanism — per-call cgroup v2 (Linux systemd hosts with `Delegate=yes` in `deploy/systemd/summa42-serve.service`):**
  - On Linux (`//go:build linux`, stdlib-only using `syscall.Statfs` for `CGROUP2_SUPER_MAGIC = 0x63677270`, keeping `go.mod` unchanged per R5/R6), inspect `/proc/self/cgroup` (`0::<path>`) inside `Cmd.Start()` (not `Command()`, avoiding leaked FDs/dirs if `Start()` is never called or `checkGateAtStart` refuses).
  - Create `<box-cgroup>/call-<id>`, open its directory fd, and launch `sudo` with `SysProcAttr{UseCgroupFD: true, CgroupFD: fd}`, closing `fd` in the parent immediately after `c.Cmd.Start()` returns (and removing the dir on `Start()` failure). Because `sudo` and the target model user inherit the child cgroup and lack `CAP_SYS_ADMIN`, every descendant — including `setsid()`, `setpgid()`, and double-forked daemons — stays inside `<box-cgroup>/call-<id>`, disjoint from concurrent calls.
  - Note on systemd `cgroup.subtree_control` (`EBUSY` no-internal-processes rule): if `mkdir` or `CLONE_INTO_CGROUP` returns `EBUSY`/`ENOSYS`/`EINVAL`/`EPERM`, clean up, log the reason per R7, and fall back cleanly to `-pgid` mode. Also note that `sudo` PAM stacks on target hosts must not enable `pam_systemd` (the `realhost` test verifies cgroup containment end-to-end).
  - **Lifecycle (`Cancel()` vs `Wait()`):** `Cancel()` preserves `SIGTERM` to `-pgid` -> `killGrace` -> `cgroup.kill` (or, on kernels < 5.14 without `cgroup.kill`, killing the PIDs read from `cgroup.procs` via `sudo -n -u <user> -- /bin/kill -KILL ...`, never direct `syscall.Kill` from the Box UID which returns `EPERM`). On normal `c.Cmd.Wait()` completion, `Wait()` runs unconditional cleanup using `context.WithoutCancel(ctx)` with a bounded timeout (15s): kill any remaining processes in the cgroup (`cgroup.kill` or `sudo kill -KILL`), remove `$scratchDir` via `sudo -n -u <user> -- /bin/sh -c 'chmod -R u+rwX -- "$1" 2>/dev/null; rm -rf -- "$1"' sh "$scratchDir"`, verify no live survivors remain, and `rmdir` the cgroup.
  - **Survivor detection (cgroup):** after killing and a short reap poll, read `cgroup.procs`. Ignore confirmed `'Z'`/`'X'` zombies in `/proc/<pid>/stat` (parsed from the last `')'`); any remaining live PID leaves the cgroup directory for inspection and fails `Wait()` with `ErrSurvivorDetected`.
- **Fallback mechanism — per-call process group (hosts without cgroup delegation, e.g., CLI runs outside systemd, containers, CI):**
  - Keep `pgid` from `__S42_PGID=<pgid>`.
  - On `Wait()` completion (using `context.WithoutCancel(ctx)` with bounded timeout), if `pgid > 1`, unconditionally run `sudo -n -u <user> -- /bin/kill -KILL -- -<pgid>` (ignoring exit `1` / `No such process` just like `Sweep`) and remove `$scratchDir` via `sudo -n -u <user>`.
  - **Survivor detection (process-group fallback):** probe `/bin/kill -0 -- -<pgid>` as `<user>` (ignoring `'Z'`/`'X'` zombies) and scan `/proc` as `<user>` (excluding `$$` itself) for any live non-zombie process whose `/proc/<pid>/cwd` target or `/proc/<pid>/environ` `TMPDIR=` equals `$scratchDir` (explicitly NOT matching `/proc/<pid>/cmdline`, which contains the scanner's own `$scratchDir` argument). If any non-zombie survivor matches, kill it and return `ErrSurvivorDetected`.
  - Per Rule R7, startup logs and documentation explicitly state that escaping `-pgid` via `setsid()` + `cd /` + `env -i` is contained only when delegated cgroup v2 is active, not in fallback mode.
- **Testability seam (`internal/modelrun/seams.go`):** expose unexported cgroup root/probe/FD seams (reset via `t.Cleanup`) so `go test ./internal/modelrun/...` exercises `UseCgroupFD`, `cgroup.kill`, `cgroup.procs` survivor detection, `EBUSY` fallback, and `ErrSurvivorDetected` deterministically without root or systemd.

## 3. Scope and Out of Scope

- **In scope (`M9-T6` BUILD):**
  - `internal/modelrun/` (`modelrun.go`, `preflight.go`, `process_unix.go`, `seams.go`, `modelruntest/`, and unit tests),
  - `scripts/setup-model-user.sh` and `scripts/test_setup_model_user.py`,
  - `internal/ghtriage/climodel/adapter.go`,
  - `internal/repoworkspace/validation.go` and `internal/repoworkspace/realhost_test.go`,
  - `cmd/summa42-box/` (`main.go`, `ghpipeline.go`, `ghplan_review.go`, `ghcode_review.go`, `ghimplement.go`, `fallbackreviewers.go`, `main_test_env_test.go`, `serve_deploy_test.go`, and unit tests),
  - `deploy/systemd/summa42-serve.service` (`Delegate=yes`), `deploy/summa42-serve.sh`, `deploy/serve.env.example`,
  - `docs/operations/running-serve.md` and `README.md`.
- **Out of scope:** Model egress firewall/proxy (`M9-T7`), OCI validation (`M9-T1`), and per-Project manifests (`M2-T3`).

## 4. Test Plan

1. `TestReviewerCannotReadProducerHome`: `modelrun.Command` as reviewer fails closed when `producerHome` is `0755` or `0711` (traversable), and passes when `0700` owned by producer.
2. `TestProducerCannotReadReviewerHome`: symmetric check for producer against `reviewerHome` (and `summa42-validate` against `reviewerHome`).
3. `TestSameUserForBothRolesRefused`: `ResolveReviewerUser`, `run-gh-pipeline`, `run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review`, and `buildCopilotExecutorFromEnv` reject identical username, UID, or `HomeDir` across producer, reviewer, and validation users, while valid in-process pipeline overrides succeed.
4. `TestNothingSurvivesAModelCall`: a model call spawning a background child and exiting `0` (and one ignoring `TERM` on cancel) has its child killed and its scratch directory removed immediately; simulated survivors trigger `ErrSurvivorDetected`, covering both cgroup-path (via `seams.go`) and `-pgid` fallback logic.
5. `realhost` tests (`//go:build realhost` in `internal/repoworkspace/realhost_test.go`): verify with real `summa42-model`, `summa42-review`, and `summa42-validate` users that cross-role homes are unreadable, and that background/cancelled children are terminated and scratch directories removed immediately (updating the stale comment at `realhost_test.go:113` and `README.md:215`).
6. `scripts/test_setup_model_user.py`: verify `review` user creation/sudoers generation, cross-home unreadability checks, `audit_login_scopes` (mocking `check_github_oauth_scopes` for `gho_` offline/write/read-only cases while keeping `.copilot/mcp-config.json` import working), and removal of stale over-scoped logins on re-run.
