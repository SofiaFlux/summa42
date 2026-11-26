# M1A-T1 Design: Agentic Workspace Executor

**Card:** `M1A-T1` (`DESIGN`) — [development plan](../plans/2026-11-11-development-plan.md#m1a-agentic-execution)
**Date:** 2026-11-26 (revised after independent adversarial review by Claude)
**Status:** Proposed (awaiting Owner approval before `M1A-T8` and `M1A-T3` implementation)
**Vision & Plan Alignment:** [Vision](../../VISION.md) (Articles 6, 9, 10, 11, 17, 18, 21), [Plan Section 2.5](../plans/2026-11-11-development-plan.md#25-design-goal-no-runaway-agents-no-private-channels), [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary)

---

## 1. Problem

Every model invocation in Summa42's GitHub pipeline (`internal/ghtriage/`) is currently a short, read-only or tool-free call:

1. `internal/ghtriage/climodel/profile.go` requires every `Profile` to declare `ReadOnly` or `NoTools: true` and rejects write/auto-approve flags (`dangerousArgs`).
2. `internal/ghtriage/implementation.go` (`Implementer.prepare`) sends a bounded JSON snapshot (at most 32 files / 256 KiB) and accepts back a single JSON object (`ImplementationOutput`) with at most 64 complete-file edits. The model never runs compilers, test suites, linters, or exploration commands on the repository.
3. Meanwhile, trusted execution boundary primitives exist in `internal/teb/` (`DockerProvider`, `OCIRequest`, `EnforcedProxyProfile`, `ConnectProxy`, `ModelEgressAssessment`), along with `execution.Service.RenewLease` (`internal/execution/service.go`), pinned checkouts (`internal/repoworkspace/`), and executor abstractions (`internal/executors/executor.go`), but they are incomplete and not composed into an end-to-end sandboxed agentic implementation path.

As stated in [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary), governance must constrain the blast radius rather than crippling the agent: inside an isolated sandbox an agentic harness should have full write/build/test tools for hours, while everything crossing the sandbox boundary remains governed by deterministic trusted code.

---

## 2. Decisions

### 2.1 Sandbox: Per-Attempt OCI Container (`internal/teb`)

Each agentic implementation Attempt runs inside a dedicated, ephemeral OCI container managed by `teb.DockerProvider` (`internal/teb/docker_provider.go`, `internal/teb/oci.go`), started with `teb.EnforcedProxyProfile()` (`domain.EnforcementEnforced`).

#### Container hardening & runtime flags

Extend `OCIRequest` and `ExecutionRequest` (wiring the currently unused `ExecutionRequest.Environment` field) so `teb.DockerArgs` emits:

- **Root filesystem & privileges:** `--read-only`, `--cap-drop=ALL`, `--security-opt=no-new-privileges`, `--user=65532:65532`, `--init` (reaps zombies), `--pull=never` (no implicit registry pulls at run time), explicit `--entrypoint` (from the audited image spec), and `--log-driver=none` (prevents unbounded Docker `json-file` host disk growth).
- **Resource & kernel limits:** `--pids-limit`, `--memory`, `--memory-swap=<memory>` (no swap bypass), `--cpus`, `--shm-size=64m`, `--ulimit fsize=<cap>` and `--ulimit nofile=<cap>`.
- **Network & DNS isolation:** `--network=<attempt-net>` (per-Attempt internal IPv4-only Docker network) and `--dns=0.0.0.0` (disables Docker embedded DNS / outbound DNS).
- **Lifecycle tracking:** `--name=s42-attempt-<attemptID>`, `--cidfile=<box-run-dir>/<attemptID>.cid`, and `--label=summa42.attempt=<attemptID>`.
- **Writable mounts (workspace and ephemeral `tmpfs` caches only):**
  - `/workspace`: bind-mounted from a quota-limited Box-managed workspace directory (`--mount=type=bind,src=<attempt-workspace>,dst=/workspace`, `--workdir=/workspace`), populated by a **full clone** at `BaseSHA` (not shallow, avoiding bundle boundary/prerequisite pitfalls) with `core.hooksPath=/dev/null` and clean `git config` (no remotes or credentials).
  - `/out`: a separate Box-owned directory mounted **only** during the post-run bundle export step (not writable during agent execution, or mounted as `tmpfs`/`ro` during agent run and writable only to UID `65532` during the offline export step), so `/workspace` is never used to pass the bundle path to the host.
  - `/tmp` (including `HOME=/tmp/home`, `TMPDIR=/tmp`, `GOTMPDIR=/tmp/go-tmp`, `GOCACHE=/tmp/go-cache`, `GOMODCACHE=/tmp/go-mod`): `--tmpfs /tmp:rw,exec,nosuid,nodev,size=<cache-cap>,uid=65532,gid=65532`. Because Docker `--tmpfs` defaults to `noexec`, explicit `exec` is required so compiled Go test binaries run, and `tmpfs` size counts toward `--memory` (sizing guidance: for a Go repository, size `--memory` as `base_toolchain_ram + tmpfs_size`, e.g. `4 GiB` RAM with `1 GiB` `/tmp` tmpfs).
  - Optional pre-staged dependency cache mounts (e.g., module mirror) are mounted **read-only** (`readonly`) and validated before mount (`trustedPath`: regular directory owned by Box user, not a symlink, never under `~/.summa42`). No host-writable cache bind mounts are permitted, keeping `GuaranteeWorkspaceOnlyWrite` honest (writable mounts are strictly `/workspace` + ephemeral in-container `tmpfs`).
- **Environment & socket exclusion:**
  - `DockerArgs` structurally refuses any `Environment` key matching credential patterns (`*TOKEN*`, `*KEY*`, `*SECRET*`, `*PASSWORD*`, `*AUTH*` except explicitly allowlisted non-secret placeholder keys — see §2.3) and refuses any workspace/mount path containing symlinks or container runtime sockets (`docker.sock`, `podman.sock`, `/var/run/docker*`), extending `validateWorkspace` so it does not skip symlinks or non-`docker.sock` socket names.
  - `DockerBackend.Run` bounds captured `stdout` and `stderr` with a fixed-size capped reader (e.g., `256 KiB` tail/head ring) so container output cannot OOM the Box.

#### Daemon trust, rootless execution, and residual host risk

Running `docker` as the Box user against a root-owned `/var/run/docker.sock` grants effective root equivalence on the host. Therefore:

1. **Prescribed deployment:** run **rootless Docker or rootless Podman** under a dedicated unprivileged OS user with user-namespace mapping and the default seccomp profile.
2. **Socket wiring:** rather than clearing the entire `docker` CLI environment (`cmd.Env = []string{}` in `internal/teb/oci.go:123`, which breaks rootless `DOCKER_HOST` / `XDG_RUNTIME_DIR`), `DockerBackend` passes **only** the explicitly configured trusted `DOCKER_HOST` (or `XDG_RUNTIME_DIR`) and `PATH`/`HOME` needed by the rootless client, never arbitrary host env or credentials.
3. **Workspace UID ownership & cleanup:** under rootless userns mapping, UID `65532` inside the container maps to a sub-UID of the rootless daemon user, so the Box prepares `<attempt-workspace>` writable by that mapped UID and tears it down via an unprivileged cleanup container (`chown -R`/`rm -rf`) followed by Box `os.RemoveAll` fallback.
4. **Honest classification (Rule R7):** `EnforcementEnforced` means the full OCI sandbox + firewall + proxy configuration was applied and verified via `docker inspect` (§2.8); it does not claim immunity to Linux kernel or container-runtime CVEs, and `README.md` must state that residual kernel/daemon risk explicitly.

#### Host without Docker & explicit fallback policy

Summa42 **never** fakes `ENFORCED` containment when Docker/OCI is absent:

1. Project manifests (`M2-T3`) declare `agentic: required | preferred | off`.
2. If `teb.DockerProvider.Available(ctx)` is `false` (or rootless OCI is unavailable):
   - When `agentic == required` (or a Task declares `RequiredEnforcement: domain.EnforcementEnforced`), the Attempt **fails closed** (`teb.ErrProviderUnavailable`) and never runs write/auto-approve CLIs on the host.
   - When `agentic == preferred`, `Implementer` explicitly downgrades to the existing **complete-file JSON edit stage** (`internal/ghtriage/implementation.go`, `UNENFORCED` model call + host validation) and records the downgrade (`mode: json_edits`, `enforcement: UNENFORCED`) as immutable case evidence so a non-sandboxed run is never mistaken for an agentic sandbox run.

---

### 2.2 Container Image & Adoption Audit

Images are **Owner-approved Project data**, never agent-selected:

1. **Image reference:** the Project manifest (`M2-T3`) pins an immutable digest (`<registry>/<image>@sha256:<digest>`), installed/pulled beforehand by the Owner/operator.
2. **Image-adoption audit (mandatory before use):** before an image digest is accepted in a manifest or run, Box trusted code inspects (`docker image inspect`) and runs a read-only audit scan inside the image (`--network=none --user=65532:65532`) verifying:
   - no `VOLUME` declarations in the image config (prevents anonymous unquotaed host writes under `/var/lib/docker`),
   - expected `ENTRYPOINT` / `CMD` (not overridden to an arbitrary shell wrapper),
   - no setuid or setgid binaries (`find / -perm /6000`),
   - no `docker`, `podman`, `nerdctl`, `ssh`, or `sudo` binaries,
   - no pre-baked CLI auth/login files (`~/.claude`, `~/.codex`, `~/.gemini`, `~/.copilot`, `~/.commandcode`).
   - **Test:** `TestImageAuditRefusesSetuidOrContainerCLI`.
3. **Contents:** minimal Linux userland, `git`, the Project's pinned toolchain (e.g. Go 1.27, Python, Node), and the headless harness binary installed read-only (`0755`) for UID `65532`.

---

### 2.3 Network, Egress Proxy, Harness Auth, and Mediated Fetch (`M1A-T8`)

#### Container network isolation & host firewall

An `--internal` Docker network blocks internet routing, but containers on a bridge/internal network can still reach the host's gateway IP and any host daemon listening on `0.0.0.0` (e.g., Box control API, `sshd`, `ollama`, TCP daemons). To close this:

1. **Per-Attempt network lifecycle:** the Box allocates a dedicated `/29` internal IPv4-only Docker network (`--internal`, IPv6 disabled) for the Attempt, deletes it on teardown, and runs a startup reaper for any orphaned `s42-attempt-*` networks.
2. **Host firewall (`nftables`/`iptables`):** the Box (or a privileged helper configured during host setup alongside `setup-model-user.sh`) installs an `INPUT` rule scoped to the Attempt's subnet accepting **only** TCP to `<gateway-ip>:<attempt-proxy-port>` and dropping all other traffic to the host (including all other ports and loopback/metadata destinations). If the host cannot enforce this rule (e.g., unprivileged test host without firewall capability), `ModelEgressAssessment.DirectGeneralEgressBlocked` is `false` and `ENFORCED` agentic mode is **refused** (never silently degraded).
   - **Test:** `TestSandboxHasNoRouteButProxy` (verifies a container port-scan of the gateway IP and external/metadata IPs fails on every port except the Attempt's proxy listener).

#### Amending `M1A-T8`: plain-HTTP reverse proxy (not `CONNECT`) for credential injection & registry control

The `M1A-T8` card originally mentioned reusing the "existing CONNECT proxy" to inject credentials and enforce read-only access. An HTTP `CONNECT` tunnel is end-to-end opaque TLS: without TLS MITM interception (and trusting a Box CA inside containers), a `CONNECT` proxy **cannot** inject `Authorization` headers, inspect HTTP methods (`GET` vs `PUT`), or parse provider token usage.

Therefore, **this spec explicitly amends `M1A-T8`**:
- Keep `teb.DialAllowedTarget` / `publicEgressIP` DNS-rebinding protections from `internal/teb/egress_proxy.go`.
- Replace opaque `CONNECT` with a **per-Attempt HTTP reverse proxy** bound strictly to the Attempt's gateway IP (`<gateway-ip>:<ephemeral-port>`, reusing `NewConnectProxyOn` validation that refuses wildcard binds). Each Attempt gets its **own** proxy listener/instance so credentials, allowlists, lease checks, and usage counters bind 1:1 to a single `attemptID`.
- All proxy paths enforce:
  - **Strict `httputil.ReverseProxy.Rewrite` (never `Director`)** so client-supplied `Connection: Authorization` headers cannot trick Go into stripping Box-injected auth headers.
  - **Header allowlist:** only safe request headers (`Content-Type`, `Accept`, `User-Agent`, `Anthropic-Version`, `OpenAI-Beta`, etc.) pass; `Authorization`, `x-api-key`, `Proxy-Authorization`, `Cookie`, `Host`, org/project override headers, `Upgrade` (no WebSockets), and `Connection`-named headers are stripped.
  - **Request & path hygiene:** reject absolute-form request targets, `..`, `%2e`, `//`, encoded slashes (`%2f`), and match route prefixes strictly by whole path segment; set `CheckRedirect` to `http.ErrUseLastResponse` (never follow upstream redirects automatically); cap request body size and dial/header/idle timeouts; redact upstream `401`/`403` response bodies so upstream error messages cannot echo key fragments.
  - **Active-lease & hard-cap check on every request:** before forwarding any request, the proxy checks in-process budget/in-flight limits and verifies the Attempt lease is still active (immediately refusing upon `ErrLeaseInactive` or freeze rather than waiting for the next watcher poll).

#### First-slice harnesses, subscription auth (`CostPrepaidQuota`), and telemetry suppression

Until `M3-T2` (money ledger) ships, agentic Attempts allow **only** profiles classified as `resources.CostPrepaidQuota` (`internal/resources/service.go:20`) or free/local endpoints (never pay-per-token API profiles). Because subscription CLIs use OAuth tokens and may contact telemetry/updater hosts, **every** agentic harness must satisfy explicit base-URL + placeholder-credential + zero-background-egress rules, verified by a live OCI conformance probe (`summa42-box agentic-probe`) before adoption:

1. **First-slice supported harnesses for `M1A-T3`:**
   - **`claude` (Claude Code, Anthropic subscription OAuth):**
     - *In-container env:* `ANTHROPIC_BASE_URL=http://<gw>:<port>/provider/anthropic`, `ANTHROPIC_AUTH_TOKEN=s42-placeholder`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_TELEMETRY=1`, `DISABLE_AUTOUPDATER=1`, `DISABLE_ERROR_REPORTING=1`. No `~/.claude/.credentials.json` inside container.
     - *Proxy behavior:* allows only `POST /provider/anthropic/v1/messages` (and `/v1/messages/count_tokens`), swaps `s42-placeholder` for the Box-held OAuth bearer token (refreshing via `console.anthropic.com/v1/oauth/token` **outside** the container when near expiry), forces `Accept-Encoding: identity` (or decodes gzip before metering), sets `FlushInterval: -1` for SSE streaming, and parses `usage` tokens from the final JSON/SSE event into durable ledger state before acknowledging or advancing.
   - **`codex` (OpenAI Codex CLI, ChatGPT/Codex subscription or prepaid endpoint):**
     - *In-container env:* `OPENAI_BASE_URL=http://<gw>:<port>/provider/openai/v1`, `OPENAI_API_KEY=s42-placeholder`, `CODEX_DISABLE_TELEMETRY=1`, `NO_UPDATE_NOTIFIER=1`. No `~/.codex/auth.json` inside container.
     - *Proxy behavior:* allows only `POST /provider/openai/v1/responses` and `POST /provider/openai/v1/chat/completions`, swaps `s42-placeholder` for the Box-held credential/OAuth token (handling token refresh **outside** the container if using ChatGPT backend OAuth, or failing closed in `agentic-probe` if the installed `codex` version requires non-overridable ChatGPT endpoints), sets `FlushInterval: -1`, and fails closed if usage parsing fails on non-stream/stream completion.
2. **Unsupported in `M1A-T3` until proven by `agentic-probe`:**
   - `copilot` (requires multi-step GitHub OAuth `gho_` -> Copilot token exchange endpoints), `agy`, `commandcode`, and any harness that ignores `*_BASE_URL`, insists on reading local credential files, or emits un-suppressible telemetry to non-provider hosts. Adding another harness requires a passing OCI `agentic-probe` proving placeholder auth works with an empty `HOME` and **zero** denied egress signals.

#### Package registry path

- **Private projects (`data_class == "private"`):** **cache-only** (`--network` allows zero live registry egress; dependencies come solely from Box pre-staged read-only cache mounts or vendored trees), preventing exfiltration via module lookup URLs (e.g., `proxy.golang.org/<attacker-host>/<secret>/@v/list`).
- **Non-private projects:** plain-HTTP proxy endpoint (`GOPROXY=http://<gw>:<port>/registry/go`, `npm_config_registry=http://<gw>:<port>/registry/npm`, etc.) permitting **`GET` and `HEAD` only** (`PUT`/`POST`/`DELETE` refused), restricted strictly to module/package names enumerated in the Project manifest or repository lockfile (`go.mod`, `package-lock.json`, etc.), dialing upstream **HTTPS port 443 only** after `publicEgressIP` DNS validation, with per-Attempt request count and byte-transfer caps.
- **Test:** `TestRegistryPathCannotReachArbitraryModuleHost`.

#### Mediated fetch tool (`/s42/fetch`)

Exposed inside the sandbox via `http://<gw>:<port>/s42/fetch?url=...` (and a thin CLI/MCP helper), acting as hard containment:

- **URL validation (`net/url`):** must be `https` on port `443` (explicit or default); reject userinfo (`allowed.com@evil.com`), IP literals (both in the URL and in the Project manifest allowlist — also tighten `dialAllowedTarget` in `internal/teb/egress_proxy.go:64` so IP literals never bypass `publicEgressIP`), trailing-dot hosts (`example.com.`), non-ASCII / IDN hosts (or canonicalize and match exact ASCII punycode), and disallow automatic redirects (or re-validate every hop against the allowlist and `publicEgressIP`).
- **Domain allowlist:** exact lowercase host match against the Project manifest's `fetch_allowlist`.
- **Method, length, rate, size:** `GET` only, URL `<= 2048` bytes, per-Attempt cap (e.g. `50` requests), rate cap (e.g. `10/min`), decoded response body cap (e.g. `1 MiB` via `io.LimitReader` after decompression to stop gzip bombs).
- **Private data class refusal:** if Project or source `data_class == "private"`, **all** `/s42/fetch` calls fail closed (`403 Forbidden`) and trigger a rogue signal.
- **Evidence & untrusted provenance:** every allowed fetch records an immutable Box evidence object (`agent.fetch.v1` containing URL, status, SHA-256, byte count, and a bounded sanitized/size-capped copy up to e.g. `256 KiB` flagged untrusted for `M1A-T7` license parsing), and the HTTP response carries `X-Summa42-Untrusted: true`.
- **Signal deduplication (L5):** per-refusal rogue signals (`egress_denied`) are deduped/rate-limited per `(attemptID, host, reason)` so accidental/malicious noisy probing triggers containment cleanly without flooding the audit log or forcing artificial bypasses.

---

### 2.4 Write-Mode Harness Safety Gate (`internal/ghtriage/climodel/profile.go`)

`internal/ghtriage/climodel/profile.go` keeps `dangerousArgs` rejection and `ReadOnly`/`NoTools` mandatory for all existing profiles and native callers:

1. **Structuring `AgenticSpec`:**
   - Add an optional `Agentic *AgenticSpec` field (`WriteArgs []string`, `Harness string`) to `climodel.Profile`.
   - **`DraftProfile` refuses `Agentic`:** a model-drafted profile may never declare `Agentic`. `Agentic` profiles are accepted **only** from `BuiltinProfiles()` or Owner-trusted `ReadProfileFile` (`trustedPath` in `$SUMMA42_HOME/model-profiles/`).
   - **`WriteArgs` replaces `Args` (never appends):** appending would leave `--permission-mode plan` or `--deny-tool write` active by last-option-wins semantics or conflict with read-only runs. `Profile.Validate()` ensures `WriteArgs` does not contain `ReadOnly`, and matches a per-harness `WriteArgs` allowlist pinned by unit test (e.g. `codex` `--full-auto` / `claude` `--permission-mode acceptEdits`).
2. **Runtime enforcement gate (safe inside OCI and nowhere else):**
   - `climodel.Adapter` (host OS-user runner) **always refuses** any profile/call requesting `Agentic` mode (`TestAutoApproveRefusedOutsideSandbox`, `TestClimodelAdapterRefusesAgenticMode`).
   - Only the OCI Agentic Executor (`M1A-T3`), after `teb.Manager` / `teb.DockerProvider.Assess` + post-start `docker inspect` + `ModelEgressAssessment.Verified()` prove `domain.EnforcementEnforced` with all 12 guarantees, may invoke `WriteArgs` inside the container.
   - Because `profile.go` is a protected path (Rule R5), its change in `M1A-T3` requires line-by-line Owner review.

---

### 2.5 Result Extraction: Bundle Collection, `fsck`, and Box-Built Candidate

Because the agent had write access to `/workspace` (including `/workspace/.git`), **trusted Box code never executes `git`, hooks, or tools inside `/workspace`**.

1. **Candidate vs. uncommitted / intermediate commits:**
   - Intermediate commits the agent creates during its run serve solely as checkpoint evidence (`M1A-T4`).
   - Uncommitted working-tree changes left when the agent exits are **dropped** (never committed by the host or an extra helper); if no commit advances past `BaseSHA` (detached, unborn, or `HEAD == BaseSHA`), `git bundle create` is skipped and the Attempt records **no candidate** (incomplete/failed).
2. **Offline bundle export into Box-owned `/out`:**
   - Only after a normal exit or external wall-time expiry (never after break-glass freeze, containment, or lease loss — §2.6), a short offline step runs as UID `65532` in the container (`--network=none`, `--read-only`, `/workspace` mounted `ro`, `/out` mounted `rw`):
     `git -C /workspace -c core.hooksPath=/dev/null -c safe.directory=/workspace bundle create /out/candidate.bundle <BaseSHA>..HEAD` with `GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_NO_REPLACE_OBJECTS=1`.
3. **Host-side safe consumption of `/out/candidate.bundle`:**
   - The Box opens `/out/candidate.bundle` using `O_NOFOLLOW | O_NONBLOCK`, runs `fstat` to require a regular file (`!ModeSymlink`, `!ModeNamedPipe`, `!ModeDir`) with `st_size <= maxBundleBytes` (e.g. `64 MiB`), and streams through that file descriptor (`io.LimitReader`) into a fresh Box-owned temp file inside a size-limited Box scratch directory (quota/tmpfs) outside `/workspace` and `/out`.
   - Verify the file begins with `# v2 git bundle\n` or `# v3 git bundle\n`, then run `git bundle verify` inside a clean `git init` Box repo seeded with `BaseSHA`, under a wall-time cap and with:
     `GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_NO_REPLACE_OBJECTS=1`
     plus `-c core.hooksPath=/dev/null -c transfer.fsckObjects=true -c receive.fsckObjects=true -c core.bigFileThreshold=1m`, `fetch --no-tags --no-recurse-submodules`, and `index-pack --max-input-size=<maxPackBytes>` (protecting against compressed pack bombs).
   - Run `git fsck --strict --full` with explicit `fsck.*=error` severities (`fsck.zeroPaddedFilemode=error`, `fsck.gitmodulesSymlink=error`, `fsck.badName=error`, `fsck.duplicateEntries=error`).
4. **Tree inspection & Box-built single-parent Candidate:**
   - Reject merges or non-linear history (`BaseSHA..tip` must be linear with no merge commits; `BaseSHA` must be an ancestor of `tip`).
   - Walk `git diff-tree -r --no-renames -z <BaseSHA> <tip>` and reject:
     - any non-regular-blob mode (`120000` symlink, `160000` gitlink/submodule),
     - `.gitmodules`, new/modified `.gitattributes`, case-colliding paths, `.git` lookalikes (`git~1`, `.GIT`, HFS ignorable characters),
     - unauthorized executable-bit (`100755`) changes unless allowed by manifest,
     - binary or non-UTF-8 file contents,
     - paths outside `allowed_paths`/`allowed_prefixes` (or **protected pipeline paths**, §3.5),
     - path or content hygiene violations (using `texthygiene.Check("path of an edit", path)` plus a new exported `repoworkspace.CheckTreeHygiene(ctx, repoDir, baseSHA, tipSHA)` replacing unexported `checkEditHygiene` on `Edit` structs), and per-blob size/total object count caps (`ReadFile(..., maxBytes)`).
   - **Box builds the Candidate commit:** from the verified `tip^{tree}`, the Box creates **one** unsigned single-parent commit on `BaseSHA` in the Box-owned repo (`Summa42` committer/author identity and Box-written commit message, stripping hostile agent author/message text), producing a `repoworkspace.Candidate` compatible with `ExportCandidate` / `VerifyCandidate`.
   - Until `M1A-T5` lifts `Export.Validate` (`64` files / `256 KiB`), candidates exceeding `Export.Validate` limits are **held** rather than published.

---

### 2.6 Attempt Limits, Lease Renewal (`M1A-T2`), and Active Kill Watcher

Agentic Attempts run for hours while bounded across four axes:

| Limit | Mechanism | Enforcement Point |
| --- | --- | --- |
| **Wall time & liveness** | Short initial lease (e.g. `10m`) renewed via `execution.Service.RenewLease` (`M1A-T2`), plus external hard `max_attempt_wall_time` (`context.WithDeadline`) and an in-container dead-man wall timer (`timeout -s KILL <wall>`) | Outer Box context + `RenewLease` + in-container `timeout` |
| **Money & quota** | `RenewLease` invokes `leaseGate.CheckLeaseAllowedTx` and checks envelope/budget **in the same SQLite transaction** as updating `lease_expires_at` (`M1A-T2`). Proxy persists usage counters to SQLite before threshold checks. **Until `M3-T2` exists, only `resources.CostPrepaidQuota` / free profiles are allowed** | Pre-start eligibility + every `RenewLease` tx + proxy per-request check |
| **Compute** | OCI `--pids-limit`, `--memory`, `--memory-swap=<memory>`, `--cpus`, `--ulimit` | Kernel cgroups inside OCI runtime |
| **Disk** | Quota-limited workspace filesystem (XFS/ext4 project quota or pre-sized loop mount / tmpfs), bounded `/tmp` `tmpfs`, `--log-driver=none`, `--shm-size`, and no `VOLUME` in image | Filesystem quota + container runtime |

**Active Kill Watcher (`<= 5s` latency bound) & Crash Recovery:**
- A watcher goroutine checks every `<= 5s` for: break-glass `FROZEN` / safe mode, Attempt containment (`containment`), `RenewLease` failure (including `domain.ErrLeaseInactive` / `ErrStaleAttempt` when an already-expired lease has not yet been swept), or budget/quota exhaustion.
- On **any** trigger (or `ctx.Done()`), `DockerBackend.Run` and the watcher read the `--cidfile` and immediately run `docker kill <cid>` + `docker rm -f <cid>` (`context.WithoutCancel(ctx)` with bounded timeout). Stopping lease renewal alone is never enough.
- **No export on kill:** if killed by freeze, containment, or lease loss, bundle export is **not** run; only normal completion or external wall-time expiry may enter bundle collection.
- **Startup orphan reaper:** at Box / `serve` startup, any leftover containers or networks carrying label `summa42.attempt` whose Attempt is no longer `ACTIVE` are killed and removed.

---

### 2.7 Existing One-Shot Stages & Residual Validation Risk

1. **One-shot stages stay:** triage (`stage2`, `reviewer`), grounded planning (`planner`, `planreview`), and independent code review (`codereview` as `summa42-review`) remain short `read_only` / `no_tools` calls. Complete-file JSON implementation (`internal/ghtriage/implementation.go`) stays as the explicit `agentic == preferred` fallback on Docker-less hosts.
2. **Candidate validation exposure (`H9` residual risk until `M9-T1`):** until `M9-T1` moves `summa42-validate` into an OCI container, `summa42-validate` executes agent-written tests on the host (as an isolated OS user without credentials, but with host network until `M9-T7`). Until `M9-T1` and `M9-T7` land:
   - `M1A-T3` must run validation with network access blocked where host rules permit (or explicitly mark validation `PARTIAL` / host-networked in `README.md` per Rule R7),
   - pre-publication checks must verify Project CI does not expose deployment secrets to untrusted branch PR builds,
   - `README.md` must explicitly state that agentic candidates execute on host `summa42-validate` until `M9-T1`.

---

### 2.8 Non-Circular Enforcement Verification

To ensure `EnforcementEnforced` is never self-reported from requested flags (`teb/profile.go` lines 3–5):

1. After starting the container (before unlocking or keeping `Agentic` execution results), `DockerProvider` runs `docker inspect <cid>` and verifies every field matches the request (`HostConfig.ReadonlyRootfs == true`, `CapDrop == ["ALL"]`, `SecurityOpt` contains `no-new-privileges`, `Config.User == "65532:65532"`, `HostConfig.Privileged == false`, `NetworkMode == <attempt-net>`, `PidsLimit`, `Memory`, `MemorySwap`, `NanoCpus`, `Mounts` only `/workspace` + `/out` + `/tmp` `tmpfs`, and `Config.Env` contains no unallowlisted keys).
2. `teb.ModelEgressAssessment` (`internal/teb/assessment.go`) is populated from actual host-firewall + proxy readiness probes (`DirectGeneralEgressBlocked`, `RequiredModelEndpointsViaProxy`, `Evidence`) and `Verified()` must return `true` before `Agentic` write mode is permitted.
3. **Test:** `TestEnforcedClaimRequiresInspectEvidence`.

---

## 3. Mandatory Security Closures and Named Tests

1. **Bundle export & strict `fsck` (never run Git/tools inside agent `.git` as Box user):**
   - **Closure:** Export to `/out/candidate.bundle` as UID `65532`, host `O_NOFOLLOW | O_NONBLOCK` + `fstat` copy, bundle magic + `git bundle verify`, `fetch` into fresh Box repo with `core.hooksPath=/dev/null`, `--max-input-size`, `fsck --strict` (`fsck.*=error`), and Box-built candidate commit.
   - **Named tests:** `TestResultIsCollectedViaBundleAndFsckNotAgentGit`, `TestResultIsReadFromGitNotFromAgentClaim`, `TestHostileGitConfigIsNotExecuted`, `TestBundlePathSymlinkFifoDirectoryRefused`, `TestBundleWithoutBasePrerequisiteRefused`, `TestPackBombRefused`, `TestCandidateWithSymlinkOrGitlinkRefused`, `TestMultiCommitHistoryCollapsedByBox`.
2. **Zero credentials inside sandbox; proxy injects & meters:**
   - **Closure:** Placeholders only inside container; per-Attempt reverse proxy injects real provider tokens and records usage outside the sandbox; verified by live `agentic-probe` per harness.
   - **Named tests:** `TestNoCredentialReachableInside`, `TestCredentialInjectedOutsideSandbox`, `TestProxyMeasuresProviderUsage`, proxy tests for header smuggling (`Rewrite` / `Connection:`), path traversal, redirect refusal, and SSE metering.
3. **Host firewall, registry containment, and mediated fetch:**
   - **Closure:** Per-Attempt `--internal` network + host `INPUT` firewall blocking all gateway ports except the proxy; cache-only registries for `private` projects and lockfile-scoped `GET`/`HEAD` reverse proxy for others; `/s42/fetch` `https:443` `GET`-only, allowlisted domain, no IP literals/userinfo, decoded size/rate/URL caps, total refusal for `data_class == "private"`, `agent.fetch.v1` evidence + untrusted marking.
   - **Named tests:** `TestSandboxHasNoRouteButProxy`, `TestOnlyAllowlistedHostsReachable`, `TestRegistryPathCannotReachArbitraryModuleHost`, `TestFetchRefusedForPrivateDataClass`, `TestFetchOutsideAllowlistRefused`, `TestMediatedFetchEnforcesMethodLengthAndRate`.
4. **Write-mode gate (`climodel`):**
   - **Closure:** `Agentic` refused in `DraftProfile` and `climodel.Adapter`; unlocked only in OCI executor after `inspect` + `ModelEgressAssessment.Verified()`.
   - **Named tests:** `TestAutoApproveRefusedOutsideSandbox`, `TestClimodelAdapterRefusesAgenticMode`.
5. **Inter-Attempt isolation through M9-T8 hygiene only:**
   - **Closure:** Container, `/tmp`, and session files destroyed on Attempt end; only verified checkpoint commits (`M1A-T4`) and Box evidence checked by `internal/texthygiene` (`Check` + `M9-T8` role-directive hygiene once merged) reach the next Attempt.
   - **Named tests:** `TestResumeStartsFromLastCheckpoint`, `TestCheckpointOfOldAttemptIsNotAResult`, `TestNoHarnessStateCrossesAttempts`.
6. **CI and pipeline files protected in every Project (including greenfield):**
   - **Closure:** One shared protected-pipeline list driving both Go (`repoworkspace.ProtectedPipelinePath`) and `scripts/check_protected_paths.py`, covering all of `.github/` (workflows, actions, dependabot), `azure-pipelines*.yml`/`.yaml`, `.gitlab-ci.yml`, `Jenkinsfile`, `.circleci/`, and manifest-declared CI/template paths (case-insensitive normalized match).
   - **Named tests:** `TestProtectedPipelineFilesRejectedInEveryProject` (`M1A-T3`), `TestProtectedPipelineFilesRejectedInGreenfieldProject` (`M1A-T5`).
7. **Freeze, containment, lease expiry, crash, and exhausted budget kill the sandbox:**
   - **Closure:** Active watcher (`<= 5s` bound), `docker kill` via `--cidfile`, proxy immediate refusal on `ErrLeaseInactive`, startup orphan reaper, no export on kill, quota-backed disk caps, and `inspect`-backed `EnforcementEnforced`.
   - **Named tests:** `TestFreezeKillsSandbox`, `TestContainmentOrBudgetExhaustionKillsSandbox`, `TestBoxCrashLeavesNoRunningSandbox`, `TestKillLatencyBounded`, `TestWorkspaceCannotExceedQuota`, `TestEnforcedClaimRequiresInspectEvidence`, plus `M1A-T2` lease tests (`TestHeartbeatKeepsLeaseForHours`, `TestMissedHeartbeatExpires`, `TestFreezeStopsRenewal`, `TestRenewalRefusedWhenEnvelopeSpent`, `TestExpiredAttemptCannotRecordResult`).
8. **Prepaid-only gate until money ledger (`M3-T2`):**
   - **Closure:** Agentic executor refuses any profile without `resources.CostPrepaidQuota` (or free/local) and enforces external + in-container wall-time deadlines.
   - **Named tests:** `TestPaidProfileRefusedWithoutMoneyLedger`, `TestAgentCanBuildAndRunTestsInside`, `TestImageAuditRefusesSetuidOrContainerCLI`.

---

## 4. Implementation Roadmap & Dependencies

This spec feeds the following downstream cards in [Section 4.1](../plans/2026-11-11-development-plan.md#41-execution-order):

1. **`M1A-T2` (`BUILD`):** Update `execution.Service.RenewLease` (`internal/execution/service.go`) to run `leaseGate.CheckLeaseAllowedTx` + envelope check in one transaction and handle inactive/expired leases cleanly, plus the 5 named lease tests.
2. **`M1A-T8` (`BUILD`, amended by §2.3):** Upgrade `internal/teb/egress_proxy.go` to per-Attempt HTTP reverse proxy (subscription/provider token swap & refresh, SSE/body usage metering with durable SQLite persistence, lockfile-scoped `GET`/`HEAD` registry proxy, hardened `/s42/fetch`, IP-literal refusal in `dialAllowedTarget`, per-Attempt network/firewall lifecycle + `ModelEgressAssessment`, deduped rogue signals).
3. **`M1A-T3` (`BUILD`, split per R8; depends on `M1A-T1 approved`, `M1A-T2`, `M1A-T8`, `M2-T3`, `M9-T6`):** Implement `OCIRequest`/`DockerProvider` extensions (`docker inspect`, `--cidfile`, `--tmpfs exec`, `--log-driver=none`, image audit, `agentic-probe`), `/out` bundle export + `fsck` + `CheckTreeHygiene` + Box-built Candidate, shared `ProtectedPipelinePath`, `climodel` `Agentic` gate (protected `profile.go`), active kill watcher + orphan reaper, `cmd/summa42-box` wiring, and OCI CI gate (protected `.github/workflows/ci.yml`).
4. **`M1A-T4` (`BUILD`), `M1A-T7` (`BUILD`), `M1A-T6` (`OWNER` benchmark), `M1A-T5` (`DESIGN` + `BUILD`):** Checkpoints/resume, new-dependency gate, parity benchmark, and large/greenfield git-push delivery.

---

## 5. Out of Scope

- Replacing one-shot triage, planning, or review stages with write-mode agents.
- Running candidate validation inside Docker (`M9-T1`, tracked separately; §2.7 defines the residual risk and controls until `M9-T1` lands).
- Monetary micro-USD pricing tables and paid per-token API accounting (`M3-T1`, `M3-T2`).
- Multi-host/remote Docker sandboxes or Kubernetes runners (deferred outside this single-Cube plan per Section 8).
