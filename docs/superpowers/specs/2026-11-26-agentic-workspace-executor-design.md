# M1A-T1 Design: Agentic Workspace Executor

**Card:** `M1A-T1` (`DESIGN`) — [development plan](../plans/2026-11-11-development-plan.md#m1a-agentic-execution)
**Date:** 2026-11-26 (revised after three independent adversarial review passes by Claude)
**Status:** Proposed (awaiting Owner approval; approval includes the explicit plan amendments to `M1A-T8`, `M2-T3`, `M1A-T4`, and `M1A-T3` in §4)
**Vision & Plan Alignment:** [Vision](../../VISION.md) (Articles 6, 9, 10, 11, 17, 18, 21), [Plan Section 2.5](../plans/2026-11-11-development-plan.md#25-design-goal-no-runaway-agents-no-private-channels), [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary)

---

## 1. Problem

Every model invocation in Summa42's GitHub pipeline (`internal/ghtriage/`) is currently a short, read-only or tool-free call:

1. `internal/ghtriage/climodel/profile.go` requires every `Profile` to declare `ReadOnly` or `NoTools: true` and rejects write/auto-approve flags (`dangerousArgs`).
2. `internal/ghtriage/implementation.go` (`Implementer.prepare`) sends a bounded JSON snapshot (at most 32 files / 256 KiB) and accepts back a single JSON object (`ImplementationOutput`) with at most 64 complete-file edits. The model never runs compilers, test suites, linters, or exploration commands on the repository.
3. Meanwhile, trusted execution boundary primitives exist in `internal/teb/` (`DockerProvider`, `OCIRequest`, `EnforcedProxyProfile`, `ConnectProxy`, `ModelEgressAssessment`), along with `execution.Service.RenewLease` (`internal/execution/service.go`), `resources.CostPrepaidQuota` (`internal/resources/service.go`), pinned checkouts (`internal/repoworkspace/`), and executor abstractions (`internal/executors/executor.go`), but they are incomplete and not composed into an end-to-end sandboxed agentic implementation path.

As stated in [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary), governance must constrain the blast radius rather than crippling the agent: inside an isolated sandbox an agentic harness should have full write/build/test tools for hours, while everything crossing the sandbox boundary remains governed by deterministic trusted code.

---

## 2. Decisions

### 2.1 Sandbox: Per-Attempt OCI Container with `--network=none` + Unix-Socket Proxy (`internal/teb`)

Each agentic implementation Attempt runs inside a dedicated, ephemeral OCI container managed by `teb.DockerProvider` (`internal/teb/docker_provider.go`, `internal/teb/oci.go`), started with `teb.EnforcedProxyProfile()` (`domain.EnforcementEnforced`).

#### Container lifecycle: `create` -> `inspect` -> `start`

Do **not** use `docker run --rm`. Instead, `DockerProvider` executes:
1. `docker create` (omitting `--rm` so `--cidfile` and post-exit `docker inspect` remain valid until the Box explicitly runs `docker rm -f`),
2. `docker inspect <cid>` **before `docker start`** (§2.8) so enforcement guarantees are verified before any untrusted code runs,
3. `docker start -a <cid>` draining stdout/stderr continuously into a fixed `256 KiB` ring buffer (never blocking container writes over hours-long runs), followed by explicit `docker rm -f <cid>`.

#### Container hardening & runtime flags

Extend `OCIRequest` and `ExecutionRequest` (wiring the currently unused `ExecutionRequest.Environment` field) so `teb.DockerArgs` emits:

- **Root filesystem & privileges:** `--read-only`, `--cap-drop=ALL`, `--security-opt=no-new-privileges`, `--user=65532:65532`, `--init` (reaps zombies), `--pull=never` (no implicit registry pulls at run time), explicit `--entrypoint` (from the audited image spec), and `--log-driver=none` (prevents unbounded Docker `json-file` host disk growth).
- **Resource & kernel limits:** `--pids-limit`, `--memory`, `--memory-swap=<memory>` (no swap bypass), `--cpus`, `--shm-size=64m`, `--ulimit fsize=<cap>` and `--ulimit nofile=<cap>`.
- **Network isolation (`--network=none`):** `--network=none` gives the container **loopback `127.0.0.1` only** and zero routes to the internet, host LAN, host loopback/wildcard daemons (`sshd`, `ollama`, Box control API), IPv6, or Docker's embedded DNS (`127.0.0.11` is absent on `--network=none`). Unlike an `--internal` bridge + host `nftables` rules, `--network=none` works identically under **rootless Docker, rootless Podman, and rootful Docker** without requiring root network namespaces or privileged firewall helpers.
- **Lifecycle tracking:** `--name=s42-attempt-<attemptID>`, `--cidfile=<box-run-dir>/<attemptID>-<nonce>.cid` (fresh unique path per invocation; fail if the path already exists), and `--label=summa42.attempt=<attemptID>`, `--label=summa42.role=agent`.
- **Mounts during the agentic execution step (Step 1):**
  - `/workspace`: bind-mounted `rw` from a quota-limited Box-managed workspace directory (`--mount=type=bind,src=<attempt-workspace>,dst=/workspace`, `--workdir=/workspace`), populated by a **full clone** at `BaseSHA` (not shallow, avoiding bundle boundary/prerequisite pitfalls) with `core.hooksPath=/dev/null`, repo-local `user.name`/`user.email` configured in `.git/config` (never `GIT_AUTHOR_*` env vars), and zero remotes or credentials.
  - `/run/s42-proxy`: bind-mounted **`readonly`** (`ro`; Unix domain socket `connect()` works on a `ro` mount) from a per-Attempt Box-owned directory holding `proxy.sock` (§2.3).
  - `/tmp` (including `HOME=/tmp/home`, `TMPDIR=/tmp`, `GOTMPDIR=/tmp/go-tmp`, `GOCACHE=/tmp/go-cache`, `GOMODCACHE=/tmp/go-mod`): `--tmpfs /tmp:rw,exec,nosuid,nodev,size=<cache-cap>,uid=65532,gid=65532`. Because Docker `--tmpfs` defaults to `noexec`, explicit `exec` is required so compiled Go test binaries run, and `tmpfs` size counts toward `--memory` (sizing guidance: for a Go repository, size `--memory` as `base_toolchain_ram + tmpfs_size`, e.g. `4 GiB` RAM with `1 GiB` `/tmp` tmpfs).
  - Optional pre-staged dependency cache mounts (e.g., Go module mirror): mounted **read-only** (`readonly`) and validated before mount (`trustedPath`: regular directory owned by Box user, not a symlink, never under `~/.summa42`).
  - `/out` is **not** mounted during Step 1; Step 1 writable paths are strictly `/workspace` and `/tmp` `tmpfs` (keeping `GuaranteeWorkspaceOnlyWrite` exact).
- **Environment & socket exclusion:**
  - `DockerArgs` structurally refuses any `Environment` key matching credential patterns (`*TOKEN*`, `*KEY*`, `*SECRET*`, `*PASSWORD*`, `*AUTH*`) except the explicitly allowlisted synthetic placeholder keys defined in §2.3, and refuses any workspace/mount path containing symlinks or container runtime sockets (`docker.sock`, `podman.sock`, `/var/run/docker*`), extending `validateWorkspace` so it does not skip symlinks or non-`docker.sock` socket names.

#### Daemon trust, rootless ownership, and CI honesty

Running `docker` as the Box user against a root-owned `/var/run/docker.sock` grants effective root equivalence on the host. Therefore:

1. **Prescribed deployment:** run **rootless Docker or rootless Podman** under a dedicated unprivileged OS user with user-namespace mapping and the default seccomp profile.
2. **Socket wiring:** rather than clearing the entire `docker` CLI environment (`cmd.Env = []string{}` in `internal/teb/oci.go:123`, which breaks rootless `DOCKER_HOST` / `XDG_RUNTIME_DIR`), `DockerBackend` passes **only** the explicitly configured trusted `DOCKER_HOST` / `XDG_RUNTIME_DIR` and `PATH`/`HOME` needed by the rootless client, never arbitrary host env or credentials.
3. **Deterministic ownership across rootless userns:**
   - **Socket (`/run/s42-proxy`):** the Box creates a private `0711` random 128-bit hex directory `<run-dir>/s42.<hex>/proxy`, listens on `proxy.sock` inside it, and `chmod 0666`s `proxy.sock`. Because only that single Attempt's container mounts `<run-dir>/s42.<hex>/proxy` as `/run/s42-proxy:ro` and no other local user/container can list `<run-dir>/s42.<hex>` (`0700` parent), UID `65532` (under any rootless sub-UID mapping) can `connect()` to `/run/s42-proxy/proxy.sock` cleanly.
   - **Workspace & `/out` export dir:** under rootless Podman use `--userns=keep-id:uid=65532,gid=65532` (mapping container UID `65532` directly to the Box user's host UID with standard `0700` ownership); under rootless Docker (where UID `65532` maps to an unprivileged sub-UID), the Box creates a `0711` random-hex parent dir and sets `0777` (or explicit ACL for the mapped sub-UID) on `<attempt-workspace>` and `<export-dir>`, mounts `/out` `rw` in Step 2 where UID `65532` writes `/out/candidate.bundle` with mode `0644` (`umask 022`), and cleans up via an unprivileged UID-`65532` `--network=none` container (`rm -rf`) followed by Box `os.RemoveAll`.
4. **Honest CI & classification claims (Rule R7):** `EnforcementEnforced` means the full OCI sandbox + `--network=none` + Unix-socket reverse proxy configuration was applied and verified via `docker inspect` (§2.8); it does not claim immunity to Linux kernel or container-runtime CVEs. Because GitHub's `ubuntu-latest` CI runs rootful Docker (`oci-acceptance`), rootless userns ownership is verified via a `realhost` test on the target host and `README.md` explicitly states both the residual kernel/daemon risk and whether rootless ownership is verified in CI vs on-host.

#### Host without Docker & explicit fallback policy

Summa42 **never** fakes `ENFORCED` containment when Docker/OCI is absent:

1. Project manifests (`M2-T3`, amended in §4) declare `agentic: required | preferred | off`.
2. If `teb.DockerProvider.Available(ctx)` is `false` (or OCI is unavailable):
   - When `agentic == required` (or a Task declares `RequiredEnforcement: domain.EnforcementEnforced`), the Attempt **fails closed** (`teb.ErrProviderUnavailable`) and never runs write/auto-approve CLIs on the host.
   - When `agentic == preferred`, `Implementer` explicitly downgrades to the existing **complete-file JSON edit stage** (`internal/ghtriage/implementation.go`, `UNENFORCED` model call + host validation) and records the downgrade (`mode: json_edits`, `enforcement: UNENFORCED`) as immutable case evidence so a non-sandboxed run is never mistaken for an agentic sandbox run.

---

### 2.2 Container Image & Adoption Audit

Images are **Owner-approved Project data**, never agent-selected:

1. **Image reference:** the Project manifest (`M2-T3`, §4) pins an immutable digest (`<registry>/<image>@sha256:<digest>`), installed/pulled beforehand by the Owner/operator.
2. **Image-adoption audit (mandatory before use):** before an image digest is accepted in a manifest or run, Box trusted code inspects (`docker image inspect`) and runs a read-only audit scan inside the image (`--network=none --user=65532:65532`) verifying:
   - no `VOLUME` declarations in the image config (prevents anonymous unquotaed host writes under `/var/lib/docker`),
   - expected `ENTRYPOINT` (a small Box-verified entrypoint that creates `/tmp/home` and starts the audited local `127.0.0.1` -> `/run/s42-proxy/proxy.sock` TCP-to-UDS bridge before `exec`-ing the harness),
   - no setuid or setgid binaries (`find / -perm /6000`),
   - no `docker`, `podman`, `nerdctl`, `ssh`, or `sudo` binaries,
   - no pre-baked CLI auth/login files (`~/.claude`, `~/.codex`, `~/.gemini`, `~/.copilot`, `~/.commandcode`).
   - **Test:** `TestImageAuditRefusesSetuidOrContainerCLI`.
3. **Contents:** minimal Linux userland, `git`, the Project's pinned toolchain (e.g. Go 1.27, Python, Node), the tiny audited TCP-to-UDS bridge (`socat` or small Go binary in `/usr/local/bin`), and the headless harness binary installed read-only (`0755`) for UID `65532`.

---

### 2.3 Unix-Socket Egress Proxy, Harness Auth, and Mediated Fetch (`M1A-T8`)

#### Amending `M1A-T8`: `--network=none` + per-Attempt Unix-socket reverse proxy (not `CONNECT`)

The `M1A-T8` card originally mentioned reusing the "existing CONNECT proxy" to inject credentials and enforce read-only access. Two obstacles prevent that design:
1. An HTTP `CONNECT` tunnel is end-to-end opaque TLS: without TLS MITM interception, a `CONNECT` proxy **cannot** inject `Authorization` headers, enforce `GET` vs `PUT`, or parse provider token usage.
2. On rootless Docker/Podman, a host process cannot bind the container bridge gateway IP or install host `nftables` rules for RootlessKit's network namespace.

Therefore, **this spec explicitly amends `M1A-T8`**:
- Keep `teb.DialAllowedTarget` / `publicEgressIP` DNS-rebinding protections from `internal/teb/egress_proxy.go` (and fix `dialAllowedTarget` at `egress_proxy.go:64` so IP literals never bypass `publicEgressIP`).
- Containers run with `--network=none` (`teb/oci.go` updated so `GuaranteeModelEgressMediated` pairs with `--network=none` plus the mounted `/run/s42-proxy:ro` socket directory).
- Inside the container, the audited entrypoint launches a local `127.0.0.1:<port>` -> `/run/s42-proxy/proxy.sock` bridge.
- On the host, each Attempt starts its **own** dedicated `http.Server` listening on `<socket-dir>/proxy.sock` (`net.Listen("unix", ...)`), so credentials, allowlists, lease checks, and usage counters bind 1:1 to a single `attemptID`.

All proxy paths enforce:
- **`httputil.ReverseProxy.Rewrite` (never `Director`):** prevents `Connection: Authorization` header smuggling from stripping Box-injected auth headers.
- **Header allowlist:** only explicitly allowlisted request headers pass (`Content-Type`, `Accept`, `User-Agent`, `Anthropic-Version`, `Anthropic-Beta`, `OpenAI-Beta`); `Authorization`, `x-api-key`, `Proxy-Authorization`, `Cookie`, `Host`, org/project override headers, `Upgrade` (no WebSockets), and `Connection`-named headers are stripped.
- **Route, method & path hygiene:** reject absolute-form request targets, `..`, `%2e`, `//`, and encoded slashes (`%2f`); match provider routes on `r.URL.Path` (preserving allowlisted query parameters such as `?beta=true` after segment validation) strictly by whole path segment; set `CheckRedirect` to `http.ErrUseLastResponse`; cap request body size and dial/header/idle timeouts; redact upstream `401`/`403` response bodies so upstream errors cannot echo key fragments.
- **Active-lease & hard-cap check on every request:** before forwarding any request, the proxy checks in-process budget/in-flight limits and verifies via `ProxyUsageSink.IsLeaseActive` (§2.6) that the Attempt lease is still active (immediately refusing upon `ErrLeaseInactive` or freeze rather than waiting for the next watcher poll).

#### First-slice harness, subscription OAuth (`PREPAID_QUOTA`), and telemetry suppression

Until `M3-T2` (money ledger) ships, agentic Attempts allow **only** profiles classified as `PREPAID_QUOTA` (§2.4). Note that pay-per-token `OPENAI_API_KEY` runs are **not** `PREPAID_QUOTA`, and `agy` is excluded from write-mode execution (Owner decision of 11 Nov keeps `agy` for read-only/JSON implementation; Owner confirmation is requested on `claude` as the first-slice agentic harness):

1. **Primary first-slice harness for `M1A-T3`: `claude` (Claude Code, Anthropic subscription OAuth):**
   - *In-container env:* `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>/provider/anthropic`, `ANTHROPIC_AUTH_TOKEN=s42-placeholder`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_TELEMETRY=1`, `DISABLE_AUTOUPDATER=1`, `DISABLE_ERROR_REPORTING=1`. No `~/.claude/.credentials.json` inside container.
   - *Proxy behavior:* matches `POST /provider/anthropic/v1/messages` and `POST /provider/anthropic/v1/messages/count_tokens` (checking `r.URL.Path` while passing query parameters like `?beta=true`), strips `s42-placeholder`, injects the Box-held OAuth bearer token **plus** `anthropic-beta: oauth-2025-04-20` (merged with any allowlisted `Anthropic-Beta` header), refreshes expired tokens via `console.anthropic.com/v1/oauth/token` **outside** the container (using a dedicated Summa42 OAuth credential file so host interactive CLI sessions do not race on rotating refresh tokens), forces `Accept-Encoding: identity` (or decodes gzip before metering), sets `FlushInterval: -1` for SSE streaming, and settles usage via `ProxyUsageSink` (§2.6).
2. **Unsupported in `M1A-T3` until proven `PREPAID_QUOTA`-compatible by `agentic-probe`:**
   - `codex` (uses metered `OPENAI_API_KEY` unless a ChatGPT subscription OAuth endpoint flow passes `agentic-probe`), `copilot` (requires GitHub OAuth `gho_` -> Copilot token exchange endpoints), `agy`, `commandcode`, `ollama`, and any harness that ignores `*_BASE_URL`, insists on local login files, or emits un-suppressible telemetry.

#### Package registry path

- **Private projects (`data_class == "private"`):** **cache-only** (zero live registry proxy access; dependencies come solely from Box pre-staged read-only cache mounts or vendored trees), preventing exfiltration via module lookup URLs (e.g., `proxy.golang.org/<attacker-host>/<secret>/@v/list`).
- **Non-private projects:** plain-HTTP proxy endpoint on the same Unix-socket server (`GOPROXY=http://127.0.0.1:<port>/registry/go`, `npm_config_registry=http://127.0.0.1:<port>/registry/npm`, etc.) permitting **`GET` and `HEAD` only**, restricted strictly to module/package names enumerated in the Project manifest `registry_allowlist` or repository lockfile (`go.mod`, `package-lock.json`, etc.), dialing upstream **HTTPS port 443 only** after `publicEgressIP` DNS validation, with per-Attempt request count and byte-transfer caps.
- **Test:** `TestRegistryPathCannotReachArbitraryModuleHost`.

#### Mediated fetch tool (`/s42/fetch`)

Exposed inside the sandbox via `http://127.0.0.1:<port>/s42/fetch?url=...` (and a thin helper CLI/MCP shim), acting as hard containment:

- **URL validation (`net/url`):** must be `https` on port `443` (explicit or default); reject userinfo (`allowed.com@evil.com`), IP literals (both in the URL and in `fetch_allowlist`; also enforce `publicEgressIP` on literals in `dialAllowedTarget`), trailing-dot hosts (`example.com.`), non-ASCII / IDN hosts (or canonicalize and match exact ASCII punycode), and disallow automatic redirects (or re-validate every hop against `fetch_allowlist` and `publicEgressIP`).
- **Domain allowlist:** exact lowercase host match against the Project manifest's `fetch_allowlist`.
- **Method, length, rate, size:** `GET` only, URL `<= 2048` bytes, per-Attempt cap (e.g. `50` requests), rate cap (e.g. `10/min`), decoded response body cap (e.g. `1 MiB` via `io.LimitReader` after decompression to stop gzip bombs).
- **Private data class refusal:** if Project or source `data_class == "private"`, **all** `/s42/fetch` calls fail closed (`403 Forbidden`) and trigger a rogue signal.
- **Evidence & untrusted provenance:** every allowed fetch records an immutable Box evidence object (`agent.fetch.v1` containing URL, status, SHA-256, byte count, and a bounded raw copy capped at `256 KiB` flagged untrusted for `M1A-T7` license parsing), and the HTTP response carries `X-Summa42-Untrusted: true`.
- **Signal deduplication:** per-refusal rogue signals (`egress_denied`) are deduped/rate-limited per `(attemptID, host, reason)` via `ProxyUsageSink.RecordEgressDenied` so noisy probing triggers containment cleanly without flooding the audit log.

---

### 2.4 Write-Mode Harness Gate & Prepaid Source of Truth (`internal/ghtriage/climodel/profile.go`)

`internal/ghtriage/climodel/profile.go` keeps `dangerousArgs` rejection and `ReadOnly`/`NoTools` mandatory for all existing profiles and native callers:

1. **Structuring `AgenticSpec` and `CostControl`:**
   - Add optional fields to `climodel.Profile` (using plain validated strings in `climodel` so `internal/ghtriage/climodel` does **not** import `internal/resources` or pull in `internal/state/sqlite`; callers in `cmd/summa42-box` / `internal/ghtriage` convert `p.CostControl` to `resources.CostControl` with `RequireHardCap: true` when calling `resources.Service.ReserveInTx`):
     - `CostControl string` (`json:"cost_control,omitempty"`, accepted values `""` or `"PREPAID_QUOTA"` matching `string(resources.CostPrepaidQuota)`).
     - `Agentic *AgenticSpec` (`json:"agentic,omitempty"`, holding `WriteArgs []string` and `Harness string`).
   - **Trusted-origin rule:** `DraftProfile` **unconditionally refuses** any profile that sets `Agentic != nil` or `CostControl != ""`. Both fields are accepted **only** in `BuiltinProfiles()` or Owner-installed profiles loaded via `ReadProfileFile` (`trustedPath` in `$SUMMA42_HOME/model-profiles/`).
   - **Replacement semantics & `dangerousArgs` exemption:** `WriteArgs` **replaces** `Args` when running in agentic OCI mode (never appends, avoiding conflicts with `--permission-mode plan` / `--deny-tool write`). `WriteArgs` is **explicitly exempt** from `dangerous()` substring checks (because headless write mode requires `--dangerously-skip-permissions` for `claude` so Bash test runs are not blocked; `--permission-mode acceptEdits` blocks Bash). Instead, `Profile.Validate()` enforces:
     1. `Args`, `ReadOnly`, and `Input.Flag` continue to pass `dangerous()` as today.
     2. `WriteArgs` validates `{model}` placeholder rules identically to `Args` (`ModelRequired == (models == 1)`).
     3. `WriteArgs` does not contain `ReadOnly`.
     4. `WriteArgs` matches a **strict per-harness allowlist** in `profile.go`, pinned by unit test and verified by live `agentic-probe` before enabling.
2. **Runtime enforcement gate (safe inside OCI and nowhere else):**
   - `climodel.Adapter` (host OS-user runner) **always refuses** any profile/call requesting `Agentic` mode (`TestAutoApproveRefusedOutsideSandbox`, `TestClimodelAdapterRefusesAgenticMode`).
   - Only the OCI Agentic Executor (`M1A-T3`), after `teb.Manager` / `teb.DockerProvider.Assess` + `docker inspect` (§2.8) + `ModelEgressAssessment.Verified()` prove `domain.EnforcementEnforced` and `profile.CostControl == string(resources.CostPrepaidQuota)` (until `M3-T2` ships), may invoke `WriteArgs` inside the container.
   - Because `profile.go` is a protected path (Rule R5), its change in `M1A-T3` requires line-by-line Owner review.

---

### 2.5 Result Extraction: Offline `/out` Export, `fsck`, and Box-Built Candidate

Because the agent had write access to `/workspace` (including `/workspace/.git`), **trusted Box code never executes `git`, hooks, or tools inside `/workspace`**.

1. **Candidate vs. checkpoints vs. uncommitted work:**
   - Intermediate commits created by the agent serve solely as checkpoint evidence (`M1A-T4`).
   - **Periodic offline checkpoint capture (`M1A-T4`):** on a periodic interval during long Attempts (e.g., on `RenewLease` ticks when `HEAD` has advanced since the last verified checkpoint), the Box runs the offline Step 2 export container (below) against `/workspace` (`ro`) into a separate checkpoint `<export-dir>` and runs full tree inspection + commit-message hygiene (`texthygiene.Check` and `M9-T8` hygiene once merged). If a checkpoint fails tree inspection or hygiene, it is discarded (logged/signaled) without overwriting the last valid checkpoint. When a fault/kill stops Step 1, `M1A-T4` resumes solely from the **last already-verified checkpoint**, never by exporting a killed/tainted container.
   - Uncommitted working-tree changes left when the agent exits are **dropped** (never committed by the host or an extra helper). Note that `repoworkspace` checks out `BaseSHA` as a detached `HEAD`, so an agent advancing `HEAD` leaves an advanced detached `HEAD` (which is accepted!). Bundle export is skipped and the Attempt records **no candidate** only if `HEAD` is unborn, `BaseSHA` is not an ancestor of `HEAD`, or `HEAD^{tree} == BaseSHA^{tree}`.
2. **Offline bundle export container (Step 2 — periodic checkpoint or normal/wall-time exit only):**
   - Never run after break-glass freeze, containment, or lease loss (§2.6).
   - Starts a separate short-lived `--network=none --read-only --user=65532:65532` container with distinct lifecycle identity (`--name=s42-export-<attemptID>-<seq>`, `--cidfile=<box-run-dir>/export-<attemptID>-<seq>.cid`, `--label=summa42.attempt=<attemptID>`, `--label=summa42.role=export`), mounting `--tmpfs /tmp:rw,nosuid,nodev,size=64m`, mounting `/workspace` **`readonly`**, and mounting a fresh Box-created export directory at `/out` **`rw`** (§2.1 ownership). Covered by the same freeze watcher so break glass kills an active export container immediately.
   - Runs (with explicit `umask 022` so `/out/candidate.bundle` is `0644`):
     `git -C /workspace -c core.hooksPath=/dev/null -c safe.directory=/workspace bundle create /out/candidate.bundle <BaseSHA>..HEAD` with `GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_NO_REPLACE_OBJECTS=1`.
3. **Host-side safe consumption of `<export-dir>/candidate.bundle`:**
   - Open `<export-dir>/candidate.bundle` with `O_NOFOLLOW | O_NONBLOCK`, `fstat` to require a regular file (`!ModeSymlink`, `!ModeNamedPipe`, `!ModeDir`) with `st_size <= maxBundleBytes` (e.g. `64 MiB`), and copy through that FD (`io.LimitReader`) into a Box-owned temp file inside a size-limited Box scratch directory (quota/`tmpfs`) outside `/workspace` and `<export-dir>`.
   - Verify the `# v2 git bundle\n` or `# v3 git bundle\n` magic header, then run `git bundle verify` and `fetch --no-tags --no-recurse-submodules` inside a clean `git init` Box repo seeded with `BaseSHA`, under a strict per-command wall-time cap with **process-group kill** (`SysProcAttr{Setpgid: true}`, killing `-pgid` on timeout/cancel so child `index-pack` processes cannot survive) and with:
     `GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_NO_REPLACE_OBJECTS=1`
     plus `-c core.hooksPath=/dev/null -c transfer.fsckObjects=true -c receive.fsckObjects=true -c core.bigFileThreshold=1m`.
   - **Pack-bomb protection:** the real bounds on inflated pack bombs are the size-limited Box scratch (`tmpfs`/quota) plus the per-command wall-time cap with `-pgid` kill (and optional pre-fetch `git index-pack --max-input-size` on the extracted pack); immediately after fetch, `git rev-list --count --objects <BaseSHA>..<tip>` verifies bounded object count before tree diff or blob reads, and `ReadFile(..., maxBytes)` caps every blob read.
   - Run `git fsck --strict --full` with explicit `fsck.*=error` severities (`fsck.zeroPaddedFilemode=error`, `fsck.gitmodulesSymlink=error`, `fsck.badName=error`, `fsck.duplicateEntries=error`).
4. **Tree inspection & Box-built single-parent Candidate:**
   - Reject merges or non-linear history (`BaseSHA..tip` must be linear with no merge commits; `BaseSHA` must be an ancestor of `tip`).
   - Walk `git diff-tree -r --no-renames -z <BaseSHA> <tip>` and reject:
     - any non-regular-blob mode (`120000` symlink, `160000` gitlink/submodule),
     - `.gitmodules`, new/modified `.gitattributes`, case-colliding paths, `.git` lookalikes (`git~1`, `.GIT`, HFS ignorable characters),
     - unauthorized executable-bit (`100755`) changes unless `allow_executable_edits` is enabled in the Project manifest (§4),
     - binary or non-UTF-8 file contents,
     - paths outside `allowed_paths`/`allowed_prefixes` (or **protected pipeline paths**, §3.6),
     - path or content hygiene violations (`texthygiene.Check("path of an edit", path)` plus a new exported `repoworkspace.CheckTreeHygiene(ctx, repoDir, baseSHA, tipSHA)` covering the same base-diff hidden-rune logic as unexported `checkEditHygiene`).
   - **Box builds the Candidate commit:** from the verified `tip^{tree}`, the Box creates **one** unsigned single-parent commit on `BaseSHA` in the Box-owned repo (`Summa42` committer/author identity and Box-written commit message, stripping hostile agent author/message text), producing a `repoworkspace.Candidate` compatible with `ExportCandidate` / `VerifyCandidate`.
   - Until `M1A-T5` lifts `Export.Validate` (`64` files / `256 KiB`) and `repoworkspace/review.go:27` (`64` changed paths), candidates exceeding those limits are **held** rather than published or reviewed.

---

### 2.6 Attempt Limits, Lease Renewal (`M1A-T2`), Proxy Usage Storage, and Kill Watcher

Agentic Attempts run for hours while bounded across four axes:

| Limit | Mechanism | Enforcement Point |
| --- | --- | --- |
| **Wall time & liveness** | Short initial lease (e.g. `10m`) renewed via `execution.Service.RenewLease` (`M1A-T2`), plus external hard `max_attempt_wall_time` (`context.WithDeadline`) and an in-container dead-man wall timer (`timeout -s KILL <wall>`) | Outer Box context + `RenewLease` + in-container `timeout` |
| **Request-unit envelope & proxy usage** | Unit is **integer request units** (`1` unit per forwarded model proxy request, matching `00005_resources.sql`). Export `resources.Service.AvailableInTx(ctx, tx, envelopeID)` so `execution.Service.RenewLease` (via an `EnvelopeChecker` interface) calls `leaseGate.CheckLeaseAllowedTx` and checks `available >= 1` **inside the same SQLite transaction** as updating `lease_expires_at`. Before forwarding each proxy request, the proxy reserves `1` unit via `ReserveInTx` (with `RequireHardCap: true` from `profile.CostControl`); for non-streamed responses it records tokens on completion, and for SSE streams it parses `message_stop` / `response.completed` events as they stream, settling tokens on completion (or settling as `UNKNOWN` with the `1`-unit reservation retained on early disconnect/parse failure). Proxy telemetry persists to `proxy_usage` (`00040+`) via `ProxyUsageSink` (keeping `internal/teb` free of SQLite imports). **Until `M3-T2` ships, only `CostPrepaidQuota` profiles are allowed** | Pre-start eligibility + every `RenewLease` tx + every proxy request |
| **Compute** | OCI `--pids-limit`, `--memory`, `--memory-swap=<memory>`, `--cpus`, `--ulimit` | Kernel cgroups inside OCI runtime |
| **Disk** | Quota-limited workspace filesystem (XFS/ext4 project quota or pre-sized loop mount / tmpfs), bounded `/tmp` `tmpfs`, `--log-driver=none`, `--shm-size`, and no `VOLUME` in image | Filesystem quota + container runtime |

#### Named `teb.ProxyUsageSink` interface

```go
type ProxyUsageSink interface {
	RecordModelCall(ctx context.Context, attemptID domain.ID, provider string, inTokens, outTokens int64, status string) error
	RecordEgressDenied(ctx context.Context, attemptID domain.ID, host, reason string) error
	IsLeaseActive(ctx context.Context, attemptID domain.ID) (bool, error)
}
```

#### Active Kill Watcher (`<= 5s` latency bound) & Crash Recovery

- A watcher goroutine checks every `<= 5s` for: break-glass `FROZEN` / safe mode, Attempt containment (`containment`), `RenewLease` failure (including `domain.ErrLeaseInactive` / `ErrStaleAttempt` when an expired lease is no longer active), or envelope exhaustion.
- On **any** trigger (or `ctx.Done()`), `DockerBackend.Run` and the watcher read the `--cidfile` and immediately run `docker kill <cid>` + `docker rm -f <cid>` (`context.WithoutCancel(ctx)` with bounded timeout). Stopping lease renewal alone is never enough.
- **No export on kill:** if killed by freeze, containment, or lease loss, Step 2 bundle export is **not** run.
- **Box crash recovery & startup orphan reaper:** if the Box crashes, the container stops either when its in-container `timeout -s KILL` fires (`TestBoxCrashLeavesNoRunningSandbox` tests the reaper on restart and bounded wall-timer teardown) or immediately when the restarted Box's startup reaper scans containers with label `summa42.attempt` whose Attempt is no longer `ACTIVE` and kills/removes them.

---

### 2.7 Existing One-Shot Stages & Residual Validation Risk

1. **One-shot stages stay:** triage (`stage2`, `reviewer`), grounded planning (`planner`, `planreview`), and independent code review (`codereview` as `summa42-review`) remain short `read_only` / `no_tools` calls. Complete-file JSON implementation (`internal/ghtriage/implementation.go`) stays as the explicit `agentic == preferred` fallback on Docker-less hosts.
2. **Candidate validation exposure (residual risk until `M9-T1`):** until `M9-T1` moves `summa42-validate` into an OCI container, `summa42-validate` executes agent-written tests on the host (as an isolated OS user without credentials, but with host network until `M9-T7`). Until `M9-T1` and `M9-T7` land:
   - `M1A-T3` validation runs without host credentials and documents this in `README.md` per Rule R7 ("agentic candidates run on host validation until `M9-T1`"),
   - pre-publication checks must verify Project CI does not expose deployment secrets to untrusted branch PR builds.

---

### 2.8 Non-Circular Enforcement Verification

To ensure `EnforcementEnforced` is never self-reported from requested flags (`teb/profile.go` lines 3–5):

1. After `docker create` and **before `docker start`**, `DockerProvider` runs `docker inspect <cid>` and checks:
   - **Positive matches:** `HostConfig.ReadonlyRootfs == true`, `CapDrop == ["ALL"]`, `SecurityOpt` contains `no-new-privileges`, `Config.User == "65532:65532"`, `HostConfig.Privileged == false`, `NetworkMode == "none"`, `PidsLimit`, `Memory`, `MemorySwap == Memory`, `NanoCpus`, `HostConfig.Tmpfs` contains `/tmp` with `rw,exec,nosuid,nodev`, `Mounts` contains strictly `/workspace` and `/run/s42-proxy:ro` (plus any explicit `ro` cache mounts), and `Config.Env` equals the audited image baseline plus allowlisted non-secret Attempt vars.
   - **Negative checks:** `HostConfig.CapAdd` empty, `HostConfig.Devices` empty, `HostConfig.PidMode == ""`, `HostConfig.IpcMode != "host"`, `SecurityOpt` contains neither `seccomp=unconfined` nor `apparmor=unconfined`, `HostConfig.ExtraHosts` empty, and `HostConfig.Dns` empty.
2. `teb.ModelEgressAssessment` (`internal/teb/assessment.go`) is populated from actual `--network=none` + Unix-socket proxy readiness probes (`DirectGeneralEgressBlocked`, `RequiredModelEndpointsViaProxy`, `Evidence`), and `Verified()` must return `true` before `Agentic` mode is permitted.
3. **Test:** `TestEnforcedClaimRequiresInspectEvidence`.

---

## 3. Mandatory Security Closures and Named Tests

1. **Bundle export & strict `fsck` (never run Git/tools inside agent `.git` as Box user):**
   - **Closure:** Step 2 export to `/out/candidate.bundle` as UID `65532` (`/workspace` `ro`), host `O_NOFOLLOW | O_NONBLOCK` + `fstat` copy, bundle magic + `git bundle verify`, `fetch` into fresh Box repo with `core.hooksPath=/dev/null`, size-limited scratch + wall-time cap with `-pgid` kill + post-fetch `rev-list` object-count cap, `fsck --strict` (`fsck.*=error`), and Box-built single-parent candidate commit.
   - **Named tests:** `TestResultIsCollectedViaBundleAndFsckNotAgentGit`, `TestResultIsReadFromGitNotFromAgentClaim`, `TestHostileGitConfigIsNotExecuted`, `TestBundlePathSymlinkFifoDirectoryRefused`, `TestBundleWithoutBasePrerequisiteRefused`, `TestPackBombRefused` (high-compression-ratio pack bomb), `TestCandidateWithSymlinkOrGitlinkRefused`, `TestMultiCommitHistoryCollapsedByBox`.
2. **Zero credentials inside sandbox; Unix-socket proxy injects & meters:**
   - **Closure:** Placeholders only inside container; per-Attempt Unix-socket reverse proxy injects real provider tokens + `anthropic-beta` OAuth header and settles/persists usage outside the sandbox; verified by live `agentic-probe`.
   - **Named tests:** `TestNoCredentialReachableInside`, `TestCredentialInjectedOutsideSandbox`, `TestProxyMeasuresProviderUsage`, proxy tests for `Rewrite` / `Connection:` header smuggling, path traversal, redirect refusal, and SSE metering.
3. **Total network isolation (`--network=none`), registry containment, and mediated fetch:**
   - **Closure:** `--network=none` + `/run/s42-proxy:ro` Unix-socket bridge; cache-only registries for `private` projects and lockfile-scoped `GET`/`HEAD` reverse proxy for others; `/s42/fetch` `https:443` `GET`-only, allowlisted domain, no IP literals/userinfo, decoded size/rate/URL caps, total refusal for `data_class == "private"`, `agent.fetch.v1` evidence + untrusted marking.
   - **Named tests:** `TestSandboxHasNoRouteButProxy`, `TestOnlyAllowlistedHostsReachable`, `TestRegistryPathCannotReachArbitraryModuleHost`, `TestFetchRefusedForPrivateDataClass`, `TestFetchOutsideAllowlistRefused`, `TestMediatedFetchEnforcesMethodLengthAndRate`.
4. **Write-mode gate & prepaid source of truth (`climodel`):**
   - **Closure:** `Agentic` and `CostControl` refused in `DraftProfile`; `WriteArgs` replaces `Args`, validates `{model}`, bypasses `dangerous()` via strict per-harness allowlist pinned by test; `climodel.Adapter` refuses `Agentic`; OCI executor unlocks `Agentic` only after `docker create`+`inspect` + `ModelEgressAssessment.Verified()` + `CostControl == "PREPAID_QUOTA"`.
   - **Named tests:** `TestAutoApproveRefusedOutsideSandbox`, `TestClimodelAdapterRefusesAgenticMode`, `TestAgentCanBuildAndRunTestsInside`.
5. **Inter-Attempt isolation & periodic checkpoints through M9-T8 hygiene only:**
   - **Closure:** Container, `/tmp`, and session files destroyed on Attempt end; periodic offline Step 2 checkpoints (`M1A-T4`) validated via tree inspection + `internal/texthygiene` (`Check` + `M9-T8` hygiene once merged) are the sole state crossing Attempts; no post-kill export on freeze/containment/lease loss.
   - **Named tests:** `TestResumeStartsFromLastCheckpoint`, `TestCheckpointOfOldAttemptIsNotAResult`, `TestNoHarnessStateCrossesAttempts`.
6. **CI and pipeline files protected in every Project (including greenfield):**
   - **Closure:** One shared protected-pipeline JSON file `internal/repoworkspace/protected_pipeline_paths.json` (embedded via `//go:embed` in Go, read from `base/` by `scripts/check_protected_paths.py`, and itself listed in `PROTECTED_FILES` + `.github/CODEOWNERS`) covering all of `.github/` (workflows, actions, dependabot), `azure-pipelines*.yml`/`.yaml`, `.gitlab-ci.yml`, `Jenkinsfile`, `.circleci/`, and manifest-declared `pipeline_paths` (case-insensitive normalized match).
   - **Named tests:** `TestProtectedPipelineFilesRejectedInEveryProject` (`M1A-T3`), `TestProtectedPipelineFilesRejectedInGreenfieldProject` (`M1A-T5`).
7. **Freeze, containment, lease expiry, crash, and exhausted budget kill the sandbox:**
   - **Closure:** Active watcher (`<= 5s` bound), `docker kill` via unique `--cidfile`, proxy immediate refusal on `ErrLeaseInactive`, startup orphan reaper + in-container `timeout -s KILL`, no export on kill, quota-backed disk caps, and pre-start `inspect`-backed `EnforcementEnforced`.
   - **Named tests:** `TestFreezeKillsSandbox`, `TestContainmentOrBudgetExhaustionKillsSandbox`, `TestBoxCrashLeavesNoRunningSandbox`, `TestKillLatencyBounded`, `TestWorkspaceCannotExceedQuota`, `TestEnforcedClaimRequiresInspectEvidence`, plus `M1A-T2` lease tests (`TestHeartbeatKeepsLeaseForHours`, `TestMissedHeartbeatExpires`, `TestFreezeStopsRenewal`, `TestRenewalRefusedWhenEnvelopeSpent`, `TestExpiredAttemptCannotRecordResult`).
8. **Prepaid-only gate until money ledger (`M3-T2`):**
   - **Closure:** Agentic executor refuses any profile without `profile.CostControl == "PREPAID_QUOTA"` and enforces external + in-container wall-time deadlines.
   - **Named tests:** `TestPaidProfileRefusedWithoutMoneyLedger`, `TestImageAuditRefusesSetuidOrContainerCLI`.

---

## 4. Plan Amendments, Protected Paths, and PR Split (Rules R3, R5, R8)

### Explicit Plan Amendments (require Owner approval as part of `M1A-T1`)

Because `docs/superpowers/plans/2026-11-11-development-plan.md` is R5-protected, Owner approval of `M1A-T1` authorizes updating the plan with these exact amendments:
1. **`M1A-T8` amendment:** replace "existing CONNECT proxy" with per-Attempt `--network=none` + Unix-socket HTTP reverse proxy (§2.3) so credential injection, `GET`/`HEAD` registry restriction, and token metering work without TLS MITM or rootless firewall incompatibility.
2. **`M2-T3` manifest schema additions:** add `agentic` (`required | preferred | off`), `sandbox_image` (`@sha256:`), `sandbox_limits` (`pids`, `memory_bytes`, `cpus`, `tmpfs_bytes`, `workspace_quota_bytes`, `max_attempt_wall_time`), `registry_allowlist`, `fetch_allowlist`, `pipeline_paths`, and `allow_executable_edits` to `project.json`.
3. **`M1A-T4` amendment:** replace live extraction of every commit from a killed container's `.git` with periodic offline Step 2 checkpoint capture (§2.5) so post-kill resume never touches tainted workspace `.git` state.
4. **`M1A-T3` vs `M1A-T5` size-cap clarification:** candidates exceeding `Export.Validate` (`64` files / `256 KiB`) or `repoworkspace/review.go:27` (`64` paths) are **held** rather than published/reviewed until `M1A-T5` replaces per-file publication with Git push.
5. **`M9-T7` and `M3-T6` status:** explicitly unaffected by `M1A-T1` — `M9-T7` (host egress firewall) and `M3-T6` (direct API providers) govern native OS-user (`summa42-model` / `summa42-review`) calls, whereas `M1A` governs OCI-sandboxed agentic runs.

### Protected Files Touched by Downstream BUILD PRs (Rule R5)

No new `processStartAllowlist` entries (`tests/acceptance/architecture_test.go`) or `scripts/setup-model-user.sh` edits are needed (`teb` and `repoworkspace` are already allowlisted, and `--network=none` avoids host firewall helpers). Downstream protected files requiring Owner review are:
- `docs/superpowers/plans/2026-11-11-development-plan.md` (plan amendments above),
- `scripts/check_protected_paths.py`, `internal/repoworkspace/protected_pipeline_paths.json`, and `.github/CODEOWNERS` (shared protected-pipeline list + `test_check_protected_paths.py` parity),
- `internal/ghtriage/climodel/profile.go` (`AgenticSpec`, `CostControl`, `DraftProfile` refusal),
- `.github/workflows/ci.yml` and `Makefile` (if OCI agentic acceptance target is added to `Makefile`).

### Proposed PR Split per Rules R3 & R8 (ordered, ~200–400 non-test LOC each, every PR wired to `cmd/`)

1. **`M1A-T2`:** `resources.Service.AvailableInTx`, `execution.Service.RenewLease` envelope + `leaseGate` single-tx check, and the 5 lease tests (called via existing `execution.Service` in `cmd/`).
2. **`M1A-T8-a`:** Per-Attempt Unix-socket reverse proxy (`Rewrite`, header allowlist, `anthropic-beta` OAuth injection, SSE metering, active-lease check) + `ProxyUsageSink` interface + `proxy_usage` migration (`00040+`), wired into a new `cmd/summa42-box agentic-proxy-check` / `agentic-probe` command so the package and migration have a real `cmd/` caller (R3).
3. **`M1A-T8-b`** *(depends on `M1A-T8-a`)*: Hardened `/s42/fetch`, lockfile-scoped `GET`/`HEAD` registry proxy, and `dialAllowedTarget` IP-literal `publicEgressIP` check, wired into the `cmd/summa42-box` proxy builder.
4. **`M1A-T3-a`** *(depends on `M1A-T8-a`, `M1A-T8-b`)*: `OCIRequest` / `DockerProvider` hardening (`docker create` -> `inspect` -> `start`, `--network=none`, `--cidfile`, `--tmpfs exec`, `--log-driver=none`, image audit, `ModelEgressAssessment` wiring, `/run/s42-proxy:ro` mount), wired into `cmd/summa42-box agentic-probe` (driving `TestSandboxHasNoRouteButProxy` and `TestEnforcedClaimRequiresInspectEvidence`).
5. **`M1A-T3-b`** *(depends on `M1A-T3-a`)*: Offline `/out` bundle export container, `O_NOFOLLOW | O_NONBLOCK` + `fsck` + `CheckTreeHygiene` + Box-built `Candidate` + shared `protected_pipeline_paths.json` (`repoworkspace` + `check_protected_paths.py` + `CODEOWNERS`), wired into a `cmd/summa42-box` workspace-collect helper.
6. **`M1A-T3-c`** *(depends on `M1A-T3-a`)*: `climodel` `AgenticSpec` / `CostControl` gate (`profile.go`) + `TestAutoApproveRefusedOutsideSandbox` + `TestClimodelAdapterRefusesAgenticMode`, wired into `cmd/summa42-box` profile probe/loader.
7. **`M1A-T3-d`** *(depends on `M1A-T2`, `M1A-T3-a/b/c`, `M2-T3`, `M9-T6`)*: Active kill watcher + startup orphan reaper + `Implementer` agentic executor wiring in `cmd/summa42-box` + `.github/workflows/ci.yml` OCI acceptance gate.
8. **`M1A-T4`, `M1A-T7`, `M1A-T6`, `M1A-T5`:** Follow-on cards as scheduled in Section 4.1.

---

## 5. Out of Scope

- Replacing one-shot triage, planning, or review stages with write-mode agents.
- Running candidate validation inside Docker (`M9-T1`, tracked separately; §2.7 defines the residual risk and controls until `M9-T1` lands).
- Monetary micro-USD pricing tables and paid per-token API accounting (`M3-T1`, `M3-T2`).
- Multi-host/remote Docker sandboxes or Kubernetes runners (deferred outside this single-Cube plan per Section 8).
