# M1A-T1 Design: Agentic Workspace Executor

**Card:** `M1A-T1` (`DESIGN`) — [development plan](../plans/2026-11-11-development-plan.md#m1a-agentic-execution)
**Date:** 2026-11-26
**Status:** Proposed (awaiting Owner approval before `M1A-T8` and `M1A-T3` implementation)
**Vision & Plan Alignment:** [Vision](../../VISION.md) (Articles 6, 9, 10, 11, 17, 18, 21), [Plan Section 2.5](../plans/2026-11-11-development-plan.md#25-design-goal-no-runaway-agents-no-private-channels), [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary)

---

## 1. Problem

Every model invocation in Summa42's GitHub pipeline (`internal/ghtriage/`) is currently a short, read-only or tool-free call:

1. `internal/ghtriage/climodel/profile.go` requires every `Profile` to declare `ReadOnly` or `NoTools: true` and rejects write/auto-approve flags (`dangerousArgs`).
2. `internal/ghtriage/implementation.go` (`Implementer.prepare`) sends a bounded JSON snapshot (at most 32 files / 256 KiB) and accepts back a single JSON object (`ImplementationOutput`) with at most 64 complete-file edits. The model never runs compilers, test suites, linters, or exploration commands on the repository.
3. Meanwhile, the trusted execution boundary primitives in `internal/teb/` (`DockerProvider`, `OCIRequest`, `EnforcedProxyProfile`, `ConnectProxy`, `ModelEgressAssessment`), the process-group/cgroup launcher (`internal/modelrun/`), pinned checkouts (`internal/repoworkspace/`), `execution.Service.RenewLease` (`internal/execution/service.go`), and the executor abstraction (`internal/executors/executor.go`) already exist in part, but are not composed into an end-to-end sandboxed agentic implementation path.

As stated in [Plan Section 2.6](../plans/2026-11-11-development-plan.md#26-design-goal-full-capability-inside-the-boundary), governance must constrain the blast radius rather than crippling the agent: inside an isolated sandbox an agentic harness should have full write/build/test tools for hours, while everything crossing the sandbox boundary remains governed by deterministic trusted code.

---

## 2. Decisions

### 2.1 Sandbox: Per-Attempt OCI Container (`internal/teb`)

Each agentic implementation Attempt runs inside a dedicated, ephemeral OCI container managed by `teb.DockerProvider` (`internal/teb/docker_provider.go`, `internal/teb/oci.go`), started with `teb.EnforcedProxyProfile()` (`domain.EnforcementEnforced`):

- **Root filesystem & privileges:** `--read-only`, `--cap-drop=ALL`, `--security-opt=no-new-privileges`, `--user=65532:65532` (non-root fixed UID/GID).
- **Resource limits:** `--pids-limit`, `--memory`, `--cpus` (non-zero, mandatory via `teb.DockerArgs`).
- **Writable storage (workspace and caches only):**
  - `/workspace`: bind-mounted from a per-Attempt scratch directory owned by the Box (`--mount=type=bind,src=<attempt-workspace>,dst=/workspace`, `--workdir=/workspace`), pre-populated with a dissociated shallow/full clone checked out at `BaseSHA` with `core.hooksPath=/dev/null` and clean `git config` (no host remotes or credentials).
  - `/tmp` and toolchain caches (`GOMODCACHE`, `GOCACHE`, npm/pip cache): bounded `tmpfs` mounts (or per-Attempt writable cache bind mounts outside `.git` with explicit size caps), mounted with `nosuid,nodev` (note: `/tmp` and `/workspace` allow `exec` inside the container because Go/test runners compile and execute test binaries there).
- **No credentials & no container socket:**
  - `cmd.Env` for `docker` stays empty/minimal (`internal/teb/oci.go:123`); container environment receives only non-secret config (`HOME=/tmp/home`, `TMPDIR=/tmp`, proxy URL, and non-secret tool variables).
  - `validateWorkspace` rejects any workspace containing `docker.sock` or host root, and `DockerArgs` never binds `/var/run/docker.sock`, `/var/run/podman`, `~/.summa42`, `~/.config`, or Box credential files.
  - Note on `teb/profile.go` honest claims (Rule R7): `GuaranteeNoAmbientCredentials` and `GuaranteeNoContainerSocket` must be verified structurally in `DockerArgs` (refusing credential-file env vars and socket bind mounts) before `DockerProvider.Execute` claims `EnforcementEnforced`.
- **Container naming & tracking:** Every container is launched with `--name=s42-attempt-<attemptID>` (or a deterministic label `--label=summa42.attempt=<attemptID>`) and `--cidfile` so external cancellation/freeze can kill the exact container deterministically without relying solely on `docker run` signal propagation.

#### Host without Docker (honest degradation)

Summa42 **never** fakes `ENFORCED` containment when Docker/OCI is absent:

1. If `teb.DockerProvider.Available(ctx)` returns `false` (or the host lacks a functional OCI daemon), any Task/Attempt requiring `domain.EnforcementEnforced` or requesting an agentic write-mode harness **fails closed** (`teb.ErrProviderUnavailable`) — it never falls back to running a write-mode/auto-approve CLI on the host OS.
2. On hosts without Docker, `Implementer` (`internal/ghtriage/implementation.go`) continues to offer the existing **complete-file JSON edit stage** (`UNENFORCED` model call as `summa42-model` returning `ImplementationOutput` + `PARTIAL` validation as `summa42-validate`).
3. Native host adapters (`Command`, `CopilotExecutor`, `climodel.Adapter`) remain strictly `UNENFORCED` or `PARTIAL` and are barred from agentic write/auto-approve mode.

---

### 2.2 Container Image & Toolchains

Images are **Project-configured data**, never agent-selected:

1. The Project manifest (`M2-T3`) specifies an approved OCI image reference pinned by immutable digest (`<registry>/<image>@sha256:<digest>`, or an Owner-approved local image digest recorded in the manifest) matched to the Project's toolchain needs (e.g., Go 1.27 + Git, Python 3.12, Node.js, .NET).
2. **Base contents:** minimal Linux userland, `git`, language toolchains/compilers, and the headless agentic harness binary (e.g. `claude`, `codex`, `opencode`, `copilot`) installed read-only as root (`0755`), executable by UID `65532`.
3. **Explicit exclusions:** no `sudo` / setuid binaries, no container CLI (`docker`, `podman`), no SSH client/keys, no pre-baked CLI login files (`~/.claude`, `~/.codex`, `~/.gemini`, `~/.copilot`), and no outbound TLS client certs for host services.
4. **Offline/air-gapped dependency support:** language dependency caches (e.g. read-only Go module mirror/cache or `vendor/`) can be mounted read-only (`readonly` bind mount) from Box-prepared caches per Project manifest so builds work without broad internet access.

---

### 2.3 Network, Egress Proxy, and Mediated Fetch (`M1A-T8`)

The sandbox container attaches to an isolated, Box-managed internal Docker bridge/network (`--network=<attempt-net>`, created with `--internal` so the container has **no route to the internet, LAN, or host metadata IPs** like `169.254.169.254`), with host-gateway reachability solely to the Box egress proxy IP:port (`NewConnectProxyOn` in `internal/teb/egress_proxy.go`).

Three network capabilities exist through the Box egress edge; **everything else is denied** and emits a rogue containment signal (`egress_denied`):

1. **Model provider proxy (credential injection + usage metering):**
   - Because end-to-end TLS `CONNECT` prevents the proxy from injecting `Authorization` headers or reading token usage without custom container CA injection, the sandbox harness talks **plain HTTP** inside the isolated internal network to a Box-hosted provider reverse-proxy endpoint on the gateway (e.g., `OPENAI_BASE_URL=http://<proxy-gw>:<port>/provider/openai`, `ANTHROPIC_BASE_URL=http://<proxy-gw>:<port>/provider/anthropic`).
   - The proxy validates that the target provider matches the Attempt's configured profile, strips any client-supplied auth headers, injects the Box-held provider credential **outside** the sandbox, dials the allowlisted upstream HTTPS endpoint (using `dialAllowedTarget` / `publicEgressIP` DNS-rebinding checks), streams the response back, and parses reported token usage / rate-limit errors directly from the provider response into Box usage/budget accounting.
   - No provider API key or OAuth token ever enters the container environment or filesystem.
2. **Package registry path:**
   - Read-only `GET`/`HEAD` access through the proxy (or pre-populated read-only cache mount) restricted to the Project manifest's allowlisted package registry hosts (e.g., `proxy.golang.org`, `sum.golang.org`, `registry.npmjs.org`, `pypi.org`), with DNS-rebinding validation (`publicEgressIP`). Write/publish methods (`PUT`, `POST`, `DELETE`) to registries are refused.
   - If the Project's `data_class` is `private`, arbitrary public registry fetches are restricted to verified immutable-checksum fetches or pre-staged Box caches as specified in the manifest.
3. **Mediated fetch tool (containment, not logging):**
   - Exposed to the agent inside the sandbox via a local HTTP endpoint (`http://<proxy-gw>:<port>/s42/fetch`) or lightweight wrapper/MCP tool backed by the Box host service.
   - **Hard containment rules:**
     - **Domain allowlist:** per-Project explicit domain allowlist (exact lowercase host match; no wildcards, no private/link-local/loopback IPs via `publicEgressIP`).
     - **Method & URL bounds:** `GET` only; redirects are either refused or re-validated hop-by-hop against the allowlist and `publicEgressIP`; maximum URL length (e.g., `2048` bytes).
     - **Rate & size caps:** per-Attempt request count cap (e.g., `50` requests), sliding-window rate cap (e.g., `10/min`), and maximum response body cap (e.g., `1 MiB`).
     - **Private data class refusal:** if the Project or source `data_class == "private"`, **all** mediated fetch requests are refused unconditionally (`403` + rogue signal if probed outside policy).
     - **Provenance & untrusted marking:** every allowed fetch records an immutable Box evidence object (`agent.fetch.v1` with URL, timestamp, status, SHA-256 of body, byte count), and the response returned to the sandbox is wrapped/tagged as untrusted external data (`X-Summa42-Untrusted: true` / JSON envelope).
     - Every refused egress or fetch attempt is logged and emits a containment signal on the Attempt.

---

### 2.4 Write-Mode Harness Invocation and Safety Gate (`climodel/profile.go`)

Today, `internal/ghtriage/climodel/profile.go` enforces `ReadOnly` or `NoTools` and rejects `dangerousArgs` (`yolo`, `dangerously`, `allow-all`, `bypass`, `full-auto`, `auto-approve`, `accept-edits`, `workspace-write`, `--sandbox=none`, etc.). That invariant **stays untouched** for all native host calls (`run-gh-triage*`, `run-gh-plan*`, `run-gh-code-review`, and non-OCI fallback).

To support agentic implementation inside the sandbox:

1. **Explicit `AgenticWrite` profile capability (data, not ad-hoc flags):**
   - A `climodel.Profile` gains an optional `Agentic *AgenticSpec` block (e.g., `write_args` specifying the exact argv for autonomous write/tool mode inside the sandbox, e.g. Codex `--full-auto` or Claude `--permission-mode acceptEdits` / `--dangerously-skip-permissions` within the container).
   - `Profile.Validate()` continues to **reject** `dangerousArgs` anywhere inside `Args`, `ReadOnly`, and `Input.Flag`, and rejects `Agentic` unless `ReadOnly`/`NoTools` is also validly defined (preserving read-only use of the same profile).
2. **Runtime enforcement gate (safe there and only there):**
   - `climodel.Adapter` (the native OS-user runner) **unconditionally refuses** `Agentic` / `AgenticWrite` mode.
   - Only the OCI Agentic Executor (`M1A-T3`), after `teb.Manager` / `teb.DockerProvider.Assess` returns `domain.EnforcementEnforced` with **all 12 guarantees** of `teb.EnforcedProxyProfile()` active (plus verified non-root container, isolated internal network, no credentials, no container socket), may construct the container command using `Agentic.WriteArgs`.
   - **Why this is safe inside the OCI sandbox and nowhere else:**
     - On the host (`summa42-model`), write/auto-approve mode lets the harness execute arbitrary host processes with access to `/var/lib/summa42-model` (CLI OAuth logins), host networking, and shared host files.
     - Inside the per-Attempt OCI container, there are **zero** credentials, **zero** host filesystem access outside `/workspace`, **zero** outbound internet routes except the mediated proxy, strict PID/RAM/CPU/disk/wall-time bounds, and nothing written inside `/workspace` can escape without passing trusted bundle verification and independent validation/review.

---

### 2.5 Result Extraction: Git Bundle Collection and Zero Trust in Agent Output

The agent has write access to `/workspace`, including `/workspace/.git`. Therefore **trusted Box code never runs `git`, hooks, `go`, or any inspection tool inside `/workspace`**.

1. **Agent completion vs. result truth:**
   - Anything the harness prints to stdout/stderr, writes to status files, or claims about passing tests is treated strictly as non-authoritative telemetry (at most a summary text checked by `internal/texthygiene`).
   - Only committed Git state created inside `/workspace` is eligible as a candidate. (If the agent left uncommitted changes in the working tree when it exited, an in-container helper step under the same UID `65532` or pre-prompt instructions commit them, or uncommitted state is treated as non-candidate/incomplete — the host **never** runs `git add`/`git commit` inside `/workspace` as the Box user.)
2. **Bundle export from inside the sandbox:**
   - Before tearing down the container (or in a short unprivileged post-run container step sharing `/workspace` with `--network=none` and `--read-only`), the unprivileged container UID `65532` runs:
     `git -C /workspace bundle create /workspace/.s42-candidate.bundle <BaseSHA>..HEAD` (with `GIT_CONFIG_NOSYSTEM=1` and `-c core.hooksPath=/dev/null`).
   - Trusted Box code verifies `/workspace/.s42-candidate.bundle` is a regular file (not a symlink escaping `/workspace`) under a bounded byte cap (configured per Project manifest, e.g., 64 MiB).
3. **Host-side verification in a clean Box-owned repository:**
   - The Box creates a brand-new, empty temporary repository (`git init`) outside `/workspace` owned by the Box user, seeds `BaseSHA` from the trusted source repository, and fetches from the bundle with strict object checks:
     `git -c core.hooksPath=/dev/null -c transfer.fsckObjects=true fetch <bundle> HEAD:refs/s42/candidate`
     followed by `git fsck --strict --full`.
   - The Box verifies:
     - `BaseSHA` is an ancestor of `HEAD` (and collapses/checks single-parent or clean linear history according to `repoworkspace` / M1A-T5 rules).
     - Changed paths respect the Project's `allowed_paths` / `allowed_prefixes` (or greenfield bounds) and **never** touch protected paths (see Section 3, rule 5).
     - Every changed text file passes `internal/repoworkspace` hygiene (`checkEditHygiene` / `texthygiene`) and secret checks.
4. **Independent validation & independent review:**
   - The extracted candidate SHA from the Box-owned repository is then passed to the existing independent validator (`summa42-validate`, or OCI validator under `M9-T1`) and independent reviewer (`summa42-review`, distinct model & OS user).

---

### 2.6 Attempt Limits and Lifecycle Enforcement (`M1A-T2`)

Agentic Attempts can run for hours while staying bounded across four axes:

| Limit | Mechanism | Enforcement Point |
| --- | --- | --- |
| **Wall time & liveness** | Short initial lease (e.g. `10m`) renewed by periodic heartbeat via `execution.Service.RenewLease` (`M1A-T2`), plus hard `max_attempt_wall_time` (e.g. `2h` default, configurable in Project manifest) | Outer Box Go context (`context.WithDeadline`) + `RenewLease` failure immediately kills container |
| **Money & quota** | `RenewLease` checks the Attempt's resource/money envelope before extending (`M1A-T2`, `M3-T2`); proxy meters provider tokens/errors in real time. **Until `M3-T2` ships, agentic Attempts allow `PREPAID_QUOTA` subscription profiles only** | Pre-start eligibility check + every `RenewLease` tick + proxy edge |
| **Compute** | OCI `--pids-limit`, `--memory`, `--cpus` via `teb.DockerArgs` | Kernel cgroup enforcement inside OCI runtime |
| **Disk** | Bounded `tmpfs` sizes for `/tmp` and caches + periodic host `du`/quota check on `<attempt-workspace>` (default e.g. `2 GiB`) | Container runtime + Box lifecycle watcher |

**Active Kill Watcher (stopping renewal is not enough):**
A running Box watcher goroutine polls/subscribes to:
- `emergency` break-glass (`FROZEN`) and safe-mode transitions,
- Attempt containment (`containment` `RESTRICT`/`TERMINATE`),
- `RenewLease` errors (revoked/expired lease or exhausted resource/money envelope),
- disk ceiling breaches and overall wall-time deadline.

On **any** trigger, the watcher executes `docker kill s42-attempt-<attemptID>` and `docker rm -f s42-attempt-<attemptID>` immediately (`context.WithoutCancel(ctx)` with bounded timeout) and cancels the container execution context.

---

### 2.7 Existing One-Shot Stages

Existing one-shot, read-only/structured stages remain unchanged:
- **Issue triage & triage review** (`stage2`, `reviewer`) stay one-shot `read_only` / `no_tools`.
- **Grounded planning & plan review** (`planner`, `planreview`) stay one-shot / bounded read-only loops.
- **Independent code review** (`codereview`) stays a separate `read_only` invocation under `summa42-review` reading the Box-verified candidate diff and independent validation output.
- **Complete-file edit implementation** (`internal/ghtriage/implementation.go`) remains as the non-sandbox fallback (`M1A-T3`) when Docker/OCI is unavailable or for small `pipeline` tasks.

---

## 3. Mandatory Security Closures and Named Tests

Every security condition required by `M1A-T1` is bound to deterministic verification tests in `M1A-T2`, `M1A-T8`, and `M1A-T3`:

1. **Bundle export & strict `fsck` (never run Git/tools inside agent `.git` as Box user):**
   - **Closure:** Even if the agent writes malicious `.git/config` (`fsmonitor`, `core.sshCommand`, `core.editor`), `.git/hooks/*`, or symlinks inside `/workspace/.git`, the Box never invokes `git` against `/workspace` directly; it exports a `.bundle` as UID `65532` and fetches into a fresh Box-owned repo with `core.hooksPath=/dev/null` and `transfer.fsckObjects=true` + `git fsck --strict`.
   - **Named tests:** `TestResultIsCollectedViaBundleAndFsckNotAgentGit`, `TestHostileGitConfigIsNotExecuted`, `TestResultIsReadFromGitNotFromAgentClaim`.

2. **Zero credentials inside sandbox; egress proxy injects and meters:**
   - **Closure:** Provider API keys live solely in the Box egress proxy process outside the container. Container `env`, mounts, and image contain no credentials. Provider responses are metered at the proxy.
   - **Named tests:** `TestNoCredentialReachableInside`, `TestCredentialInjectedOutsideSandbox`, `TestProxyMeasuresProviderUsage`.

3. **Mediated fetch is hard containment, not logging:**
   - **Closure:** Per-Project host allowlist, `GET` only, bounded URL length, rate/size caps, DNS-rebinding check (`publicEgressIP`), **total refusal when `data_class == "private"`**, evidence recording (`agent.fetch.v1`), and untrusted-data tagging.
   - **Named tests:** `TestOnlyAllowlistedHostsReachable`, `TestFetchRefusedForPrivateDataClass`, `TestFetchOutsideAllowlistRefused`, `TestMediatedFetchEnforcesMethodLengthAndRate`.

4. **Inter-Attempt isolation through M9-T8 hygiene only:**
   - **Closure:** Container filesystem, `/tmp`, and harness session files are destroyed at Attempt end. Only committed Git checkpoints (`M1A-T4`) and Box evidence that passed `internal/texthygiene` (`Check` + `ScanRoleDirectives` from `M9-T8`) may project into a subsequent Attempt.
   - **Named tests:** `TestNoHarnessStateCrossesAttempts`, `TestCheckpointTextPassesM9T8Hygiene`.

5. **CI and pipeline files protected in every Project (including greenfield):**
   - **Closure:** Candidate validation rejects modifications to `.github/workflows/`, `azure-pipelines*.yml`, `azure-pipelines*.yaml`, and Rule R5 protected paths in **all** Projects (including `greenfield: true` in `M1A-T5`), preventing privilege escalation via ADO deployment service connections or GitHub Actions.
   - **Named tests:** `TestProtectedPipelineFilesRejectedInGreenfieldProject`.

6. **Freeze, containment, lease expiry, and exhausted budget kill the sandbox:**
   - **Closure:** Active watcher terminates the running OCI container immediately via `docker kill` / `docker rm -f` upon break-glass freeze, Attempt containment, `RenewLease` failure, or budget exhaustion.
   - **Named tests:** `TestFreezeKillsSandbox`, `TestContainmentOrBudgetExhaustionKillsSandbox`, `TestFreezeStopsRenewal`, `TestRenewalRefusedWhenEnvelopeSpent`.

7. **Prepaid-only gate until money ledger (`M3-T2`):**
   - **Closure:** Until `M3-T2` is implemented and wired, the agentic executor refuses any model profile not marked `PREPAID_QUOTA` and enforces an external hard wall-time deadline per Attempt.
   - **Named tests:** `TestPaidProfileRefusedWithoutMoneyLedger`.

---

## 4. Implementation Roadmap & Dependencies

This spec feeds the following downstream cards in [Section 4.1](../plans/2026-11-11-development-plan.md#41-execution-order):

1. **`M1A-T2` (`BUILD`):** Extend `execution.Service.RenewLease` (`internal/execution/service.go`) with atomic envelope/budget checks and emergency/containment revocation guards, plus the 5 named lease tests.
2. **`M1A-T8` (`BUILD`):** Extend `internal/teb/egress_proxy.go` into the sandbox HTTP/CONNECT egress proxy (provider credential injection, response usage metering, mediated fetch containment, rogue egress signals).
3. **`M1A-T3` (`BUILD`, split per R8, depends on `M2-T3` & `M1A-T1 approved`):** Wire `teb.DockerProvider`, bundle collection/fsck, active kill watcher, `climodel` `AgenticWrite` sandbox gate, and the OCI acceptance CI gate (touches protected `internal/ghtriage/climodel/profile.go` and `.github/workflows/ci.yml` with Owner review).
4. **`M1A-T4` (`BUILD`), `M1A-T7` (`BUILD`), `M1A-T6` (`OWNER` benchmark), `M1A-T5` (`DESIGN` + `BUILD`):** Checkpoints/resume, new-dependency gate, parity benchmark, and large/greenfield git-push delivery.

---

## 5. Out of Scope

- Replacing one-shot triage, planning, or review stages with write-mode agents.
- Running validation commands inside Docker (`M9-T1`; validation stays on `summa42-validate` until `M9-T1`).
- Monetary micro-USD pricing tables and paid per-token API accounting (`M3-T1`, `M3-T2`).
- Multi-host/remote Docker sandboxes or Kubernetes runners (deferred outside this single-Cube plan per Section 8).
