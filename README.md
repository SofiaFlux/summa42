# Summa42

Summa42 is an experimental local-first agent orchestration runtime. The project distinguishes the runtime (Summa42 / Cube / Box) from the **Summand** (the task-scoped cognitive worker). The current **Minimal Viable Collective (MVC)** is intentionally one Cube: one trusted Box daemon owns canonical state, policy, authority checks, scheduling semantics, external-operation commit boundaries, verification, memory, audit and resource accounting.

Read [the Vision](docs/VISION.md) for the durable Collective this MVC is meant
to validate, and the [design specification](docs/superpowers/specs/2026-09-14-summa42-design.md)
for the detailed technical model.

> **Experimental status:** Summa42 is local-first, single-Cube software under active development. It is not production-ready and must not be used for unmanaged consequential workloads. Run it only where you can review its authority, policy, evidence, and containment boundaries.

## Prerequisites

- Go 1.27
- Linux and Docker only when running the optional OCI enforcement gate

## Build and start locally

Build the two binaries explicitly:

```bash
mkdir -p ./bin
go build -o ./bin/summa42 ./cmd/summa42
go build -o ./bin/summa42-box ./cmd/summa42-box
```

Initialize the local Collective once, start the Box in one terminal, then query it from another:

```bash
./bin/summa42 init
./bin/summa42-box
./bin/summa42 status
./bin/summa42 mission create "Maintain this repository safely"
./bin/summa42 mission show
```

| Command | Purpose |
| --- | --- |
| `./bin/summa42 init` | Create the local Collective home and initial state. |
| `./bin/summa42-box` | Start the single local Box daemon. |
| `./bin/summa42 status` | Query the local Box status. |
| `./bin/summa42 mission create "..."` | Set the first active Mission with an Owner signature; prints its ID for Mission-rooted Tasks. |
| `./bin/summa42 mission show` | Show the active Mission and its ID. |
| `go test ./... -count=1` | Run the normal Go test suite. |

`summa42 init` creates `~/.summa42` by default, including the local SQLite database, evidence directory, local-development Owner/Cube keys and a `config.json` file with mode `0600`. The config contains a randomly generated 256-bit bearer token used only for the local control transport. Set `SUMMA42_HOME` to select another Collective home. `SUMMA42_CONTROL_ENDPOINT` and `SUMMA42_CONTROL_TOKEN` can override the automatically resolved endpoint/token.

The Box never loads the Owner private key during normal startup. The CLI uses it only for explicit Owner actions such as approvals and Mission creation. Only one Mission can be active; replacing or adjusting one is not yet exposed by the MVC CLI.

### Optional Azure DevOps MCP reads

The Box has **no ambient Copilot or MCP discovery**. To enable its narrowly scoped Azure DevOps read provider, install and authenticate Microsoft's local Azure DevOps MCP server separately, then set both `SUMMA42_ADO_MCP_COMMAND` (path to an executable that starts that server with the organization as its sole argument) and `SUMMA42_ADO_ORGANIZATION` before starting `summa42-box`. A small wrapper executable can be used if the installation needs fixed flags. Do not put credentials in the repository or `config.json`.

At startup, the Box checks the configured server for `core_list_projects` and `wit_work_item` and records durable capability assessments. It advertises eight read-only capabilities (`ado.projects.list`, `ado.work_item.read`, `ado.pr.list`, `ado.pr.get`, `ado.pr.org_active`, `ado.pr.threads`, `ado.pr.file`, `ado.build.status`), each with an action and parameter allowlist; `ado.work_item.read` accepts only documented read actions (`get`, `get_batch`, `list_comments`, `my`, `list_revisions`, `list_for_iteration`, `get_type`). A Task that needs one must declare that semantic name in both `RequiredCapabilities` and `AuthorityCeiling`, with a compatible enforcement level. A discovered server tool never grants Task authority by itself. This local-process integration is rated `PARTIAL`, not `ENFORCED`. ADO review votes are bound to the reviewer (`run-driver --reviewer <ado-reviewer-id>`) and to the reviewed commit.

This is a **capability primitive**, not automatic Mission execution. The default Box daemon serves local control; `summa42-box run-worker --workspace-root <directory>` explicitly starts the scheduler/executor worker with configured adapters. A Task declaration alone will not call ADO or hand an MCP session to Codex. External writes remain outside this provider and must use the protected External Operation path.

Worker capacity comes from executor contracts: one executor must support the Task class, every required capability and the required enforcement level. A registry name does not invent semantic capabilities. Model-based GitHub issue triage is currently `UNENFORCED`; the native Copilot adapter is `PARTIAL`. Tasks demanding stronger containment remain unclaimed. An explicit Task-class route never falls back to an unrelated executor.

Reported executor usage is saved as `executor.usage.v1` evidence linked to its Attempt, including on execution failure. Missing token reports remain unknown; usage evidence does not settle monetary budgets or substitute for execution evidence.

The [self-development roadmap](docs/superpowers/plans/2026-10-07-self-development-roadmap.md) records the audited baseline and remaining stages. The [executor eligibility design](docs/superpowers/specs/2026-10-07-executor-eligibility-design.md) describes the first foundation slice. [Grounded planning](docs/superpowers/specs/2026-10-07-grounded-planning-design.md) adds pinned source preparation and independent plan review; supervised local implementation, independent candidate review and protected draft-PR publication are described below. There is no merge stage: the Owner merges.

### Grounded GitHub issue plans

`run-gh-plan` without source flags keeps the v1 planning recipe. To create a v2 plan, supply an absolute local Git repository, its GitHub identity, a full commit SHA, exact context files and allowed paths, and validation commands as JSON argument arrays. For example, after configuring the existing model flags and mission:

```bash
./bin/summa42-box run-gh-plan --mission <mission-id> \
  --model-binary <planner-wrapper> \
  --source-repo /absolute/path/to/summa42 --source-repository SofiaFlux/summa42 \
  --source-sha <full-commit-sha> --context-file internal/version/version.go \
  --allowed-path internal/version/version.go --allowed-path internal/version/version_test.go \
  --validation-command-json '["go","test","./internal/version","-count=1"]'
./bin/summa42-box run-gh-plan-review --mission <mission-id> --model-binary <reviewer-wrapper>
```

These are supervised one-shot commands using the existing CLI model protocol. The reviewer gets the original issue, exact plan and hash-bound code projection through a separate invocation. It materializes a review Task and claims a fenced Attempt before calling the model, preventing overlapping ticks from paying for the same review twice. Its five-minute lease bounds the call to four minutes, leaving time for cleanup. A completed verdict resumes from its exact Task/Attempt manifest after interruption; normal runtime wake processing handles expired claims. ACCEPT can propose `github.issue.implement` only if the existing case grant permits both `workspace.repo.write` and `workspace.test` capabilities/actions. REVISE/BLOCK holds the case; a legacy v1 plan must be re-planned before implementation. Review does not execute the validation commands or materialize an implementation Task. Native model invocations remain UNENFORCED; monetary accounting and worker-based model composition remain future work.

`internal/repoworkspace.Checkout` is the pinned, detached source-preparation primitive for supervised local implementation; the current general worker still creates an empty attempt workspace. Selected context files must be regular tracked UTF-8 files; symlinks, submodules, path traversal and context exceeding 32 files or 256 KiB are rejected. Allowed paths are exact file names, including explicitly permitted new files.

Source preparation sets `GIT_NO_LAZY_FETCH=1`, so missing partial-clone objects fail offline instead of being fetched; a recent Git (2.45 or newer) is not required. Checkout copies and dissociates object storage; it does not borrow writable objects from the source. These Git controls prepare data and do not provide an execution sandbox for future tests or implementation tools.

### Supervised local implementation

After an authorized ACCEPT from `run-gh-plan-review`, `run-gh-implement` can prepare an actual local candidate:

```sh
mkdir -p /absolute/path/to/attempts
./bin/summa42-box run-gh-implement --case <case-id> \
  --source-repo /absolute/path/to/summa42 \
  --workspace-root /absolute/path/to/attempts --model-binary <implementation-wrapper>
```

The wrapper follows the existing stdin/stdout JSON protocol and returns `{"summary":"…","edits":[{"path":"file.go","content":"complete UTF-8 contents","delete":false}]}`. Models propose complete-file edits; deterministic code enforces the owner-selected paths, prepares a fresh pinned checkout and creates a local commit. Each claimed Task/Attempt records the exact plan/review/source citations, candidate/tree/diff identity and real results of owner-supplied validation argv. Concurrent calls return busy; completed calls return the same saved evidence. Execution results are indexed before completion, so a failed completion save resumes under the same live fence without another model/test call; an expired staged run requires operator recovery. The source repository’s observed Git/data state must remain unchanged during execution. Validation failure stays visible in the record and CLI exit status. Task completion remains awaiting independent code verification.

Validation commands run natively as trusted local commands, classified UNENFORCED. They receive a private HOME, GOCACHE and GOMODCACHE and no ambient credential variables. Go module downloads are disabled (`GOPROXY=off`), and the private module cache starts empty; for dependencies, use an explicit owner-selected wrapper/argv that supplies a module cache, for example `["env","GOMODCACHE=/absolute/prepopulated/module-cache","go","test","./..."]`. Each command has a fixed 60-second timeout and 16 KiB captured output. Validation cannot silently change the committed candidate. This slice does not accept code or publish; publication is a separate step. Native execution does not contain hostile repository code; use a trusted local repository and command configuration until TEB integration is available.

### Independent local code review

After `run-gh-implement` records a candidate, run a separately configured reviewer:

```sh
./bin/summa42-box run-gh-code-review --case <case-id> \
  --source-repo /absolute/path/to/repository --model-binary <review-wrapper>
```

The wrapper receives the original issue, exact accepted plan/context, implementation evidence and complete before/after contents and modes for every changed file. It returns only `{"verdict":"ACCEPT","reason":"…"}` (or `REVISE` / `BLOCK`). Projection is limited to 64 changed paths and 512 KiB total file contents; oversized or non-text changes are rejected rather than truncated. Configure a reviewer independently from the implementation wrapper; the system provides a separate invocation and Task/Attempt, not proof of different provider identity.

A claimed authority-free review Task shares the original Mission/resource envelope. Its five-minute lease permits a four-minute model call. Verdict evidence binds the exact Case/Work, implementation evidence/hash and candidate, and review Task/Attempt/fence. Interrupted completion/acceptance resumes the saved result; an expired staged review requires operator recovery. Completed reviews are reused, and tests are not rerun. Source/candidate drift or a changed issue revision/grant blocks completion/acceptance.

Only `ACCEPT` together with all passing recorded owner validations accepts the implementation Task. `REVISE` / `BLOCK` or failed validation returns a held JSON result and a nonzero CLI status, keeping implementation awaiting verification. The Case stays on the implementation Work, so a future publication stage must explicitly consume its exact acceptance. No automatic reimplementation or publication is performed by this command. Native wrappers remain trusted and UNENFORCED; drift checks do not provide an atomic filesystem lock or a hostile-host sandbox.

### Supervised candidate publication

After the independent code review accepts the passing candidate, an owner may explicitly grant both `github.repo.publish` and `github.pr.create` capabilities **and actions** on the current Case, then invoke:

```sh
./bin/summa42-box run-gh-publish --case <case-id> \
  --source-repo /absolute/path/to/source --base-branch main \
  --repository owner/repo --credential-file /absolute/path/to/private-token
```

The token file must be a regular private file (0600 on Unix). Repository selection must match the Case and accepted plan. Publication runs through policy, approvals and resource reservations, using separate protected branch and PR effect slots. Costs are bounded GitHub write-request units (three for branch objects/ref, one for the PR), not a currency charge. No approval is fabricated by this command.

The managed branch is `summa42/issue-<number>-<work-hash>`. The publisher exports at most 64 regular UTF-8 files and 256 KiB of new contents, preserving executable modes and deletions. Only ordinary unsigned single-parent commits are supported. GitHub must return the exact reviewed tree and commit SHA before a create-only branch ref can be written; a conflicting branch is held. The base branch must remain at the accepted base SHA. A deterministic marker binds the draft PR to the exact head, base, repository and evidence.

Policy-required approval produces `held: true` with the exact operation and approval IDs and a nonzero exit. Approve that bound request within the live ten-minute lease, then rerun the command; it resumes the same Attempt and prepared operation. Branch and PR may require separate approvals. Concurrent dispatch claims still permit one write.

A lost write acknowledgement yields `pending: true` and a nonzero exit. Outcome lookup is read-only; an absent branch/PR does not prove that a delayed write cannot appear. Repeated calls retain the live claim (`busy: true`); after lease recovery, protected slots reconcile uncertain effects without blindly duplicating them. Confirmed effects are staged as an immutable receipt before completion. Interrupted completion can resume under the same live fence; expired staged receipts need operator recovery. Completed replay returns the historical receipt under current authority, rather than asserting ongoing remote PR health.

The publication Task stays `AWAITING_VERIFICATION` and the case moves to `READY_FOR_VERIFICATION`. There is no merge stage: the Owner reviews and merges the draft PR. The adapters mediate their own GitHub effects as ENFORCED; they do not sandbox hostile repository execution. No live Summa publication was used to validate this slice.

### Automated GitHub pipeline

`run-gh-intake` observes issues and creates cases; it stays a separate command. `--once` runs a single observe pass; otherwise it polls. `--grant-capability` and `--grant-action` define what each case is granted (each `--grant-action` must also be a `--grant-capability`, and `github.issue.read` is required). `--bot-login` names the GitHub App login whose comments must not create new issue revisions. Prefer `--credential-file` over the deprecated `SUMMA42_GITHUB_TOKEN_FILE`.

`run-gh-pipeline` replaces the former reconciler. It advances the mission's cases through triage, plan, plan review, implementation, code review and draft-PR publication, either once (`--once`) or repeatedly (`--interval`):

```bash
./bin/summa42-box run-gh-pipeline --mission <mission-id> --interval 1m \
  --model-binary <producer-wrapper> --review-model-binary <reviewer-wrapper> \
  --source-repo /absolute/path/to/summa42 --source-repository owner/repo \
  --source-sha <full-commit-sha> --workspace-root /absolute/path/to/attempts \
  --allowed-prefix internal/ --validation-command-json '["go","test","./...","-count=1"]' \
  --repository owner/repo --base-branch main --credential-file /absolute/path/to/private-token
```

- The producer (`--model-*`) and reviewer (`--review-model-*`) are configured separately.
- `--allowed-prefix DIR/` lets the model select files per issue within a directory; it excludes `--allowed-path`, which fixes exact files for every issue. `--context-file` supplies exact context.
- **Model user (required).** Model processes never run as the Box owner. Every command that starts a model (`run-gh-pipeline`, `run-worker`, the `run-gh-*` / `run-repo-*` stage commands, and the review model) needs `--model-user <user>` or `SUMMA42_MODEL_USER`, naming a dedicated OS user. Construction fails closed when it is unset, root or the Box's own user; there is no same-user escape flag. The model is launched as `sudo -n -u <user> -- /usr/bin/env -i HOME=<that user's home> PATH=… LANG=… TMPDIR=<home>/tmp <allowlisted vars> <binary> <args>`, in the model user's home, so it can read neither the Box's environment nor its files: `~/.summa42/keys/*`, `github_token`, `gh-app.pem`, `discord_auth.json`, the state DB and `~/.config/gh`. Run `sudo scripts/setup-model-user.sh` once to create the `summa42-model` user (no login shell, no groups), install a validated `/etc/sudoers.d/summa42-model` drop-in, chmod `~/.summa42` and `~/.config/gh` to 0700 and verify that the model user cannot read the secrets (it exits non-zero if it can). Then log each model CLI in as that user, e.g. `sudo -u summa42-model -H codex login`; model binaries must be executable by it. `scripts/summa42-acp.js` chat spawns `agy`/`ollama` the same way and refuses to start without `SUMMA42_MODEL_USER`.
- Model processes get a minimal environment (PATH, LANG, the model user's own HOME and TMPDIR plus the names you list with `--model-env` / `--review-model-env`); GitHub and `SUMMA42_` credentials are rejected as variables and, because the model runs as another user, the Box's credential files are unreadable to it. Allowlisted values appear on the `env` command line, so list only variables meant for the model. Prompts are sent on stdin. The optional router `scripts/call-worker.py` fails closed (non-zero exit, empty stdout) and uses OpenRouter only when opted in (`--allow-remote` or `SUMMA42_ALLOW_REMOTE_MODELS=1`); it runs as the model command, so its `~` paths are the model user's.
- The pipeline ends at a published **draft PR**; the case moves to `READY_FOR_VERIFICATION` and the Owner merges. No merge or issue-closure stage exists.
- Each tick first runs recovery (lease handling and external-operation reconciliation), skipping foreign and in-flight operations. Policy-required approvals still hold the case until the Owner approves.

Every `run-gh-*` command also has a `run-repo-*` alias (for example `run-repo-pipeline`, `run-repo-intake`).

### Operational notes

- Several `summa42-box` processes may share one local SQLite database (`_txlock=immediate`); recovery runs at worker start and on every pipeline tick.
- Validation uses private Go caches (see above) and a fixed 60-second timeout.
- Git source preparation uses `GIT_NO_LAZY_FETCH=1`.
- Supervised publication always creates **draft** pull requests. Nothing in Summa42 marks them ready or merges them.
- Native execution (including model wrappers) is UNENFORCED. The OCI TEB egress/containment and the MCP adapter/capability Session are primitives that are not wired into execution.

## What is authoritative

SQLite is the sole MVC source of truth and must live on a **local filesystem**. It runs in WAL mode with `synchronous=FULL`, foreign keys enabled and a busy timeout. The database is not supported on NFS/SMB or as shared multi-Cube state. The Box and the `run-*` subcommands may run concurrently as separate processes on one local DB; correctness comes from guarded `BEGIN IMMEDIATE` transactions, not process exclusivity.

Canonical semantics distinguish:

- Collective, Cube, Box, and Summand (the task-scoped cognitive worker);
- Task from Attempt;
- Attempt completion from independent Task acceptance;
- Summand cognition from deterministic scripts/workers and interchangeable executors;
- compiled task context (Context Projection) from ambient model context windows;
- reusable Skills from active Capabilities and Executors;
- Summand proposals/evidence from canonical Collective truth;
- PREPARED external operations from the DISPATCHED commitment boundary;
- durable effect-slot identity from mutable intent fingerprints;
- settled cost from unresolved exposure;
- operational telemetry from the canonical audit trail.

The Box rejects stale/revoked/expired Attempts even when their fence value still matches. Unknown external outcomes are reconciled rather than blindly retried, and unresolved exposure continues to consume budget.

## Policy and authority

Bootstrap installs one active, persisted OPA policy profile (policy v2). Its safe capability profile has no network access and excludes `http.send`. The default policy is deliberately conservative: only LOW-risk consequential actions with valid authority are allowed; everything else is denied. Risk is derived from the provider: Branch, PR and ADO-vote operations are MEDIUM and require Owner approval. Existing Collectives adopt the shipped policy with `summa42 policy upgrade` (Owner-signed; it refuses while operations are in flight). Policy compile/evaluation errors and provenance mismatches fail closed.

The local control API verifies Owner approval signatures with the public key stored in canonical state. Durable approval requests are persisted and exact-bound to their subject and digest. Multi-principal approvals use AND semantics, and current policy/authority is rechecked immediately before consequential dispatch.

`summa42 approvals` and `summa42 approve` show the provider, descriptor type, effect slot, fingerprint, revision and canonical intent that the signed digest covers (caller attributes are shown as informational and are not signed); `approve` recomputes the digest from the displayed fields and refuses on mismatch. `approve` requires typing `yes` on a TTY; `--yes` is accepted only when stdin is not a TTY and `SUMMA42_APPROVE_NONINTERACTIVE=1`. Limits to know:

- With `LOCAL_DEV_FILE` custody the Owner key and the control token live in `~/.summa42`, so approval is not a boundary against UNENFORCED executors running as the same OS user: such an executor can approve its own request. Model processes are the exception: they run as the separate model user and cannot read `~/.summa42`. Validation commands and other native executors still run as the Box user.
- Only the Owner can approve. Requests that require any other approver are unsupported, so the AND semantics can only ever be satisfied by the Owner's single signature.
- An approval expires with its lease; a new lease needs a new approval, and a rejection is not sticky per slot.

## Enforcement levels

Every execution path must describe what it can actually enforce:

- **ENFORCED** — the declared security guarantees are technically enforced. The strong reference profile is Linux/OCI with read-only root, non-root execution, dropped capabilities, no-new-privileges, process/memory/CPU limits, network isolation, workspace-only writes, no ambient credentials and no container socket.
- **PARTIAL** — some controls are technically enforced but the full strong profile is not proven.
- **UNENFORCED** — orchestration semantics still apply, but containment is not claimed.

The native local Box defaults to `UNENFORCED` containment. The Codex adapter defaults to `PARTIAL` and may advertise `ENFORCED` only when its model-egress assessment proves the required mediated egress guarantees.

The CONNECT egress proxy accepts exact allowlisted hosts. For DNS names it checks every returned address against non-public ranges, then dials a validated IP directly so DNS cannot change the target between the check and connection. A trusted private endpoint must be allowlisted by its exact IP literal.

## Attempt run provenance

Every Attempt leased through the Box runtime gets one immutable, hash-bound Run Manifest in the same SQLite transaction that creates the lease. The manifest is descriptive only: copying it does not recreate authority, extend a lease, mint a capability session or change canonical Task/Attempt state.

For the MVC it snapshots only facts that are actually known at lease time: Task/Attempt identity and fence, executor kind, runtime build metadata when available, policy-set provenance when exposed by the policy engine, the effective TEB profile, the Task's `RequiredCapabilities`, which the current MVC treats as the declared executor-visible capability set at lease time, with any durable capability assessment metadata, and the resource envelope. There is no Context Projection subsystem yet, so no projection hash is invented.

Later output Evidence and settled External Operation cost remain canonical in their existing stores. `runmanifest.Provenance` joins those records for inspection without mutating the immutable lease-time manifest. Executor usage/model/version fields remain empty unless a future canonical source can supply them; the MVC does not fabricate zero/default measurements.

## Field dogfooding and local experience

Field dogfooding is **disabled and LOCAL_ONLY by default**. Raw `FieldObservation` records and candidate free text may contain workload-sensitive context and remain local. The MVC deterministic sanitizer exports a structural allowlist only (closed categories/classes/state tokens, numeric metrics, human-intervention and enforcement flags); free-form behavior text, customer/repository/project identifiers, correlation hashes, and caller-controlled runtime/executor metadata are not serialized into outbound feedback. Only a deterministic, fail-closed sanitizer can produce an immutable `SanitizedFeedback` export artifact; sanitization and authorization are separate gates.

Outbound GitHub feedback is normal governed work: a `COLLECTIVE_MAINTENANCE` Task is leased to the deterministic `feedback-emitter`, which uses the protected External Operation PREPARED → DISPATCHED → reconciliation boundary. It never sends directly from the CLI. GitHub credentials are not stored in `config.json`; pass `run-worker --credential-file` a private regular file (0600 on Unix; the deprecated `SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE` is still read) only when GitHub export is explicitly enabled. When GitHub feedback export is on, set `SUMMA42_FEEDBACK_GITHUB_BOT_LOGIN` to the bot account that creates feedback issues so reconciliation only trusts markers it authored; when unset, any author is accepted and a warning is logged once. `SUMMA42_GITHUB_TOKEN_FILE` for `run-gh-intake` is likewise deprecated in favour of `--credential-file`.

Local process adaptation requires an explicit, durable, Owner-approved `AdaptationGrant`. Experience may reorder already-eligible executors only; it cannot expand capability, authority, egress, enforcement, policy, approval requirements, or resource ceilings. 

This feature does **not** implement source-code self-modification or the future Maintainer Collective. Evidence may create new work; discovery does not authorize a change.

### ACP Discord bridge

`scripts/summa42-acp.js` is a read-only, owner-gated bridge: it offers status, cases, tickets and owner-only chat and `/mission show`, but no `/issue`, reconcile or mission-set commands. It requires `SUMMA42_HOME` and `$SUMMA42_HOME/discord_auth.json` (mode `0600`) containing a `discord_owners` allowlist. The allowlist is an ID check, not cryptographic authentication.

## Current MVC scope

Implemented now: single-Cube local runtime composition, SQLite persistence, Ed25519 principals, Constitution bootstrap, embedded restricted OPA, Task/Attempt fencing and leases, immutable Attempt Run Manifests, evidence and independent verification, resource ledger, protected external-operation lifecycle/reconciliation, durable approvals, capability registry primitives, deterministic scheduler/wake semantics, OCI TEB primitives (not wired into execution), deterministic and Codex executor adapters, field observations/sanitized governed feedback, evidence-driven local experience, memory/audit, an MCP adapter and capability Session primitive (not wired into execution), and authenticated local control/CLI.

Deferred by design: multi-Cube coordination, PostgreSQL, NATS, SPIFFE/SPIRE, Graphiti projection, distributed scheduling, Kubernetes and other infrastructure that is unnecessary for the one-Cube MVC.

## Verification

The normal verification matrix is:

```bash
go test ./... -count=1
go test -race ./... -count=1 -timeout 45m
go vet ./...
go build ./cmd/summa42 ./cmd/summa42-box
python3 docs/superpowers/research/spikes/2026-09-14-sqlite-persistence-spike.py
```

The Linux OCI gate additionally runs the bypass/capability acceptance profile against Docker. CI must not replace that check with a unit-test-only approximation.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for local setup, validation, and pull-request expectations.

## Security

Do not report suspected vulnerabilities in public Issues. See [SECURITY.md](SECURITY.md) for the private reporting route.

## Support

See [SUPPORT.md](SUPPORT.md) for reproducible-defect, security, and conduct-reporting routes.

## License

Summa42 is licensed under the [Apache License 2.0](LICENSE).
