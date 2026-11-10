# Safety controls and vision alignment

Owner goal: Summa42 should become something the Owner can run without fear as
an agentic company operating system, with routine work delegated and the Owner
remaining accountable but not operating it day to day. A full repository review
on 2026-11-10 found that the safety controls this trust depends on were either
unreachable or ineffective, that a large share of recently merged code is not
executed by any production path, and that `docs/VISION.md` describes the
kernel's limits but not the destination. Recommended decisions were selected
per AGENTS.md; four were confirmed with the Owner (see Decisions).

## Problems

1. **Dead man switch.** The heartbeat timeout was derived from the lease
   duration (ten leases, 50 minutes by default) and rewritten by every process
   that opened the state. Nothing in the CLI or control API recorded a
   heartbeat. Once the timeout passed, every `Prepare` and `Dispatch` of an
   external operation failed and stayed failed across restarts.
2. **Break glass.** No command or endpoint triggered or lifted it. When
   triggered from code it blocked external operations and capability sessions
   but not Attempt leasing, and it revoked leases with direct SQL that left no
   execution event.
3. **Issue revisions.** An issue's revision is its `updated_at`. Any comment by
   an account other than the configured bot moved it, including comments by
   accounts that are not maintainers.
4. **Code not on any production path.** `mission`, `decision`, `dtu`,
   `gateway`, `epistemic`, `replay`, `delegation`, the inter-Collective
   evidence lineage, the OpenShell TEB adapter and the MAF executor adapter are
   imported by nothing that runs. Several evaluate fixed strings or
   self-reported flags, so their passing tests do not establish the property
   their names suggest. Their epics were closed as delivered.
5. **Company OS packages.** `attention`, `obligation`, `company`, `invariant`
   and `board` are constructed or present but never called. The obligation
   planner was constructed with a nil scheduler. A migration seeded a shared
   resource envelope with a limit of 100,000,000 units for synthesized
   obligation Tasks.
6. **Vision.** `docs/VISION.md` has no destination, no article on standing
   delegation, on continuity, or on Owner absence. Its "Summand" section was
   added by pull requests #21 and #23, produced by the pipeline during the
   period covered by `docs/incidents/2026-10-21-canned-model-reviews.md`; it
   calls itself normative, states an unimplemented mechanism as fact, and
   specifies an upgrade procedure that belongs in the design specification.

## Chosen scope

### Emergency controls the Owner can reach

Add `summa42 emergency status|freeze|resume|heartbeat` to the Owner CLI. Like
`summa42 policy upgrade`, these write canonical state directly: an emergency
control must not depend on the Box it stops, and the `run-*` workers run
without the control daemon. Each mutating command is signed with the Owner key.
`emergency.Service.ApplyOwnerCommand` verifies the Ed25519 signature against the
Owner principal's stored public key, requires the command to be recent (five
minutes), and records the signature in the immutable emergency audit event; the
event ID is the nonce, so a replayed command fails on the primary key.

Alternatives considered: control API endpoints (rejected: unavailable when only
workers run, and a hung Box could not be frozen) and unsigned `summa42-box`
subcommands like `block-cases` (rejected for `resume` and `heartbeat`: both
restore authority, and an UNENFORCED executor running as the Box user could
call them; the model and validation users cannot read the Owner key).

### Dead man switch as an Owner-set horizon

The timeout is an Owner setting, not a function of leases. Default: seven days.
`EnsureDefaults` inserts the row once and never rewrites an existing timeout.
`summa42 emergency heartbeat [--timeout DURATION]` records presence and may set
a new horizon. A migration replaces previously derived timeouts with the
default, clears a safe mode that the derived value triggered and treats the
upgrade as a heartbeat.

Safe mode means dormancy with reconciliation: no new Attempt leases and no new
external operations; recovery of expired leases and read-only reconciliation of
in-flight operations continue.

### Break glass that stops work

`execution.Service` gains a lease gate that is checked inside the `StartAttempt`
transaction, so a freeze and a lease cannot interleave. `emergency.Service`
implements it: FROZEN or safe mode refuses the lease. Break glass revokes
active leases through `execution.RevokeActiveLeasesTx`, which applies the same
state changes as `RevokeLease` and appends a `LEASE_REVOKED` event with the
reason for every Attempt. Attempt containment revokes its lease the same way.

### Issue revisions follow maintainers

The intake client receives the maintainer allowlist. A comment is trusted only
when a maintainer who is not the bot wrote it. When an issue's latest update is
exactly an untrusted comment, the observer keeps the latest revision it has
already recorded for that issue, or the last trusted comment's time when that
is newer. An issue seen for the first time keeps its `updated_at`.

Limit (unchanged in kind from the existing bot rule): a maintainer's body or
label edit made after their last comment and followed by an untrusted comment
within one poll interval is not seen until the next maintainer activity.

### Parked spikes

Remove the packages in Problem 4 from `main`. They remain in history;
`docs/superpowers/research/2026-11-10-parked-spikes.md` lists, for each, the
issue, the pull request, the commits and the exact command that restores it.
Applied migrations are not removed; their tables stay unused. The research
notes for the spikes stay.

Keep the Company OS packages and `runmanifest`'s projection compiler, marked in
the README as libraries that no running path calls. Fix what is wrong in them
regardless of wiring: construct the obligation planner after the scheduler,
and set the seeded default envelope's limit to zero so a synthesized Task
cannot spend until the Owner funds it.

### Vision and documentation

- `docs/VISION.md`: add "Where this leads" (Owner and operator are different
  roles; the autonomy ladder A0 to A3; Owner attention as the scarce resource);
  add articles 27 to 29 (standing delegation, continuity, Owner absence); add a
  seventh question to the standard of progress; reduce the Summand section to
  lifecycle, projection intent, least cognitive mechanism and taxonomy, marked
  as direction rather than normative text.
- `docs/COMPANY_OS.md`: the optional profile from issue #18, with a software
  product company as the first target and client services as a later one.
- `README.md`: emergency commands and the Owner horizon; a table of what runs
  and what is library only; the validation build cache statement that
  contradicted itself.
- Roadmap and incident record: a dated status note each.

## Out of scope

Monetary budgets, notifications, backup, a process watchdog, merge and CI
observation (roadmap stages 6 and 7), wiring Company OS into the scheduler,
quarantine and containment-clearance commands, Constructors (discussion #136),
required status checks on `main` (a repository setting only the Owner can
change). Each is listed in the README or the roadmap note as open.

## Decisions

Confirmed with the Owner on 2026-11-10:

- Parked code must stay findable: index by commit with restore commands.
- Default Owner horizon: seven days.
- Destination: any company eventually; a software product / SaaS first,
  client services later.
- Reopen the affected epics, comment on PR #176, open a draft pull request.

Selected under AGENTS.md: direct-state signed CLI rather than control API;
safe mode blocks leasing; default envelope set to zero rather than removed.

A claim in the review that an expired lease is reported as a rogue signal was
wrong: only a session with `revoked_at` set is reported. No change is made.

## Verification

Tests written first for each behavior: dead man horizon survives reopen and is
not rewritten; safe mode after the horizon and recovery by signed heartbeat;
signature, freshness and replay rejection; freeze refuses `StartAttempt` and
records `LEASE_REVOKED`; resume restores leasing; untrusted comments keep the
recorded revision, maintainer comments move it; planner wired; envelope limit.
Then the validation matrix from CONTRIBUTING.md.
