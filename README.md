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

These are supervised one-shot commands using the existing CLI model protocol. The reviewer gets the original issue, exact plan and hash-bound code projection through a separate invocation. It materializes a review Task and claims a fenced Attempt before calling the model, preventing overlapping ticks from paying for the same review twice. Its attempt lease is the model timeout plus two minutes (never a fixed ceiling under the model timeout) and the call is bounded to one minute under the lease, leaving time for cleanup. A completed verdict resumes from its exact Task/Attempt manifest after interruption; normal runtime wake processing handles expired claims. ACCEPT can propose `github.issue.implement` only if the existing case grant permits both `workspace.repo.write` and `workspace.test` capabilities/actions. REVISE/BLOCK holds the case; a legacy v1 plan must be re-planned before implementation. Review does not execute the validation commands or materialize an implementation Task. Native model invocations remain UNENFORCED; monetary accounting and worker-based model composition remain future work.

`internal/repoworkspace.Checkout` is the pinned, detached source-preparation primitive for supervised local implementation; the current general worker still creates an empty attempt workspace. Selected context files must be regular tracked UTF-8 files; symlinks, submodules, path traversal and context exceeding 32 files or 256 KiB are rejected. Allowed paths are exact file names, including explicitly permitted new files.

### Command Code backend

Command Code (`cmd`, plan Go: CLI only, no API) is selected with `--model-commandcode=<model id>` (or `--review-model-commandcode`); the binary defaults to `cmd`. The adapter builds the whole argv itself (`--skip-onboarding -t -p -m <id> --output-format json --permission-mode plan --max-turns 4 --no-session --safe-mode --no-skills --no-auto-update`; the first two stop cmd asking, in the fresh scratch directory of a fresh model user, whether to trust the location and whether to run taste analysis) and refuses `--model-arg` with it, because `cmd` takes the last of repeated options and accepts unknown ones, so operator arguments could drop plan mode or name a second model. A `cmd`-like binary (`cmd`, `cmdc`, `commandcode`, `command-code`) without `--model-commandcode` is refused. Each call runs in a fresh scratch directory under the model user's `~/tmp`, so one call cannot leave project config for the next. The prompt goes on stdin. The adapter reads the last NDJSON `result` line; an error result, a missing or empty answer, a rate limit or a non-zero exit fails the stage closed. Model output is capped at 64 MiB. Token usage is logged. The login is `~/.commandcode/auth.json`, the only Command Code file `scripts/setup-model-user.sh` offers to copy. **Plan mode is not a sandbox:** the model can still read files the model user can read (including the copied CLI logins) and fetch web pages. What keeps the Box's secrets away from it is the separate OS user, not plan mode.

### Harness profiles

`--model-profile=<name> --model-name=<id>` (and `--review-model-profile`/`--review-model-name`) run any other harness through a *profile*: the whole argv, how the prompt is delivered (stdin, one stdin JSON line, or argv up to 100 KiB) and how the answer is read (text, one JSON object, or the last matching JSON line). `--model-arg`, `--model-commandcode`, `--model-zdr` and `--model-allow-paid` are refused with a profile, for the reason given for Command Code below. Built in: `agy`, `claude`, `codex`, `copilot`, `opencode`, `ollama`. Each profile declares the argument run that keeps the harness read-only (`read_only`) or that it has no tools (`no_tools`), and a validator refuses auto-approve style options (`yolo`, `allow-all`, `bypass`, accept-edits modes). That is a tripwire; the boundary is still the separate OS user. Confirmed live on 2026-10-26: `claude`, `copilot` (`--model-name=auto`), `opencode`, `ollama`. `agy`'s success result and `codex` (over its limit until 2026-11-21) are unconfirmed. The model user cannot run CLIs installed under a 0750 home: only `ollama` ran as `summa42-model` here.

A profile may set `max_prompt` (bytes): a longer prompt is refused before the call. `agy` has 110000 because it silently truncates long input (about 119 KB kept, with a note telling the model to fetch the rest with commands, which plan mode denies, so the answer comes back empty). Keep the prompt under it by lowering the planner context with `--context-max-bytes` (e.g. `80000`; default is the 256 KiB maximum).

Adding a harness is data, not code. `summa42-box model-profile-draft --binary=<tool> [--help-args=<sub>]` runs `<tool> --help` as the model user, asks the configured model for a profile, validates it and stores it in `$SUMMA42_HOME/model-profiles/pending/` (inert). Read it, then run `summa42-box model-probe --model-profile=<name> --model-profile-dir=<pending dir> --model-name=<id>` (one live request, answer parsed), a passing probe leaves `<name>.probed` (a hash of the profile bytes) beside the draft, and `summa42-box model-profile-adopt --name=<name> --yes` installs the profile in `$SUMMA42_HOME/model-profiles/` only if that stamp matches the file as it is now (an edit after the probe means probing again). Profile files there must be regular files owned by the Box user and not writable by others, may not reuse a built-in name, and reject unknown fields. A drafted profile is never active until you adopt it.

Free models only by default: `poolside/laguna-s-2.1-free`, `reflection/beam:free`, and `stealth/glyph-cluster:free` (100 requests a day, reset at 02:00). Any other id spends the plan's monthly pool and needs `--model-allow-paid` / `--review-model-allow-paid` (env `SUMMA42_ALLOW_PAID_MODELS=1`). `--model-zdr` (env `SUMMA42_MODEL_ZDR=1`) sets `CMD_ZDR=1`; the free models have no zero data retention, so it is refused with them. Use a paid ZDR model for private sources such as ADO. `scripts/call-worker.py --backend commandcode --model <id>` applies the same rules with `--allow-paid-models` and `--zdr`. Run directly from a shell, `call-worker.py` is not isolated: it runs as you and sees your files and logins. Isolation applies only when the Box launches it as the model user.

Suggested mapping: producer (triage, plan, implementation) `reflection/beam:free`; reviewer (triage review, plan review, code review) `poolside/laguna-s-2.1-free`. Glyph Cluster's daily limit runs out within hours of a live loop. The reviewer must be a different model from the producer. The free deals last "while capacity lasts" and change without notice (MiniMax M3.1 Flash was withdrawn on 2026-10-23). A quota, rate limit or withdrawn model fails the call as "unavailable": the case stays ACTIVE and the next pass retries, so a withdrawn model stalls the loop until you change it.

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

Validation commands execute model-written code (for example a test the model added runs under `go test`), so they run as a second dedicated OS user, **`summa42-validate`**, never as the Box user and never as the model user. The validation user has no credentials and no CLI logins, and it cannot read the model user's home (mode 0700), so candidate code can neither steal the model CLIs' logins nor poison their configuration for the independent code review. Configure it with `--validation-user` or `SUMMA42_VALIDATION_USER`; it is required, and `run-gh-implement` (and the pipeline's implement stage) refuses to run without it. It must exist and differ from the model user, the Box user and root. The Box streams the committed candidate (`git archive <candidate sha>`, no `.git`; the Box first writes `* -export-ignore -export-subst` into the checkout's `.git/info/attributes`, so an in-tree `.gitattributes` cannot hide or rewrite files) to the validation user through the same launcher as every model call (`internal/modelrun`). The streaming limit is 5 minutes. A script running as that user unpacks it into a private scratch directory under `~summa42-validate/tmp/run.XXXXXX` (created with `mktemp`, removed afterwards, stale ones swept on the next run) and runs each command there with a clean environment (private HOME, TMPDIR and GOMODCACHE inside the scratch directory, `GOPROXY=off`, `GOTOOLCHAIN=local`, no inherited variables), a 2 GiB file-size limit (only a guard against a runaway single file; the compiler's package archives alone are several MiB) and a per-command timeout, **10 minutes by default**, set with `--validation-timeout` or `SUMMA42_VALIDATION_TIMEOUT` (the overall run is still bounded by the implementation lease). It prints one JSON document with per-command exit code, timeout flag, duration and the first 16 KiB of combined output. The Go build cache is per-run, inside the scratch directory, and removed with it; no persistent cache is shared across candidates. The Box validates the reply strictly (exactly one document with nothing but whitespace after it, one result per command) and keeps recording the evidence. If a validation is cancelled, the script kills the running command's process group before removing the scratch; and after every validation run, whatever the outcome or cancel, the Box runs `sudo -n -u summa42-validate /bin/kill -KILL -1` (best effort, logged) and removes that user's scratch directories, so nothing the candidate left running survives (validation runs are serialized across Box processes by an exclusive file lock, `--validation-lock` / `SUMMA42_VALIDATION_LOCK`, default `validation.lock` next to the state DB, so one Box's sweep never kills another's run; Boxes with different state directories that share `summa42-validate` must point at the same `--validation-lock`). **Toolchain:** `SUMMA42_VALIDATION_TOOLCHAIN_DIRS` (colon-separated absolute directories, put in front of the fixed PATH `/usr/local/bin:/usr/bin:/bin`) must point at a Go toolchain that `summa42-validate` can execute. Because validation uses `GOTOOLCHAIN=local`, it has to be the version the repository needs; for this repository that is go1.27.x, for example installed under `/usr/local/go1.27` (`SUMMA42_VALIDATION_TOOLCHAIN_DIRS=/usr/local/go1.27/bin`). `/usr/bin/go` is usually older, and the Box owner's toolchain under their home is not reachable by the validation user. `scripts/setup-model-user.sh --toolchain-dirs DIRS` (or the variable) checks that `go version` works as `summa42-validate`. The Box's own PATH is never used. Go module downloads are disabled and the module cache starts empty: a repository that needs dependencies should vendor them (`vendor/` is used automatically), because an owner-supplied module cache under the Box home is not readable by the validation user. A prepopulated cache that the validation user can read can still be supplied by an explicit argv, for example `["env","GOMODCACHE=/srv/gomodcache","go","test","./..."]`. Commands cannot run `git` against the candidate (the tree has no `.git`). The scratch copy is separate from the Box's checkout, so a command cannot change the committed candidate. Residual risk: candidate code can tamper with its own validation result (never with the Box's files or the model's logins). Recorded validation results are therefore not proof: the code-review prompt tells the reviewer so, and a pass counts only together with the reviewer's own reading of the tests and changed code. The independent code review and the Owner's approval are the real controls. This slice does not accept code or publish; publication is a separate step.

### Independent local code review

After `run-gh-implement` records a candidate, run a separately configured reviewer:

```sh
./bin/summa42-box run-gh-code-review --case <case-id> \
  --source-repo /absolute/path/to/repository --model-binary <review-wrapper>
```

The wrapper receives the original issue, exact accepted plan/context, implementation evidence and complete before/after contents and modes for every changed file. It returns only `{"verdict":"ACCEPT","reason":"…"}` (or `REVISE` / `BLOCK`). Projection is limited to 64 changed paths and 512 KiB total file contents; oversized or non-text changes are rejected rather than truncated. Configure a reviewer independently from the implementation wrapper; the system provides a separate invocation and Task/Attempt, not proof of different provider identity.

A claimed authority-free review Task shares the original Mission/resource envelope. Its attempt lease is the model timeout plus two minutes, so a slow but healthy model call is never cut off by its own lease. Verdict evidence binds the exact Case/Work, implementation evidence/hash and candidate, and review Task/Attempt/fence. Interrupted completion/acceptance resumes the saved result; an expired staged review requires operator recovery. Completed reviews are reused, and tests are not rerun. Source/candidate drift or a changed issue revision/grant blocks completion/acceptance.

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

### Holding cases by hand

`./bin/summa42-box block-cases --reason <why> <case-id>...` is the Owner's manual stop. It moves ACTIVE cases to BLOCKED through the same assessment path a stage uses (one `workflow_assessments` row per case). It first stores one `summa42.owner.case_block` evidence object with the reason and the case list, and every assessment cites it. It prints one JSON line per case and exits non-zero if any case was not ACTIVE or could not be blocked. A case that belongs to an inactive mission cannot be assessed, so it cannot be blocked this way.

`./bin/summa42-box cancel-tasks <task-id>...` moves ELIGIBLE tasks to CANCELLED with a `TASK_CANCELLED` execution event, one JSON line per task. A task that is not ELIGIBLE is reported and left alone, and the command then exits non-zero.

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
- Model answers can take minutes: `--model-timeout` / `--review-model-timeout` default to 30m. Every lease that wraps a model call is derived from it (plan/code review: timeout + 2m; implementation: timeout + every validation command's timeout and kill grace + the 5m archive cap + a margin; triage worker: timeout + 5m), as are the pipeline stage timeouts, so a slow but healthy call is not cut off. `scripts/call-worker.py` defaults `--timeout` to 25m across all fallbacks, inside the Box timeout. Crash recovery of a stuck attempt therefore waits up to the derived lease.
- `--allowed-prefix DIR/` lets the model select files per issue within a directory; it excludes `--allowed-path`, which fixes exact files for every issue. `--context-file` supplies exact context.
- **Model user (required).** Model processes never run as the Box owner. Every command that starts a model (`run-gh-pipeline`, `run-worker`, the `run-gh-*` / `run-repo-*` stage commands, the review model and the Copilot executor) needs `--model-user <user>` or `SUMMA42_MODEL_USER`, naming a dedicated OS user. Construction fails closed when it is unset, root or the Box's own user; there is no same-user escape flag. There is one launcher (`internal/modelrun`): `sudo -n --preserve-env=<allowlisted names> -u <user> -- /usr/bin/timeout --kill-after=15s <T> /bin/sh -c '… exec env -i HOME=<that user's home> PATH=… LANG=… TMPDIR=<home>/tmp NAME="$NAME"… <binary> <args>'`, in the model user's home, so the model can read neither the Box's environment nor its files: `~/.summa42/keys/*`, `github_token`, `gh-app.pem`, `discord_auth.json`, the state DB and `~/.config/gh`. The deadline is enforced on the model user's side by `timeout` (the Box uid cannot signal another uid's processes) and again by `cmd.WaitDelay` on the Box side. An early cancel (lease lost, shutdown) is not left to that deadline: the wrapper prints its process-group id on the first stderr line (a marker the Box strips), and Cancel then runs `sudo -n -u <user> -- /bin/kill -TERM -- -<pgid>`, followed by `-KILL` after a 10-second grace, because the Box uid cannot signal sudo (it runs as root). The PATH of a model process is always `/usr/local/bin:/usr/bin:/bin` (never the Box's PATH); the Copilot executor runs in a private per-run scratch directory the model-side script creates under the model's `~/tmp` (not in the Box-owned attempt workspace, which the model user cannot enter). Before the first launch for each dedicated user (model and validation) the Box probes, as that user and through the same launcher, that each secret file is unreadable by its full path: the Summa42 home, the `keys` directory and every file in it (including `owner.key`), the state DB (plus `-wal`/`-shm` when present), the evidence directory, `config.json`, and, when present, `github_token`, `gh-app.pem`, `discord_auth.json` and `~/.config/gh/hosts.yml` (`test -r`, with a positive control so a broken sudo rule cannot pass vacuously). The validation user must also be unable to read the model user's home. A missing required path, or an unreadable-looking path that is in fact readable, refuses to start the process; only the optional paths may be absent. **Validation user (required for validation).** Candidate code runs as a separate user, `summa42-validate` (`--validation-user` / `SUMMA42_VALIDATION_USER`), which has no credentials; see the validation paragraph above. The isolation probe below runs for both users. Run `sudo scripts/setup-model-user.sh` once (pass `--summa-home DIR` if your Summa42 home is not `~/.summa42`, since sudo strips `SUMMA42_HOME`; `--db` / `--evidence` override the paths read from `config.json`) to create the `summa42-model` and `summa42-validate` users (nologin system users, each with its own home and no groups), install a validated drop-in for each (`/etc/sudoers.d/summa42-model` and `/etc/sudoers.d/summa42-validate`: `<owner> ALL=(summa42-validate) NOPASSWD: SETENV: ALL` plus `Defaults>summa42-validate !log_allowed`; rolled back if `visudo -c` of the whole configuration fails), chmod `~/.summa42`, `~/.config/gh` and the model home to 0700 (never through a symlink; the validation home and its `tmp` are 0700 too), refuse if the validation user's home holds any credential file or token pattern (nothing is ever copied there), and verify that neither user can read the secrets and that the validation user cannot read the model home and verify the secrets: the configured home, `keys`, the state DB with its `-wal`/`-shm`, the evidence directory and the credential files must exist and must be unreadable, otherwise it exits non-zero. Root never reads, writes or changes anything inside the model's home after creating it: every copy, rename, chmod and mkdir there is done by the model user itself, so a symlink planted by a model cannot redirect root. To avoid re-logging in, the script offers to copy only the **auth files** of your existing CLI logins: `~/.claude/.credentials.json` plus a filtered `~/.claude.json` (login metadata only; `mcpServers`, project history and transcripts are never copied), `~/.codex/auth.json`, and for Gemini/Antigravity `~/.gemini/{oauth_creds.json,google_accounts.json,installation_id}` plus `settings.json` reduced to its `security.auth` section (its `mcpServers`, `hooks` and `tools` are dropped). The copied content is scanned for GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), Azure DevOps PATs (52-character and 84-character values, quoted or not, and any `*_PAT` / `AZURE_DEVOPS_*` / `ADO_*` key), `Bearer` / `Basic` authorization headers and private keys (binary files are scanned as text, not skipped), and the copy is refused, naming the file, if any is found. It asks per CLI, showing source and destination, and copies only on `y`/`yes`; `--yes` (alias `--default-approve`) approves all except Copilot; with no TTY and no `--yes` nothing is copied and the skipped ones are listed. Copilot's own login (`~/.copilot/config.json`, `~/.config/github-copilot/{apps,hosts}.json`) is a GitHub credential, and its MCP server definition (`~/.copilot/mcp-config.json`, which the Copilot executor needs in the model user's `~/.copilot`) is offered with the same scan: it is skipped under `--yes` and copied only after an explicit interactive `y`. The script is idempotent: re-run it to add credentials for a CLI logged in later or to refresh a copy; an existing user is accepted only if it is a nologin system account, and an unchanged sudoers file is left as is. `~/.config/gh`, `~/.summa42` and gh tokens are never offered. Any other CLI can be logged in directly, e.g. `sudo -u summa42-model -H codex login`; model binaries must be executable by it. `scripts/summa42-acp.js` chat spawns `agy`/`ollama` the same way (uid comparison, `timeout` wrapper; the ollama prompt goes on stdin, the `agy -p` prompt is still an argument because that CLI takes it there).
- **sudo log and `ps`.** The drop-ins contain `Defaults>summa42-model !log_allowed` and `Defaults>summa42-validate !log_allowed`, so commands run as these users are not written to the sudo log (journal), and allowlisted variable values travel in the environment (`--preserve-env`), never on argv. The Copilot and `agy` prompts are still process arguments, visible in `ps` to local users for the duration of a call; use a single-user host.
- **WSL.** On WSL the isolation is incomplete while Windows interop is on and `/mnt/c` is world-readable: the model user can read Windows files (for example the Windows `gh` token) and start Windows executables. The setup script detects WSL and prints a clear warning; it neither refuses nor edits `/etc/wsl.conf`. To close the gap set `[interop] enabled=false`, `appendWindowsPath=false` and `[automount] options="metadata,umask=077,fmask=077"` in `/etc/wsl.conf`, then `wsl --shutdown`. That also turns interop off for your own shell. To keep interop for yourself (browser logins), run the Box in a systemd unit that cannot see Windows instead: `sudo systemd-run --unit=summa42-loop --uid=$USER -p InaccessiblePaths=/mnt/c -p InaccessiblePaths=/run/WSL <Box command>`. The model users are started by the Box inside the unit and inherit the hidden paths; verified on 2026-10-26 (both users: `/mnt/c` unreadable, `cmd.exe` not startable, with `/run/WSL` hidden because its interop sockets are world-writable). Plain Linux with the dedicated user is the supported target.
- Model processes get a minimal environment (PATH, LANG, the model user's own HOME and TMPDIR plus the names you list with `--model-env` / `--review-model-env`); GitHub and `SUMMA42_` credentials are rejected as variables and, because the model runs as another user, the Box's credential files are unreadable to it. Allowlisted values are passed in the launcher's environment, not on argv, but the model can read them, so list only variables meant for the model. Prompts are sent on stdin. The optional router `scripts/call-worker.py` fails closed (non-zero exit, empty stdout) and uses OpenRouter only when opted in (`--allow-remote` or `SUMMA42_ALLOW_REMOTE_MODELS=1`); it runs as the model command, so its `~` paths are the model user's.
- The pipeline ends at a published **draft PR**; the case moves to `READY_FOR_VERIFICATION` and the Owner merges. No merge or issue-closure stage exists.
- Each tick first runs recovery (lease handling and external-operation reconciliation), skipping foreign and in-flight operations. Policy-required approvals still hold the case until the Owner approves.

Every `run-gh-*` command also has a `run-repo-*` alias (for example `run-repo-pipeline`, `run-repo-intake`).

### Operational notes

- Several `summa42-box` processes may share one local SQLite database (`_txlock=immediate`); recovery runs at worker start and on every pipeline tick.
- Validation runs as the `summa42-validate` user with a private module cache inside its scratch directory, a persistent Go build cache in that user's home, and a per-command timeout of 10 minutes by default (see above).
- Git source preparation uses `GIT_NO_LAZY_FETCH=1`.
- Supervised publication always creates **draft** pull requests. Nothing in Summa42 marks them ready or merges them.
- Native execution (including model wrappers) is UNENFORCED. The OCI TEB egress/containment and the MCP adapter/capability Session are primitives that are not wired into execution.

### Verify isolation on a real host

The tests use fakes. After `sudo scripts/setup-model-user.sh` on the real host, check the isolation yourself, as the Box owner (needs real sudo; the Windows/WSL gap is warning-only):

Steps 3–6 and the probe check of step 8 are automated (as the Box owner, after the setup script): `S42_TOOLCHAIN=/usr/local/go1.27/bin go test -tags realhost -run RealHost -v ./internal/repoworkspace`. A cancelled (SIGKILLed) model call keeps its scratch directory until the day-old sweep of a later call. One real model call (Command Code, free model, as `summa42-model`): `S42_CMD=/opt/summa42-node/bin/cmd go test -tags realhost -run RealHost -v ./internal/ghtriage/climodel`. Model binaries must be outside your home and executable by the model user, for example root-owned copies under `/opt` with `node` linked into `/usr/local/bin` (the model PATH is fixed).

1. Run `sudo scripts/setup-model-user.sh` and confirm it exits 0. Then, for both users (`summa42-model`, `summa42-validate`), confirm each secret is unreadable. Each of these prints `1`:
   ```sh
   for u in summa42-model summa42-validate; do
     for f in ~/.summa42/keys/owner.key ~/.summa42/config.json ~/.summa42/github_token ~/.summa42/gh-app.pem \
              ~/.summa42/discord_auth.json ~/.config/gh/hosts.yml "$DB" "$DB-wal" "$DB-shm" "$EVIDENCE_DIR"; do
       [ -e "$f" ] && { sudo -n -u "$u" -- /usr/bin/test -r "$f"; echo "$u $f $?"; }
     done
   done
   ```
2. The validation user cannot read the model home or its logins: `sudo -n -u summa42-validate -- /usr/bin/test -r /var/lib/summa42-model` prints exit 1, and so does the same for `/var/lib/summa42-model/.claude/.credentials.json`. `ls -ld /var/lib/summa42-model` shows `drwx------`. `sudo ls -A /var/lib/summa42-validate` shows no `.claude`, `.codex`, `.gemini`, `.copilot` or `.config`.
3. Run one model call and confirm the first stderr line is `__S42_PGID=<n>` and is stripped from the output (for example run `summa42-box run-repo-triage-review` on a test case with `strace -f -e trace=write` or a debug log). Confirm `sudo` prints no warning before it (hostname resolution); if it does, the warning is passed through and the marker is still found.
4. Start a long model call, cancel it (Ctrl-C or kill the Box process), and confirm `ps -u summa42-model` is empty within about 15 s.
5. Run validation with a real `go test ./...` on a small Go repository. With `SUMMA42_VALIDATION_TOOLCHAIN_DIRS` set to a Go 1.27 bin directory that `summa42-validate` can execute, it must pass; the cache is per-run and removed afterwards.
6. Kill the Box during a long validation command (for example `sleep 600` as the command) and confirm `ps -u summa42-validate` is empty and `/var/lib/summa42-validate/tmp` has no `run.*` directory afterwards. Also let a validation finish and confirm the sudo-driven `kill -KILL -1` left nothing behind.
7. Copilot executor: one real run. Confirm the cwd is `/var/lib/summa42-model/tmp/run.*`, the directory is removed afterwards, and MCP tools load from `/var/lib/summa42-model/.copilot/mcp-config.json`.
8. Probe negative control: `chmod 0755 ~/.summa42 ~/.summa42/keys` temporarily, then confirm the next model command refuses with "user ... can read". Restore 0700 afterwards. Repeat with `chmod 0755 /var/lib/summa42-model` and a validation run.
9. `sudo -l -U "$USER"` shows only `(summa42-model)` and `(summa42-validate)`. As the Box user, `sudo -n -u root true` fails.


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

- With `LOCAL_DEV_FILE` custody the Owner key and the control token live in `~/.summa42`, so approval is not a boundary against UNENFORCED executors running as the same OS user: such an executor can approve its own request. Model processes are the exception: they run as the separate model user and cannot read `~/.summa42`. Validation commands run as the separate validation user; other native executors still run as the Box user.
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
