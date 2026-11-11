# Contributing to Summa42

## Scope and behavioral expectations

Summa42 is an experimental, local-first, single-Cube MVC. Keep contributions consistent with that scope: do not present local controls as production guarantees, and do not expand authority, egress, or containment claims without matching implementation and evidence. Treat contributors and reviewers respectfully; the conduct policy is available at [CODE_OF_CONDUCT.md](https://github.com/SofiaFlux/summa42/blob/main/CODE_OF_CONDUCT.md).

## Local setup

Use Go 1.27, then download dependencies and build the local binaries:

```bash
go mod download
mkdir -p ./bin
go build -o ./bin/summa42 ./cmd/summa42
go build -o ./bin/summa42-box ./cmd/summa42-box
```

For a local smoke path, initialize a disposable home, start the Box in one terminal, and query it from another:

```bash
SUMMA42_HOME="$(mktemp -d)"
export SUMMA42_HOME
printf 'Reuse this SUMMA42_HOME in both terminals: %s\n' "$SUMMA42_HOME"
./bin/summa42 init
```

In terminal 1, export the printed path and start the Box:

```bash
export SUMMA42_HOME=/the/path/printed/above
./bin/summa42-box
```

In terminal 2, export that same path and query the Box:

```bash
export SUMMA42_HOME=/the/path/printed/above
./bin/summa42 status
```

## Focused changes and tests

A new package enters `main` together with a caller on a running path (a command or the Box) and a test over that path. A spike without a caller belongs in a research note or on a branch; `docs/superpowers/research/2026-11-10-parked-spikes.md` lists the ones parked so far and how to restore them.

Keep each change focused, explain its behavior and boundaries, and add or update tests for changed behavior. Run the relevant focused tests while working, then run `make verify` before requesting review.

Do not commit `SUMMA42_HOME`, keys, bearer tokens, SQLite databases, raw evidence, or credentials. Redact logs and examples before sharing them.

## Validation

Two `make` targets run the checks from the repository root:

```bash
make verify-fast   # while working
make verify        # before opening a pull request
```

`make verify-fast` runs the formatting check (`gofmt -l .` must print nothing), `go vet ./...`, `go build ./cmd/...` and `go test ./... -count=1`.

`make verify` runs `verify-fast` and then the Python unit tests (`python3 -m unittest discover -s scripts -p 'test_*.py'`), `scripts/verify_docs.py` on every tracked Markdown file, `go test -race ./... -count=1 -timeout 45m` and `python3 docs/superpowers/research/spikes/2026-09-14-sqlite-persistence-spike.py`.

Both use `GOCACHE=/tmp/summa42-full-go-cache` unless `GOCACHE` is already set. Run them as an unprivileged user: the `internal/ghtriage` tests refuse to run as root.

CI runs the same checks, plus a smoke run of the local bootstrap and control path.

On Linux with Docker available, also run the OCI bypass/capability acceptance profile. That gate must not be replaced with a unit-test-only approximation.

## Pull-request checklist

- [ ] The change is focused and its scope is described.
- [ ] Relevant tests were added or updated and `make verify` was run.
- [ ] Documentation reflects user-visible behavior and limits.
- [ ] The change contains no secrets, personal data, raw local state, or unredacted evidence.
- [ ] Suspected vulnerabilities are reported through [SECURITY.md](SECURITY.md), not a pull request.

## Inbound license

By contributing, you agree that your contributions are licensed under the Apache License 2.0, consistent with the repository [LICENSE](LICENSE).

For security reports, use [SECURITY.md](SECURITY.md). For support routing, use [SUPPORT.md](SUPPORT.md).
