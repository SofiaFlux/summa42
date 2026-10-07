# Executor eligibility and truthful capacity

Owner approved the self-development roadmap on 2026-10-07. This first slice prevents a registered but unrelated executor from leasing work, preserves exact TaskClass routing, and stops unassessed executors being advertised as ENFORCED.

An executor may describe its supported semantic capabilities, task classes and actual enforcement. Absence of a description offers only its registry-kind capability. Production composition treats unassessed enforcement as UNENFORCED. Capability eligibility must be satisfied by one executor, not by the union of unrelated executors. Preferred/learned selection is applied only after this qualification.

Existing trusted callers that supply legacy CapacitySnapshot capability maps retain registry-kind matching. The CLI supplies descriptors from real registered executors. Caller descriptors can narrow actual contracts, never expand them. A TaskClass mapping is mandatory when selection is attempted; a missing or blank required executor returns an actionable error and never falls through to alphabetical selection. Tasks lacking any eligible executor remain unclaimed; an idle step is not a diagnosis of the whole backlog.

The worker filters registry membership and capability availability before candidate selection; no eligible executor means no lease. A frozen descriptor/capacity snapshot is used for each step. This does not grant authority, raise task budgets or accept completed Tasks.

Native model subprocess containment is reported honestly. Deterministic protected publishers retain their existing enforcement claim. No change to task RequiredEnforcement is made to bypass a missing assessment.

Acceptance: unrelated alphabetical executor is never called; partial/unknown enforcement is not promoted; capabilities split across two executors cannot qualify one Attempt; unavailable mapped kind never falls back; existing one-executor legacy callers still work.

Usage accounting: persist nonempty observed usage as JSON evidence linked to the Attempt, and include it in the completion/failure manifest. Usage alone cannot turn an evidence-free execution into completion. `Reported` records the presence of a provider usage object, not verified billing completeness. Monetary reservation and settlement remain separate roadmap work.

ADO compatibility: a publication successor requires only its proposed effect capabilities, not the previous model executor's capabilities. Every publisher mode handles comment and approve decisions; `none` records without dispatch, and `comments` skips votes. Authority, policy and approvals still gate actual dispatch.
