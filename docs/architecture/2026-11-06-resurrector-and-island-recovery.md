# Architecture: Resurrector & Genesis — Collective Survival, Watchdogs, and Island Mode Recovery

**Status:** Proposed Architecture  
**Author:** SofiaFlux  
**Category:** Ideas / High Availability  
**Related Discussions:** #136  

---

## 1. Problem & Context
In a multi-node or cloud environment, worker nodes (Cubes) are inherently ephemeral: they can crash, be evicted (Kubernetes spot instances / OOMKilled), or suffer localized network partitioning.

This raises two fundamental failure scenarios:
1. **Total Catastrophe (Death of all Cubes):** What happens if every active Cube process or machine goes down simultaneously? Where does the Collective survive, and how does it restart without human intervention?
2. **Partial Network Partitioning & Island Mode:** What happens if a Cube is severed from the central database while executing a task, continuing to compute in isolation ("island mode")? How do newly revived nodes safely discover lost or running workers without creating split-brain conflicts?

---

## 2. Collective State: Surviving the Death of All Nodes
In Summa42, a **Cube holds no unique or canonical state in volatile memory**. A Cube is purely a stateless compute engine.

The Collective's ground truth lives across two durable tiers:
* **Relational State Store (`internal/state`):** SQLite or managed relational DB (PostgreSQL). Holds the Constitution, registered Principals (cryptographic keys), Task definitions, Constructor profiles, and active leases.
* **Evidence Object Store (`internal/evidence`):** Immutable, hash-addressed blob storage (local disk or Azure Blob Storage / S3).

If every active Cube process is killed (`kill -9` on the entire fleet), **zero state, evidence, or tasks are lost**. The Collective enters a passive dormant state, awaiting a compute worker.

---

## 3. The `Resurrector`: External Genesis Watchdog
To ensure the Collective does not remain dormant forever when all nodes perish, we introduce a fail-safe called **`Resurrector`**:
* **Nature:** A minimal, lightweight external probe (e.g., an Azure Function or local systemd watchdog). Consumes near-zero resources when idle.
* **Role:** Periodically inspects the Collective state. If active missions or eligible tasks exist, but **zero Cubes have refreshed their heartbeat in the database for > 15 minutes**, the Resurrector executes a **Genesis Event**.

### Declarative `Resurrection Contract`
The Resurrector does **not** hardcode machine startup logic. Instead, it reads a signed, declarative contract from the database:

```json
{
  "schema": "collective.resurrection.contract.v1",
  "collective_id": "col_prod_01",
  "heartbeat_threshold_seconds": 600,
  "resurrection_driver": "azure_container_app",
  "driver_payload": {
    "image": "ghcr.io/sofiaflux/summa42-cube:latest",
    "cpu": "4.0",
    "memory": "8Gi",
    "min_replicas": 1
  },
  "on_database_unreachable": {
    "action": "escalate_to_owner",
    "channel": "pagerduty://critical"
  },
  "signer_principal_id": "usr_owner_root",
  "signature": "ed25519:..."
}
```

* **Security Guard:** The contract must be cryptographically signed by the Owner Principal. An untrusted agent cannot alter the image or cloud provisioning parameters.
* **Database Down:** If the database itself is unreachable, the Resurrector does not blindly spawn compute; it immediately dispatches an emergency notification to the human Owner.

---

## 4. Local Node Guardian & "Death of the Watchdog" (`Fail-Stop`)
At the node level, every Cube is supervised by a local process guardian (systemd supervisor or container PID 1):
* **Supervisor Crash = Node Panic:** A running model executor without an active supervisor is an uncontrolled hazard. If the local watchdog crashes or panics, the node enforces a bilateral **fail-stop** (`KillMode=control-group` / PID 1 exit), terminating the container immediately.

---

## 5. Island Mode & Quarantine Policy
When a Cube loses connection to the Collective DB while running a task:
* The task lease eventually expires in the central database (`lease_expires_at`).
* The isolated Cube is forbidden from making consequential external commits (GitHub pushes, PR merges) without an active lease renewal.
* Upon lease expiration, the Cube enters **Quarantine** in one of two configurable modes:

| Quarantine Mode | Target Environment | Behavior |
| :--- | :--- | :--- |
| **Passive Wait** | Edge nodes, developer laptops | Suspends execution, freezes evidence in local buffer, listens on a recovery port. Zero unnecessary CPU/network churn. |
| **Active Recon** | Cloud clusters (k8s / multi-zone) | Uses exponential backoff with jitter to ping the central DB endpoint and query cluster DNS/service discovery for network restoration. |

---

## 6. Genesis Cube: Fleet Reconnaissance & Deep Diagnostics
When the Resurrector or cloud autoscaler boots the first new Cube after a blackout (the **Genesis Cube**), its mandatory first duty is **`collective.cube.reconciliation`**:
1. **Not jumping directly to new tasks:** It first sweeps the database for expired leases that were active when connectivity dropped.
2. **Deep Diagnostic Probe (beyond a simple ping):**
   * If a Cube vanished, it does not assume network glitch.
   * It queries the Kubernetes / Cloud API: `kubectl get pod <cube-id> -o json` to inspect exit codes (`OOMKilled` code 137, spot eviction, node drain).
   * It retrieves the node's **Last Will & Tombstone Logs** (`kubectl logs <cube-id> --previous --tail=100`) and records them as verified evidence (`KindEvidence: cube.post_mortem`).
3. **Adaptive Fleet Learning:** If multiple Cubes recently perished from `OOMKilled` on a specific task type, the Collective adapts resource allocations rather than repeating identical failures.
