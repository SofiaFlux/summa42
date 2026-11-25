# M9-T6 Design: Separate OS Users for Producer and Reviewer

**Card:** `M9-T6` (`DESIGN` then `BUILD`) — [development plan](../plans/2026-11-11-development-plan.md)
**Date:** 2026-11-25
**Protected paths touched in subsequent BUILD PR(s):** `internal/modelrun/`, `scripts/setup-model-user.sh` (Rule R5: Owner review line by line).

## 1. Problem

Today, producer and reviewer model calls share one OS account (`summa42-model` / `SUMMA42_MODEL_USER`) and one `0700` home directory:

1. `registerModelFlags` (`cmd/summa42-box/main.go`) defaults both `--model-user` and `--review-model-user` to `SUMMA42_MODEL_USER`, and `run-gh-pipeline` (`cmd/summa42-box/ghpipeline.go`) never checks that the two resolved users differ.
2. Single-stage review commands (`run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review`, and `run-repo-*` aliases) and replacement reviewers (`cmd/summa42-box/fallbackreviewers.go`) also read `--model-user` defaulting to `SUMMA42_MODEL_USER`.
3. `climodel.Adapter.runOnce` (`internal/ghtriage/climodel/adapter.go`) only enables `Scratch: true` when `a.commandCode || a.profile != nil`; bare `--model-binary` calls run directly in the user's home directory.
4. `internal/modelrun` only signals `-pgid` on cancellation (`Cmd.Cancel`), not on normal completion (`Cmd.Wait`). A model call that spawns a background child and exits `0` leaves that process alive, and a `SIGKILL`-ed call leaves its scratch directory until the day-old sweep.
5. `scripts/setup-model-user.sh` creates only `summa42-model` and `summa42-validate`, and imports Copilot's GitHub OAuth credentials (`gho_`) without auditing whether the copied logins carry repository-write scopes.

Because both roles share one home directory and CLI state, a prompt-injected producer call can inspect or tamper with reviewer logins, configs, or temporary files, violating Vision Articles 6, 9, 17, 18 and plan Section 2.5.

## 2. Decisions

### 2.1 Distinct OS users and flag defaults

Summa42 introduces a dedicated reviewer OS user (`SUMMA42_REVIEW_MODEL_USER`, default `summa42-review` in `scripts/setup-model-user.sh`), pairwise distinct by both username and numeric UID from:
- the producer user (`--model-user` / `SUMMA42_MODEL_USER`),
- the validation user (`--validation-user` / `SUMMA42_VALIDATION_USER`),
- the Box owner user, and
- `root` (UID `0`).

Flag and subcommand behavior:
- **Do not add `--review-model-user` again:** `registerModelFlags(flags, "review-model", ...)` in `cmd/summa42-box/main.go` already registers it. Change `registerModelFlags` so `prefix == "review-model"` defaults to `os.Getenv("SUMMA42_REVIEW_MODEL_USER")` instead of `SUMMA42_MODEL_USER`.
- **Single-stage review commands** (`run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review` and their `run-repo-*` aliases): keep taking `--model-user` (so `run-gh-pipeline`'s `modelFlagArgv(reviewer)` and explicit CLI invocations continue to use `--model-user`), but default it to `os.Getenv("SUMMA42_REVIEW_MODEL_USER")`. Refuse startup if `SUMMA42_MODEL_USER` or `SUMMA42_VALIDATION_USER` is set and resolves to the same UID/username as the reviewer user.
- **Producer commands** (`run-worker`, `run-gh-plan`, `run-gh-implement`, `run-repo-*` aliases): keep defaulting `--model-user` to `os.Getenv("SUMMA42_MODEL_USER")` and refuse startup if `SUMMA42_REVIEW_MODEL_USER` is set and resolves to the same UID/username.
- **`run-gh-pipeline`** (`cmd/summa42-box/ghpipeline.go`): resolve `producer`, `reviewer`, and `validation` users via `modelrun` and refuse startup unless all three have distinct usernames and UIDs. Replacement reviewers (`fallbackConfig` in `cmd/summa42-box/fallbackreviewers.go`) copy `reviewer`, so they inherit the distinct reviewer user.
- **`modelrun.ResolveReviewerUser(name string, others ...*user.User) (*user.User, error)`**: wraps `ResolveRunAsUser(name)` and rejects any match (`Uid` or `Username`) against `others`.

### 2.2 Setup script and login scope audit (`scripts/setup-model-user.sh`)

1. Add `REVIEW_USER="${SUMMA42_REVIEW_MODEL_USER:-summa42-review}"` and `SUDOERS_REVIEW_FILE="/etc/sudoers.d/summa42-review"`.
2. Validate `REVIEW_USER` with `valid_model_user` and verify `MODEL_USER`, `REVIEW_USER`, `VALIDATE_USER`, `OWNER`, and `root` are pairwise distinct before creating anything, and again by UID and home directory after `ensure_user`.
3. Create `summa42-review` as a `nologin` system user with home `/var/lib/summa42-review` (`0700`) and `$REVIEW_HOME/tmp` (`0700`), and install `/etc/sudoers.d/summa42-review` using the same validated `install_sudoers` flow (`!log_allowed`).
4. **Role-separated CLI auth import:** run `import_cli` separately for producer (`$MODEL_HOME`) and reviewer (`$REVIEW_HOME`) — interactively asking per CLI and role, and under `--yes` copying non-Copilot CLI auth files to both homes while continuing to skip Copilot.
5. **Scope audit (`audit_login_scopes`) before any file is copied into `$MODEL_HOME` or `$REVIEW_HOME`:**
   - Keep `scan_secrets` (refusing `ghp_`, `github_pat_`, `ghu_`, `ghs_`, `ghr_`, ADO PATs, `Bearer`/`Basic`, and private keys).
   - Inspect JSON auth payloads (`scope` / `scopes` string or array fields in `.gemini/oauth_creds.json`, `.claude/.credentials.json`, `.codex/auth.json`, etc.) and refuse any file declaring write-capable repository/CI scopes (`repo`, `public_repo`, `workflow`, `delete_repo`, `write:*`, `admin:*`, `vso.code_write`, `vso.code_manage`, `vso.build_execute`).
   - For Copilot (`.config/github-copilot/{hosts,apps}.json`, `.copilot/config.json`, which hold `gho_` GitHub OAuth tokens whose scopes are stored server-side on GitHub rather than inside the JSON): extract any `gho_` token; if `curl` can query `https://api.github.com/user` read-only (`HEAD`/`GET`), inspect `X-OAuth-Scopes` and refuse if any repository-write scope (`repo`, `public_repo`, `workflow`, `delete_repo`, `write:*`, `admin:*`) is present; if scopes cannot be verified read-only (offline or non-200), fail closed and drop/refuse the Copilot login copy rather than copying an unaudited GitHub write credential.
6. **Script-level isolation verification:** prove `review` cannot read Box secrets, `review` cannot read `$MODEL_HOME` or its login files, `model` cannot read `$REVIEW_HOME` or its login files, and `validate` cannot read either `$MODEL_HOME` or `$REVIEW_HOME`.

### 2.3 Startup cross-home isolation probe (`internal/modelrun/preflight.go`)

Extend `modelrun` probe registration with role-to-home cross checks:
- `modelrun.RegisterRoleHomes(producerHome, reviewerHome string)` (or `Spec.Unreadable` populated from the configured opposite role's `HomeDir`):
  - When `Command` runs as the producer user (`producerHome != ""` and `reviewerHome != ""`), `reviewerHome` is included in the required unreadable paths probed via `test -r <reviewerHome>` (and fails closed if readable or missing).
  - When `Command` runs as the reviewer user, `producerHome` is included in the required unreadable paths probed via `test -r <producerHome>`.
  - Both `RegisterRoleHomes` and explicit `Spec.Unreadable` flow through the same `probeIsolation` cache keyed by `(username, path)`, so `TestReviewerCannotReadProducerHome` and `TestProducerCannotReadReviewerHome` exercise the real launcher path.

### 2.4 Per-call scratch, cleanup, and survivor detection (`internal/modelrun/`)

#### Decision: Per-call cgroup v2 (when delegated) + per-call process group fallback

**Every model call runs with `Scratch: true`:**
- In `internal/ghtriage/climodel/adapter.go`, set `Scratch: true` unconditionally in `modelrun.Spec` (not only for `commandCode || profile != nil`). Internal probes (`probeTest`) remain `Scratch: false`.
- Instead of choosing the scratch directory name inside the model shell via `mktemp -d` alone, the Box generates a 128-bit random call ID (`s42.<hex>`) and passes it to the model-side prologue, which creates `$__s42_home/tmp/run.s42.<hex>` with `mkdir -m 0700` (failing closed if it already exists or `$__s42_home/tmp` is a symlink). The Box therefore knows the exact per-call scratch path without parsing or trusting model output.

**Mechanism choice (cgroup v2 per call vs process group per call):**
- **Primary mechanism — per-call cgroup v2 (Linux systemd hosts with `Delegate=yes` in `deploy/systemd/summa42-serve.service`):**
  - At `modelrun.Command` / `Start`, check whether the Box's own cgroup v2 directory (from `/proc/self/cgroup`) is writable by the Box user.
  - When writable, create a child cgroup `<box-cgroup>/call-<id>`, open its directory fd, and launch `sudo` with `SysProcAttr{UseCgroupFD: true, CgroupFD: fd}` (Go standard library). Because `sudo` and the target model user inherit the child cgroup and lack `CAP_SYS_ADMIN` to migrate out, every descendant — including processes that call `setsid()`, `setpgid()`, or double-fork — stays inside `<box-cgroup>/call-<id>`. Concurrent calls have disjoint cgroups, so cleanup never touches sibling calls.
  - On `Cancel()` or `Wait()` completion (including exit `0`), kill the entire cgroup (`cgroup.kill` when supported by the kernel, otherwise `SIGKILL` every PID in `cgroup.procs` until empty), remove the per-call scratch directory from the Box side via `sudo -n -u <user> -- /bin/sh -c 'chmod -R u+rwX -- "$1" 2>/dev/null; rm -rf -- "$1"' sh "$scratchDir"`, and remove the empty cgroup directory.
  - **Survivor detection (cgroup):** after the kill + short reap poll, read `cgroup.procs`. Ignore confirmed `'Z'`/`'X'` zombies in `/proc/<pid>/stat` (for containers whose PID 1 does not reap); any remaining live PID fails `Wait()` with `ErrSurvivorDetected` (and leaves the cgroup directory intact for inspection).
- **Fallback mechanism — per-call process group (hosts without cgroup delegation, e.g., CLI runs outside systemd, containers, CI):**
  - Launch keeps `pgid` from `__S42_PGID=<pgid>`.
  - On `Wait()` completion (even exit `0`), unconditionally run `sudo -n -u <user> -- /bin/kill -KILL -- -<pgid>` (in addition to `Cancel()`'s `TERM` -> `killGrace` -> `KILL`) and remove `$scratchDir` via `sudo -n -u <user>`.
  - **Survivor detection (process-group fallback):** after killing `-pgid`, check `/bin/kill -0 -- -<pgid>` as `<user>` and scan `/proc` as `<user>` for any live (non-zombie) process of `<user>` whose `cwd` or `environ` `TMPDIR` references `$scratchDir` (catching `setsid()` children that kept the scratch dir/env). If any non-zombie survivor is found, kill it and return `ErrSurvivorDetected`.
  - Per Rule R7, documentation and startup logs clearly state that escaping `-pgid` via `setsid()` + `cd /` + `env -i` is only contained when the Box runs with cgroup v2 delegation (`Delegate=yes`), not in fallback mode.

Why this choice over process-group-only or user-wide `kill -KILL -1`:
- `kill -KILL -1` cannot run when multiple model calls execute concurrently as the same role user (and `M9-T6` explicitly forbids reusing it).
- Process-group-only cannot contain a `setsid()` child even on a supported systemd Linux VM.
- Using `SysProcAttr.UseCgroupFD` requires zero new `root` sudoers rules (only `Delegate=yes` on the Box's systemd unit) while degrading cleanly to per-call `-pgid` + scratch cleanup in non-systemd environments and unit tests.

## 3. Scope and Out of Scope

- **In scope (`M9-T6` BUILD):** `internal/modelrun/`, `scripts/setup-model-user.sh`, `scripts/test_setup_model_user.py`, `internal/ghtriage/climodel/adapter.go`, `cmd/summa42-box/` (`main.go`, `ghpipeline.go`, `ghplan_review.go`, `ghcode_review.go`, `fallbackreviewers.go`, `serve_deploy_test.go`), `deploy/systemd/summa42-serve.service` (`Delegate=yes`), `deploy/serve.env.example` (`SUMMA42_REVIEW_MODEL_USER`), `internal/repoworkspace/realhost_test.go`, and `README.md`.
- **Out of scope:** Model egress firewall/proxy (`M9-T7`), OCI validation (`M9-T1`), and per-Project manifests (`M2-T3`).

## 4. Test Plan

1. `TestReviewerCannotReadProducerHome`: `modelrun.Command` as reviewer fails closed when `producerHome` is readable, and passes when `0700` owned by producer.
2. `TestProducerCannotReadReviewerHome`: symmetric check for producer against `reviewerHome`.
3. `TestSameUserForBothRolesRefused`: `ResolveReviewerUser`, `run-gh-pipeline`, and single-stage review commands (`run-gh-triage-review`, `run-gh-plan-review`, `run-gh-code-review`) reject identical username or UID across producer, reviewer, and validation users.
4. `TestNothingSurvivesAModelCall`: a model call that spawns a background child and exits `0` (and one that ignores `TERM` on cancel) has its child killed, its scratch directory removed, and any simulated survivor triggers `ErrSurvivorDetected` (covering both cgroup-path and process-group fallback logic).
5. `realhost` tests (`//go:build realhost`): verify with real `summa42-model` and `summa42-review` users that neither home is readable by the other role, and that a background child spawned by a model call is terminated and its scratch directory removed on return and cancel.
6. `scripts/test_setup_model_user.py`: verify `review` user creation/sudoers generation, cross-home unreadability checks, and `audit_login_scopes` dropping/refusing write-scoped logins.
