# Parked spikes (2026-11-10)

These packages were merged between 2026-11-06 and 2026-11-10 and removed from
`main` on 2026-11-10 because no running path called them: nothing outside their
own tests (or another package on this list) imported them. They are parked, not
discarded. Every file is in the repository history.

The last commit on `main` that contains all of them is **`c3d5454`**. To bring
one back, check it out from there and wire it to a caller in the same change:

```bash
git checkout c3d5454 -- internal/mission
```

A package returns to `main` when a command or a running Box calls it, an
acceptance test exercises that path, and its tests check behavior against the
real kernel rather than against values the test supplied.

## Index

| Parked | Issue | Pull requests | Commits (oldest first) | Restore |
| --- | --- | --- | --- | --- |
| `internal/mission` (structured Mission schema, intent compiler, goal research, oracle) | #12 (#38 to #41) | #165, #166, #167, #168 | `aeaa565`, `3193ecb`, `c28acf6`, `ca89b9c` | `git checkout c3d5454 -- internal/mission` |
| `internal/decision` (decision engine, rule and LLM providers, provenance) | #16 (#46 to #49) | #157, #158, #159, #160 | `965def4`, `cc246ae`, `71030f0`, `1466f71` | `git checkout c3d5454 -- internal/decision` |
| `internal/dtu` (digital twin router, retry safety, adversarial scenarios, blue/green gate) | #11 (#34 to #37) | #161, #162, #163, #164 | `25d63f5`, `b266239`, `b600ff0`, `1686aad` | `git checkout c3d5454 -- internal/dtu` |
| `internal/replay` (replay fixtures and offline harness) | #4 (#61, #62) | #147, #148 | `2a44f86`, `726e018` | `git checkout c3d5454 -- internal/replay` |
| `internal/epistemic` (epistemic schemas, reflection, consolidation, process evolution gate) | #15 (#42 to #45) | #169, #170, #171, #172 | `70d3643`, `89e1174`, `02cdb4d`, `4033ac5` | `git checkout c3d5454 -- internal/epistemic` |
| `internal/delegation` (foreign principals, delegation records) | #17 (#50) | #154 | `7b1e638` | `git checkout c3d5454 -- internal/delegation` |
| `internal/evidence/lineage.go`, `acceptance.go`, `acceptance_test.go` (inter-Collective evidence lineage and acceptance) | #17 (#51) | #155 | `a862e65` | `git checkout c3d5454 -- internal/evidence/lineage.go internal/evidence/acceptance.go internal/evidence/acceptance_test.go` |
| `internal/gateway` (inter-Collective boundary gateway and sanitizer) | #17 (#52) | #156 | `48c476e` | `git checkout c3d5454 -- internal/gateway` |
| `internal/teb/openshell.go`, `openshell_test.go` (NVIDIA OpenShell TEB adapter) | #3 (#57) | #173 | `fb5f1c0` | `git checkout c3d5454 -- internal/teb/openshell.go internal/teb/openshell_test.go` |
| `internal/executors/maf.go`, `maf_test.go`, and the `ProposedDelta` field of `executors.ExecutionResult` (Microsoft Agent Framework adapter) | #3 (#58) | #174 | `02931cc` | `git checkout c3d5454 -- internal/executors/maf.go internal/executors/maf_test.go` and re-add the field |

Dependencies between them: `gateway` and the evidence lineage files need
`delegation`; `dtu` needs `replay`; the MAF adapter needs `epistemic`.

## What stays

- The provider-neutral `teb.Provider` manager and the Docker provider from #57
  (`internal/teb/provider.go`, `docker_provider.go`).
- The context projection compiler from #59 (`internal/runmanifest/compiler.go`),
  which no running path calls yet.
- Migration `00034_foreign_evidence_lineage.sql`. Applied migrations are never
  removed; its two tables stay empty.
- The research notes for the spikes in this directory.

## What to check before restoring

Findings from the 2026-11-10 review that a restore should resolve first:

- `mission`: `Oracle.Evaluate` matches fixed phrases taken from its test
  fixtures and returns a constant confidence. `mission`, `decision` and `dtu`
  each create a table outside the goose migrations, and `mission` keeps a
  Mission model separate from the `missions` table the CLI uses.
- `replay`: invariants are asserted on flags the simulated executor reports
  about itself; nothing passes through the kernel services.
- OpenShell adapter: `Assess` returns the requested profile unchanged, and
  `Execute` reports the requested enforcement level with constant evidence
  strings. No gateway client exists. This contradicts the rule that an
  execution path states only what it can prove.
- `decision`: there is no `LLMClient` implementation.
- `epistemic`: state lives in process memory only.
