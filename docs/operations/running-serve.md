# Running Summa42 unattended on one repository

This is the shortest path from a fresh Linux host to a Box that takes labelled
GitHub issues of one repository to draft pull requests. It uses what exists
today. Section 8 lists what you still do by hand.

What was checked, and how:

- The start script and the example settings are run by a test
  (`cmd/summa42-box/serve_deploy_test.go`) that gives the resulting flags to
  the real flag parsers.
- `serve --control`, `mission create`, `envelope create`, the refusal of a
  second `serve` and `emergency freeze` were run together once on a fresh
  Collective.
- **Not checked here:** a complete run against GitHub with real model CLIs.
  That needs your token and your model logins. Treat the first issue as the
  test.

## 1. Accounts on the host

Three OS users. Never run the Box as root.

| User | Purpose |
| --- | --- |
| `summa42` (any name) | The Box user. Owns the Collective home, the token and the Owner key |
| `summa42-model` | Runs the model CLIs. Created by the setup script |
| `summa42-validate` | Runs candidate code (tests). No credentials. Created by the setup script |

## 2. Build and initialize

As the Box user, in a checkout of this repository (Go 1.27):

```bash
go build -o ./bin/summa42 ./cmd/summa42
go build -o ./bin/summa42-box ./cmd/summa42-box
sudo install -m 0755 ./bin/summa42 ./bin/summa42-box /usr/local/bin/
summa42 init
```

`summa42 init` creates `~/.summa42` with the database, the evidence directory
and the Owner key. Keep a copy of `~/.summa42/keys/owner.key` somewhere safe:
nothing in Summa42 backs it up yet.

## 3. Model and validation users

```bash
sudo scripts/setup-model-user.sh --toolchain-dirs /usr/local/go/bin
```

The script creates the two users, lets the Box user run commands as them
through `sudo`, and proves that neither can read the Box's secrets. The Go
toolchain directory must hold the Go version this repository needs and must be
readable by `summa42-validate`.

Install and log in the model CLIs **as the model user** (the README section
"Harness profiles" lists them), then confirm each one with a live request:

```bash
summa42-box model-probe --model-profile=<profile> --model-name=<model id> --model-user=summa42-model
```

Use two different models: one produces (triage, plan, implementation), one
reviews. The pipeline refuses to start when they are the same.

## 4. Mission, budget and Owner presence

The Mission is created through the control daemon, so start it for a moment:

```bash
summa42-box serve --control &
summa42 mission create "Maintain <owner>/<name> safely"   # prints the Mission ID
kill %1
```

Give the Collective a budget. Units are bounded requests, not money; a
publication costs four. The command is signed with the Owner key and the grant
is recorded with its signature:

```bash
summa42 envelope create summa42-work --limit 1000 --reason "first budget"
summa42 envelope list
```

The dead man switch stops new work when the Owner has not been seen for the
configured horizon. Check it, and record your presence from time to time:

```bash
summa42 emergency status
summa42 emergency heartbeat
```

## 5. GitHub access and source

- A token that can read issues and open pull requests on the repository, in a
  private file owned by the Box user: `~/.summa42/github_token`, mode `0600`.
- A local clone of the repository that the Box user can read, for example
  `/srv/summa42/source/<name>`, with the base branch checked out and current.
- An empty directory outside that clone for attempt workspaces, for example
  `/srv/summa42/attempts`.

The pipeline works on one pinned commit: the start script pins the commit of
the base branch in the local clone when it starts. After a pull request is
merged, update the clone and restart the service (section 7).

## 6. Install the service

```bash
sudo install -d /usr/local/lib/summa42 /etc/summa42
sudo install -m 0755 deploy/summa42-serve.sh /usr/local/lib/summa42/
sudo install -m 0644 deploy/serve.env.example /etc/summa42/serve.env
sudo install -m 0644 deploy/systemd/summa42-serve.service /etc/systemd/system/
sudoedit /etc/summa42/serve.env          # fill in every value
sudoedit /etc/systemd/system/summa42-serve.service   # set User= to the Box user
sudo systemctl daemon-reload
sudo systemctl enable --now summa42-serve
journalctl -u summa42-serve -f
```

`/etc/summa42/serve.env` holds no secrets. Its values:

| Setting | Meaning |
| --- | --- |
| `SUMMA42_HOME` | The Collective home of the Box user |
| `SUMMA42_MISSION`, `SUMMA42_ENVELOPE` | From section 4 |
| `SUMMA42_GITHUB_REPOSITORY`, `SUMMA42_BASE_BRANCH` | The repository and the branch pull requests target |
| `SUMMA42_MAINTAINER` | The login whose issues are taken. Issues by others are ignored |
| `SUMMA42_GITHUB_TOKEN_FILE` | The private token file |
| `SUMMA42_AUTO_GRANT_LABEL` | Only issues with this label may be implemented. Empty: plan only |
| `SUMMA42_SOURCE_REPO`, `SUMMA42_WORKSPACE_ROOT` | From section 5, absolute paths |
| `SUMMA42_ALLOWED_PREFIX` | The directory changes may touch, ending in `/` |
| `SUMMA42_VALIDATION_COMMAND_JSON` | What validates a candidate, as a JSON argument array |
| `SUMMA42_MODEL_USER`, `SUMMA42_VALIDATION_USER`, `SUMMA42_VALIDATION_TOOLCHAIN_DIRS` | From section 3 |
| `SUMMA42_PRODUCER_*`, `SUMMA42_REVIEWER_*` | Harness profile and model ID of the two models |
| `SUMMA42_CONTEXT_MAX_BYTES` | Planner context limit; keep `80000` for `agy` |

The service runs `summa42-box serve` with three supervised loops: the control
daemon, intake and the pipeline. A loop that stops is restarted by `serve`;
`serve` itself is restarted by systemd. A second `serve` on the same
Collective refuses to start.

## 7. Daily use

| You want to | Do |
| --- | --- |
| Give it work | Open an issue as the maintainer and add the label |
| See what it is doing | `summa42 cases`, `journalctl -u summa42-serve` |
| See what one case cost | `summa42 case report <case-id>` |
| Approve a publication | `summa42 approvals`, then `summa42 approve <approval-id>` |
| Finish | Review and merge the draft pull request yourself |
| Continue after a merge | `git -C <source clone> pull`, then `sudo systemctl restart summa42-serve` |
| Stop everything now | `summa42 emergency freeze --reason "..."`. No new work starts, and a running model process is killed within seconds |
| Start again | `summa42 emergency resume --reason "..."` |
| Raise the budget | `summa42 envelope set-limit summa42-work --limit <n> --reason "..."` |

## 8. What you still do by hand

- **A plan or a candidate the reviewer sends back (`REVISE`) holds the case.**
  Nothing re-plans by itself yet (plan card M1-T1).
  `scripts/requeue-revise.py` posts the reviewer's reasons as your comment,
  which opens a new case.
- **Merging and closing the issue.** There is no merge stage.
- **Restart after each merge**, because the commit is pinned at start.
- **Notifications.** Nothing tells you that an approval is waiting or a case
  is held; look at `summa42 approvals` and `summa42 cases --state BLOCKED`.
- **Backups.** Copy `~/.summa42` (database, evidence, keys) yourself while the
  service is stopped.
- **One repository per Collective.** One Mission is active at a time.
