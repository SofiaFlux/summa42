# Company OS profile

> **Status:** direction. No part of this profile runs today. The packages named
> below have schemas and tests, and nothing in a running Box calls them. The
> README's "What runs today" table is the source of truth for what does.

Company OS is an optional profile on top of the Summa42 kernel. It adds the
model of an organization: what the company is, what it owes, what must stay
true, and what the Owner has to decide. The [Vision](VISION.md) describes the
kernel and the destination; this document describes the profile.

One dependency rule holds throughout:

> Company semantics may depend on the kernel. The kernel must not depend on
> company semantics.

A kernel-only deployment stays coherent and useful, for example as a governed
engineering runtime. Nothing below may be required to run a Task.

## First target: a software product company

The first company this profile is written for builds and sells a software
product (SaaS). Services for clients are a later target; any company is the
eventual one. The table lists what such a company does and where each function
stands.

| Function | What the Collective would do | State |
| --- | --- | --- |
| Build | Take a request to a verified, reviewed change | Runs: issue to draft pull request, Owner approves and merges |
| Release | Observe CI, merge under a standing delegation, deploy, canary, roll back | Not built (roadmap stages 6 and 7) |
| Operate | Watch the product, triage incidents, remediate within granted authority | Not built |
| Support | Take customer requests in, triage, keep response commitments | Not built; intake reads GitHub issues opened by maintainers only |
| Money | Bill, follow receivables, keep spending inside budgets in a currency | Not built; the ledger counts provider request units |
| Obligations | Renewals, reporting deadlines, service commitments | Library only (`internal/obligation`) |
| Owner surface | Show what waits for the Owner, with evidence, at a bounded attention cost | Library only (`internal/attention`) |

Services for clients add contracts, deadlines per client and invoicing, and a
boundary to parties outside the Collective. The first version of that boundary
is parked; see
[parked spikes](superpowers/research/2026-11-10-parked-spikes.md).

## Building blocks

| Block | Package | Purpose |
| --- | --- | --- |
| Company State | `internal/company` | Durable facts about the organization: products, customers, contracts, assets, exposure. Changed only by evidence-backed transitions |
| Obligations | `internal/obligation` | Duties with a due time, a lead time and a cost of delay; they synthesize Tasks before they are due |
| Business invariants | `internal/invariant` | Deterministic conditions over Company State; a breach raises a Task, a containment signal or an Owner decision |
| Owner attention | `internal/attention` | A budget for Owner decisions; related decisions are grouped into one packet |
| Board | `internal/board` | One work queue across sources; a card move never causes an external effect |

All five are libraries in the sense of the status note above.

## Rules the profile keeps

- **Tasks are not Company State.** A contract or a customer is a durable fact
  that causes Tasks; it does not exist only as one.
- **Obligations are not dropped for more attractive work.** They are inputs to
  scheduling alongside goals.
- **Synthesized work spends only granted budget.** A Task created from an
  obligation or an invariant uses the default envelope, which ships unfunded.
  It waits until the Owner funds it.
- **Invariants are deterministic.** An instruction in a prompt is not
  enforcement.
- **Other systems stay authoritative.** Billing, CRM, CI and ticketing remain
  the source of truth for their domain. A change to them is an external
  operation behind the commitment boundary.
- **Owner attention is a budget.** The profile asks the Owner only what needs
  the Owner (Vision, Articles 24 and 29).

## Order of work

Each block enters the running system the same way: a caller in a running Box,
an acceptance test over that path, and a row in the README table.

1. Finish the engineering loop on this repository: ten small tasks at A1, then
   standing delegation (A2).
2. Continuity (Vision, Article 28): budgets in a currency, notifications to the
   Owner, tested backup and restore, a watchdog outside the process.
3. Wire obligations into the scheduler with one real obligation, funded by the
   Owner.
4. Owner surface: one command that lists what waits for the Owner and why.
5. Release and operate (A3).
6. Support and money.

Origin: issue #18. Related: #3 (provider strategy), discussion #136
(Constructors, not yet decided).
