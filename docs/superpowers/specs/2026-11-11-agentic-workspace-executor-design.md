# Agentic workspace executor: full tools inside a sandbox

**Status:** the Owner answered the questions of section 12 on 11 November
2026; approval is the merge of this document. **Card:** M1A-T1 of the
[development plan](../plans/2026-11-11-development-plan.md), sections 2.5 and
2.6. **Base:** `main` @ `2db7d4a`.

No build card of M1A starts before the Owner approves this document.

## 1. Problem

A Summand today answers a bounded read-only question and returns complete-file
edits in one JSON document, at most 64 files and 256 KiB
(`internal/repoworkspace/candidate.go`). It never runs what it writes. The same
model in its own harness works for hours, builds, tests and iterates. The
Owner's goal is that Summa42 loses none of that, and that no agent can break
out or talk to another agent while doing it.

The rule from the plan: constrain the blast radius, not the agent.

## 2. Goals and non-goals

**Goals**

- One Attempt may run an agentic harness for hours with a shell, compilers,
  test runners and write access to a checkout.
- Nothing the agent does inside can cause an effect outside, read a secret of
  the Collective, or reach another agent.
- The result enters the existing pipeline unchanged: a candidate commit bound
  to a base SHA, then independent validation, independent review, publication
  through the commitment boundary.
- Every claim about isolation is one a test or a real run proves.

**Non-goals**

- Replacing triage, plan review or code review. They stay bounded one-shot
  calls. Review of changes too large for the one-shot reviewer is M1A-T5.
- Running without a container runtime. Without one there is no agentic stage;
  the existing complete-file stage remains the fallback.
- Multi-host fleets.

## 3. Overview

```text
Box (trusted)                               sandbox (untrusted, one per Attempt)
------------------------------------------  ---------------------------------------
pinned checkout  --copy-->                  /workspace/repo   (read-write)
TASK.md (plan, criteria, rules)  -->        /workspace/TASK.md
heartbeat: renew lease, budget,             harness in agent mode: edits, builds,
  emergency state, wall time, disk            tests, commits on branch "work"
gateway + fetch + registries  <--only-->    isolated network, no default route
collector container (no network,  <--ro--   /workspace
  no credentials) writes a Git bundle
fresh Box-owned repository: fetch bundle,
  fsck, path rules, squash to one commit
        |
        v
candidate (as today) -> validation -> code review -> publication
```

## 4. The sandbox

### 4.1 Runtime and profile

One OCI container per Attempt through the existing TEB provider
(`internal/teb`: `DockerProvider`, `DockerArgs`). The backend binary is
configurable, so rootless Podman works where its command line matches.

A rootful Docker daemon makes the Box user equivalent to root on the host. The
supported setups are rootless Docker or rootless Podman; with a rootful daemon
the Box logs a warning at startup and the run manifest records it (decision 1
in section 12).

A new named profile, `oci-agentic`, carries every guarantee of
`EnforcedProxyProfile`: read-only root, non-root uid, all capabilities
dropped, no-new-privileges, PID, memory and CPU limits, isolated network with
mediated egress, workspace-only write, no ambient credentials, no container
socket. The default seccomp profile of the runtime stays on. The container is
never privileged and never gets a device.

`DockerArgs` gains, for this profile only: `--tmpfs` mounts for `/tmp` and for
the agent's home (size-limited, `noexec` not set because builds run there), a
label `summa42.attempt=<attempt id>`, and a hard stop time. No other host path
is mounted except the workspace and an optional read-only package cache.

### 4.2 Image

The Project manifest names the image by digest, never by tag. The Owner builds
it from a Dockerfile kept in the repository under `deploy/sandbox/`; it holds
the toolchains the Project needs and the harness CLIs. Because the manifest is
Owner-signed, so is the choice of image. The image contains no credential.

### 4.3 Filesystem

| Path in the container | Backing | Access |
| --- | --- | --- |
| `/workspace/repo` | copy of the pinned checkout, per Attempt, on the host | read-write |
| `/workspace/TASK.md` | written by the Box | readable; the Box never reads it back, so a change to it has no effect |
| `/workspace/out/` | empty directory | read-write; optional `REPORT.md` |
| `/home/agent`, `/tmp` | tmpfs | read-write, gone with the container |
| `/cache` | Box-maintained package cache of the Project | read-only, optional |
| everything else | image | read-only |

The workspace directory is created by the Box, owned by the container uid, and
lives outside every role home and outside `SUMMA42_HOME`. Bind mounts have no
quota, so disk use is checked by the heartbeat (section 6) and the limit is
recorded as monitored, not enforced.

### 4.4 Network

The container joins an isolated network with no default route. The only
reachable address is the Box-side gateway. The gateway is three things on one
listener, each a refusal by default:

1. **Provider gateway** (section 5): forwards model requests to the profile's
   provider.
2. **Registry mirror:** `GET` only, to the package registries the manifest
   lists (for example the Go module proxy, npm, PyPI, NuGet), with a cache.
3. **Fetch:** `GET` only, to the domains the manifest lists for the Project,
   with caps on URL length, response size and request rate. The response is
   returned as data. It is refused entirely for a repository whose data
   class is not `public` (section 5.1): a URL is a way to carry text out.

Every request and every refusal is recorded. A refusal is a rogue signal on
the Attempt. The existing CONNECT proxy (`internal/teb/egress_proxy.go`) stays
the outbound side of the gateway: exact allowlisted hosts, addresses checked
against non-public ranges, the validated address dialed directly.

## 5. Credentials

A harness must authenticate to its provider. The plan requires that no
credential that can cause an effect is readable by a model (R12). A CONNECT
proxy cannot add a credential to a TLS connection it only tunnels, so there
are two modes, and each profile declares which one it uses.

**Mode G, gateway.** For harnesses that accept a base URL and a key or bearer
token (the direct API providers of M3-T6, and any CLI with a base-URL
setting). The harness inside talks plain HTTP to the gateway with a
placeholder key; the gateway adds the real credential and forwards over TLS.
The credential is never in the sandbox. This mode carries the
`NO_AMBIENT_CREDENTIALS` guarantee and the profile can be `ENFORCED`.

**Mode L, login inside.** For subscription harnesses whose login cannot be
mediated. The Owner's current access is all of this kind (Gemini through
`agy`, Claude, Command Code, opencode). A dedicated login for sandbox use,
separate from the role homes, is kept by the Box per profile; only its token
file is mounted into the container, the rest of the harness's configuration
directory is generated fresh for each run from a Box-side template. The
outbound side allows only the provider's hosts.

What Mode L means, stated plainly:

- The agent can read that token. Its reach is the provider account: it can
  spend the subscription's quota. It cannot reach a repository, the Box or
  another service, because the network allows nothing else.
- The host allowlist cannot tell accounts apart, so the agent could send what
  it can read to the same provider under another account if it obtained one.
  What it can read is the checkout. The Owner accepted this for code
  (section 5.1); it is the reason customer data is never inside a sandbox.
- A login that is also a credential elsewhere is never used in Mode L. Copilot
  is excluded (its login is a GitHub credential).
- The profile is `PARTIAL`, without the `NO_AMBIENT_CREDENTIALS` guarantee,
  and the run manifest says "provider login inside".
- A harness may refresh its token. After the run the Box validates the token
  file as data (known keys, bounded sizes) before keeping it for the next run;
  anything else is discarded and the Owner logs in again. That file is the one
  piece of agent-writable state that survives a run; its validation is what
  keeps it from being a channel of any width.

### 5.1 Data classes

Owner decision of 11 November 2026, replacing the earlier single rule
"private means zero data retention". Every repository and every source in a
Project manifest carries one of three classes:

| Class | Example | Models | Mode L | Fetch |
| --- | --- | --- | --- | --- |
| `public` | the `summa42` repository | any profile | allowed | allowed, per-Project domain list |
| `private-code` | the product's repositories | subscription profiles allowed; the provider sees the code without a retention guarantee | allowed | refused |
| `customer-data` | support requests, customer records, anything read from the product's database or production systems | zero-data-retention provider or local model only | refused | refused |

A sandbox never receives `customer-data`: no production credential, no
database access, no mounted export. A Task that needs such data is not an
agentic Attempt. If a repository itself contains customer data (fixtures,
dumps), its class is `customer-data` and the rules of that row apply; the
collection step's secret scan does not detect this, so the class is the
Owner's statement in the manifest.

Whether a given harness runs headless in a container in write mode, where its
token lives and how it refreshes is not knowable from documentation alone.
Card M1A-T1b is a spike that answers it per harness before M1A-T3 is built.

## 6. One Attempt

1. **Lease.** The scheduler leases the Attempt as today. The executor refuses
   unless the TEB provider is available and assesses the requested profile.
2. **Prepare.** `repoworkspace.Checkout` prepares the pinned checkout; a copy
   becomes `/workspace/repo` on a branch `work` at the base SHA. The Box
   writes `TASK.md`: the accepted plan, the acceptance commands, the paths it
   must not touch, and the instruction to commit as it goes and to write
   blockers to `out/REPORT.md`.
3. **Run.** The harness starts in agent mode (section 7) with `TASK.md` as its
   task.
4. **Heartbeat.** While the container lives, the Box renews the lease
   (`execution.RenewLease`) at a third of the lease duration and at each
   renewal checks: emergency state (break glass, safe mode, Project pause,
   containment of this Attempt), the envelope, the wall-time ceiling, disk
   use. Any failed check, a refused renewal or a lost lease kills the
   container. Emergency state is polled at least every 30 seconds between
   renewals, so a freeze stops a run within that time and not at the next
   renewal.
5. **Checkpoint.** Every N renewals the Box runs collection (section 8) and
   stores the bundle as checkpoint evidence (M1A-T4).
6. **End.** The harness exits, or the Box kills it. The Box runs collection
   once more, removes the container and the workspace, and records usage.
7. **Orphans.** The container carries the Attempt's label and a hard stop time
   equal to the wall-time ceiling. On every tick and at startup the Box lists
   containers with the label and kills every one whose Attempt has no live
   lease.

Wall-time ceiling: 8 hours per Attempt by default, set in the manifest
(decision 4). Until the money ledger exists (M3-T2) only profiles marked
`PREPAID_QUOTA` may run agentic Attempts; a paid profile is refused.

## 7. Harness in agent mode

A harness profile gains an optional `agent` section: the argument run that
starts the harness with write and execute permission and no confirmation
prompts, how the task is delivered, and its credential mode.

The validator in `internal/ghtriage/climodel/profile.go` refuses auto-approve
options today. It keeps refusing them everywhere except in the `agent`
section, and the `agent` section can be turned into a command only by the
sandbox executor, whose constructor requires an assessed `oci-agentic`
profile. There is no flag and no environment variable that runs an `agent`
section on the host. A test asserts that the one-shot adapter cannot be built
from it.

## 8. Collection

The agent has had write access to `/workspace/repo/.git` for hours. Git
configuration there can run programs (hooks, `core.fsmonitor`, pagers, pack
hooks, includes). Therefore:

1. The Box never runs Git, or any other tool, in the agent's directory.
2. The Box starts a **collector** container: a minimal Box-built image with
   Git only, pinned by digest, no network, no credentials, the same hardening
   as the sandbox, the workspace mounted read-only. It runs
   `git bundle create - work ^<base sha>` and writes the bundle to standard
   output. Hostile configuration executes there, where it can reach nothing.
3. The Box reads the stream under a size cap and a timeout into a file.
4. In a **fresh, Box-owned** bare repository that already holds the base
   commit, the Box fetches from the bundle file with object checking on
   (`transfer.fsckObjects`), then verifies by its own rules, all
   deterministic:
   - the bundle's only prerequisite is the pinned base SHA, and every
     collected commit descends from it;
   - no submodule entries, no symlink that leaves the tree, text files only
     where the existing candidate rules require it;
   - no path in the Project's protected list changed, which always includes
     CI and pipeline definition files (rule R5), also in a greenfield Project;
   - changed paths lie inside the manifest's allowed prefixes;
   - size limits from the manifest (they replace the fixed 64 files and
     256 KiB for this stage);
   - the existing secret patterns do not match the added content.
5. The Box creates **one commit of its own** on the base whose tree is the
   tree of the agent's last commit. Author, committer and message are the
   Box's. The agent's commit messages and history are not published; they are
   kept as evidence and pass M9-T8 before any model sees them.
6. That commit is the candidate. From here the existing implement stage's
   record is written unchanged: plan and review citations, base, candidate,
   tree and diff identity.

`out/REPORT.md` is not part of the bundle. The collector copies it out as a
separate size-capped text stream; it is untrusted and is shown to the reviewer
only as delimited data (M9-T8).

A rule violation in step 4 rejects the candidate with the reason, records a
rogue signal for a protected-path change, and leaves the case to the bounded
repair of M1-T1. Nothing the agent says about its own result (tests pass,
task complete) is recorded as a fact.

## 9. What stays outside

Unchanged and required:

- Validation runs the candidate independently of the sandbox the agent used.
- Code review reads the diff. A candidate beyond the one-shot reviewer's
  projection limits (64 paths, 512 KiB) is not truncated: it is held for the
  Owner until M1A-T5 provides a read-only agentic reviewer in its own sandbox,
  as the reviewer user's counterpart.
- Publication, approval, delegation: as today.

## 10. Threats and what answers them

| Threat | Answer | Test |
| --- | --- | --- |
| Agent reads the Owner key, tokens, state | Nothing of `SUMMA42_HOME` is mounted; no host path but the workspace | `TestNoBoxPathMounted` |
| Agent reaches a repository or any service directly | No route but the gateway | `TestSandboxHasNoRouteButGateway` |
| Agent carries private code out | Fetch refused for `private-code`; the remaining path is the provider itself, which the Owner accepted for code | `TestFetchRefusedForNonPublicClass` |
| Customer data reaches a model without zero data retention | `customer-data` is never mounted in a sandbox and refuses Mode L and every profile without zero data retention | `TestCustomerDataRefusesLoginMode`, `TestCustomerDataSourceNeverMounted` |
| Agent talks to another agent | Separate network and mount namespace per Attempt; nothing survives a run except the validated token file and Box evidence | `TestTwoSandboxesCannotReachEachOther`, `TestHomeIsFreshEachRun` |
| Hostile Git configuration runs as the Box | Collection in a container; Box fetches a bundle into its own repository | `TestHostileGitConfigIsNotExecuted` |
| Agent writes CI definitions that run with deployment secrets | Protected paths checked at collection in every Project | `TestPipelineFileChangeRejectsCandidate` |
| Agent's claim is taken as fact | Only the collected tree and independent validation count | `TestAgentClaimIsNotRecordedAsResult` |
| Run continues after break glass | Emergency state polled; container killed | `TestFreezeKillsSandboxWithinPollInterval` |
| Run spends without limit | Prepaid profiles only, wall-time ceiling, envelope checked at renewal | `TestPaidProfileRefusedWithoutMoneyLedger`, `TestWallTimeCeilingKills` |
| Box crashes, container keeps running | Label, hard stop time, reaper | `TestOrphanContainerIsReaped` |
| Auto-approve harness started on the host | `agent` section usable only by the sandbox executor | `TestAgentSectionRefusedOutsideSandbox` |
| Poisoned image | Digest in the Owner-signed manifest | `TestImageByTagRefused` |
| Container escape through a kernel defect | Not answered by this design beyond rootless runtime, dropped capabilities and seccomp. A hardened runtime (gVisor, Kata) can replace the backend later without changing the executor | none; stated as a residual risk |

The fakes for these tests are a fake container backend that records its
arguments; the guarantees that depend on a real runtime are proven by tests
under the existing `oci` build tag, which CI runs in the `oci-acceptance` job.
A guarantee that only a fake checked is not claimed.

## 11. Build cards

This replaces the card list of M1A in the plan where they differ; the plan is
updated in the same pull request.

| Card | Content | Depends |
| --- | --- | --- |
| M1A-T1b `OWNER` with `agent` | Harness spike: for `agy`, Claude, Command Code and opencode, run each headless in the sandbox image in write mode; record whether it works, its credential mode, where its token lives, how it refreshes. Result in `docs/superpowers/research/`. Needs the Owner's logins | this spec approved |
| M1A-T2 `BUILD` | Long Attempts: renewal by heartbeat, emergency polling, kill on any failed check (section 6, steps 4 and 7) against a fake backend | this spec approved |
| M1A-T8a `BUILD` | Gateway: registry mirror and fetch (section 4.4), recorded requests, refusals as rogue signals | this spec approved |
| M1A-T8b `BUILD` | Provider gateway, Mode G (section 5) | M1A-T8a |
| M1A-T3a `BUILD` | `oci-agentic` profile, tmpfs and label arguments, sandbox executor, image by digest, orphan reaper | M1A-T2, M2-T3 |
| M1A-T3b `BUILD` | Collection (section 8) and the candidate record | M1A-T3a |
| M1A-T3c `BUILD` | `agent` section of harness profiles and its validator (section 7); Mode L with token validation | M1A-T1b, M1A-T3a, M9-T6 |
| M1A-T4 `BUILD` | Checkpoints and resume from the Box-side repository | M1A-T3b |
| M1A-T7, M1A-T6, M1A-T5 | as in the plan | M1A-T3c |

M1A-T3a to M1A-T3c touch protected paths (`internal/ghtriage/climodel/profile.go`,
the CI workflow for new `oci` tests); the Owner reviews them line by line.

## 12. Owner decisions (11 November 2026)

1. Container runtime: rootless Podman or rootless Docker is the target on the
   new VM; a rootful daemon is accepted with a recorded warning.
2. Mode L with a login made for sandbox use only: yes, for `summa42` and also
   for the code of the `daas` Project.
3. Data classes as in section 5.1: code may go to subscription models;
   customer data stays with zero-data-retention or local models and never
   enters a sandbox.
4. Wall-time ceiling per Attempt: 8 hours, set in the manifest.
5. The Owner builds the sandbox image from the Dockerfile in the repository; a
   later card may let a pipeline case propose changes to that Dockerfile as an
   ordinary reviewed change.
