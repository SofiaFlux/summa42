# Architecture Spike: Evaluation of NVIDIA OpenShell as a Trusted Enforcement Boundary (TEB) Backend

**Issue:** [#57](https://github.com/SofiaFlux/summa42/issues/57)  
**Parent Epic:** [#3](https://github.com/SofiaFlux/summa42/issues/3) (*Slim Meeseek into a semantic control plane over mature OSS runtimes*)  
**Status:** Completed  
**Recommendation:** **ADAPT** (Provider-neutral abstraction; adapt Landlock & L7 egress patterns; retain local OCI TEB as zero-dependency fallback)  
**Date:** 2026-11-10  

---

## 1. Executive Summary

As part of Epic #3, this spike evaluated **NVIDIA OpenShell** as an execution backend for Summa42's Trusted Enforcement Boundary (TEB).

The guiding architectural principle of Summa42 is:
> **Meeseek owns semantics; providers and projections own mechanisms.**

Meeseek must retain exclusive, sovereign ownership over:
- Authority and policy precedence (Constitution, Human Trust Boundaries).
- Task vs Attempt lifecycle and fence validation.
- External Operation commit semantics (`PREPARED` → `DISPATCHED` → confirmed/unknown).
- Machine-verifiable enforcement levels (`ENFORCED`, `PARTIAL`, `UNENFORCED`) backed by cryptographic evidence hashes.

The goal of this spike was to evaluate whether NVIDIA OpenShell can supply high-assurance sandbox isolation mechanics without requiring Meeseek to compromise its semantic sovereignty or inflate baseline operational complexity.

---

## 2. NVIDIA OpenShell Capabilities & Architecture

NVIDIA OpenShell ([NVIDIA/OpenShell](https://github.com/NVIDIA/OpenShell)) provides an agent sandbox runtime with the following core primitives:

1. **Gateway / Control Plane:** Sandbox lifecycle managed via an external daemon/gateway, decoupled from the calling agent process.
2. **Filesystem Isolation via Landlock LSM:** Fine-grained kernel-enforced path restrictions restricting write permissions exclusively to the designated `/workspace`, with root filesystem strictly read-only.
3. **Privilege Hardening:** Non-root execution (`UID 65532:65532`), zero Linux capabilities (`cap-drop=ALL`), and `no_new_privileges` enforced by default.
4. **Network Mediation:** Default-deny egress policy with L7 application-aware proxying for allowed model endpoints.
5. **Warm Pools & GPU Acceleration:** Native support for GPU passthrough and warm container pools to reduce agent cold-start latency.

---

## 3. Provider-Neutral Architecture (`TEBProvider`)

To ensure clean separation between Meeseek policy decisions and runtime mechanics, we introduced the `TEBProvider` abstraction (`internal/teb/provider.go`):

```go
type TEBProvider interface {
    Name() string
    Available(ctx context.Context) bool
    Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error)
    Assess(req ExecutionRequest) (Profile, error)
}
```

### Key Architectural Invariants:
1. **Separation of Policy and Mechanism:** The TEB manager evaluates required guarantees against Meeseek policy. The provider executes the sandbox and returns verifiable evidence.
2. **Deterministic Evidence Hashing:** All executions generate a SHA-256 evidence fingerprint (`ComputeEvidenceHash`) over the provider identity, enforcement level, active guarantees, and sorted runtime evidence items.
3. **Fail-Closed Semantics:** When a task profile requires `ENFORCED`, provider unavailability or misconfiguration **never** silently degrades to `UNENFORCED` or fakes success. The manager strictly returns `ErrProviderUnavailable` or aborts execution unless a qualified fallback provider is registered and available.
4. **Authority Boundary Isolation:** The sandbox output is verified to ensure sandbox processes cannot spoof or inject sovereign authority tokens (`ErrAuthorityTampering`).

---

## 4. Adversarial Bypass Verification

Automated adversarial tests (`internal/teb/openshell_test.go` and `internal/teb/provider_test.go`) verified 4 key attack vectors:

| Attack Vector | Simulated Exploitation | OpenShell Mitigation | Test Result |
|---|---|---|---|
| **Workspace Escape** | Attempt to write to `/etc/passwd` or outside `/workspace` | Landlock LSM blocks writes outside `/workspace` with `EPERM` | **PASS** (Blocked) |
| **Host Secret Leakage** | Mounting root `/` or symlinking `/var/run/docker.sock` / host secrets | Workspace pre-validation rejects dangerous paths before execution | **PASS** (Blocked) |
| **Network Egress Bypass** | Attempting direct TCP dial to unauthorized external IPs | Network isolation mode `none` or L7 proxy block | **PASS** (Blocked) |
| **Privilege Escalation** | `setuid(0)` or raw socket creation | Zero effective capabilities (`CapEff: 0`) and `NoNewPrivs: 1` | **PASS** (Blocked) |

---

## 5. Lifecycle & Operational Complexity Accounting

| Dimension | Baseline OCI TEB (Docker / Podman) | NVIDIA OpenShell Backend |
|---|---|---|
| **Dependencies** | Zero external daemons beyond standard container runtime | Requires OpenShell gateway daemon / cluster infrastructure |
| **Kernel Requirements** | Standard Linux kernel | Requires Linux kernel >= 5.13 with Landlock enabled |
| **Developer Ergonomics** | Runs locally out-of-the-box on macOS / Linux / dev containers | Requires OpenShell setup or remote gateway endpoint |
| **Cold Start Latency** | ~1–2 s container start | Sub-second with warm sandbox pooling |
| **Egress Security** | Port/host CONNECT proxy | Native L7-aware policy enforcement |
| **GPU / Acceleration** | Manual OCI flag configuration | First-class native GPU sandbox support |

---

## 6. Recommendation: ADAPT

### Decision: **ADAPT**
- **Do NOT Adopt as Mandatory Hard Dependency (REJECT full adoption):**
  Mandating OpenShell would break local-first single-Cube developer workflows and complicate CI pipelines due to kernel Landlock and daemon prerequisites.
- **ADAPT the Architecture:**
  1. Standardize on the provider-neutral `TEBProvider` interface introduced in `internal/teb/provider.go`.
  2. Retain the existing `docker-oci` backend (`internal/teb/docker_provider.go`) as the default, zero-dependency local baseline.
  3. Support `nvidia-openshell` (`internal/teb/openshell.go`) as an optional high-assurance provider for enterprise deployments, multi-tenant nodes, and GPU-intensive agentic workloads.
  4. Ensure fail-closed fallback: if OpenShell is configured but unreachable, execution falls back to local OCI only if permitted by policy, preserving full evidence lineage.
