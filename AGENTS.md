# AGENTS.md

## Autonomy and decisions

When a choice is required during a task (design trade-off, review finding, plan-vs-spec
conflict, tooling option), pick the recommended option on my behalf and continue. Do not
stop to ask. Only interrupt me when work is genuinely blocked, not merely ambiguous, and
state the recommended resolution together with the blocker.

Tie this to a high-confidence reasoning first: if the confidence behind the recommended
option is not high, say so explicitly and ask.

## Environment

- Go 1.27. Run go commands with `GOCACHE=/tmp/summa42-full-go-cache`; the default GOCACHE
  is not writable in this environment.
- Long-running validation gates need an explicit timeout. The concurrency-flake gate is
  `go test -run TestCloseConcurrentReplay ./internal/workflowcase -count=600 -timeout 30m`:
  that single test takes ~1 s per iteration, so 600 iterations is ~10 min and collides with
  the default 10 min timeout. The whole `internal/workflowcase` package takes ~12 s per
  `-count=1`, so do not multiply the entire package by a high `-count` — scope the count to
  the one test.

## Development plan

Work is organized by `docs/superpowers/plans/2026-11-11-development-plan.md`. Before
implementing anything, read its sections 1 to 4. Its section 3 (rules R1 to R10) is
binding: one task card per branch and draft pull request, tests first, no package or
table without a caller in `cmd/`, no fabricated results, protected paths need the Owner.
