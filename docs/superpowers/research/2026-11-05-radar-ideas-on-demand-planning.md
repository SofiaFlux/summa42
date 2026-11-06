# Summa42 — Ideas parked from the 2026-11-05 AI radar

**Status:** Parking note; nothing here is a decision or implemented
**Date:** 2026-11-05
**Input:** external AI radar report (2026-11-05) + local check of `main` at `42224c9` (merge of PR #131)

---

## Repo state (verified 2026-11-05)

- #114 (parent) → #132–#135: planning with on-demand file reads. All open, nothing implemented.
- Today the planner only sees files chosen up front: `Source.Paths` + `SelectFiles` (`internal/ghtriage/grounding.go:212`), trimmed by `FitContext`.

## Ideas to keep

1. **Keep the order #132 → #133 → #134 → #135.** Land the `ReadFile` primitive with its table tests first (`../x`, `/etc/passwd`, `.git/config`, `sub/.GIT/HEAD`, `.env`, `id_rsa`, symlink pointing outside, submodule, directory, binary, oversize), only then the loop.

2. **Three metrics for #135**, so the new mode can be compared with `SelectFiles`, not just shown to work:
   - bytes and model calls per accepted plan;
   - share of extra reads that actually influenced the plan (files from the turn log that the plan references);
   - whether reviewer and implementer receive exactly the file set recorded in the #134 context proof — checked by `File.SHA256`, not by path.

3. **Guiding rule:** the model decides what it needs; the deterministic layer decides whether, from which commit, and within which budget. A refused path goes back to the model as information and does not end the task.

4. **Patterns from ShipIT Forge** (`shipiit/forge`, MIT; small project, ~130 commits — borrow ideas, do not import):
   - deterministic scanner (secrets, IaC) → agent → second agent re-reads the code and verifies each finding;
   - review limited to changed files; skip re-review when a push did not touch files covered by the previous review;
   - per-stage cost accounting (calls, tokens, cost, outcome);
   - skills bound to an explicit tool allow-list: a `SKILL.md` is an instruction, not a permission control.

5. **WSL2 isolation, confirm on a real host:** the model process cannot read `/mnt/c` or launch Windows binaries via Interop; with the systemd unit from the README, `/run/WSL` is hidden too. The README still marks default local Box execution as `UNENFORCED`.

6. **If Summa42 invokes `claude -p` (Claude Code ≥ 2.1.318):** judge success by exit code and `credential_expires_at` in the JSON result, not by the response text. For Agent SDK hooks that act as a safety control, use `onFailure: "block"` (2.1.317).

## Parked

- Kimi K3.1 Pro (open weights, 2.8T MoE): local run needs ~8× MI355X; API comparison possible but no current reason to change executors.
- GPT-6 Cyber: access limited to verified defense teams (Daybreak); stated scope compliance does not replace deterministic permission and execution control.
