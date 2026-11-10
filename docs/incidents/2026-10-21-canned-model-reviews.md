# 2026-10-21: canned model reviews (PR #21)

The plan review and code review of PR #21 were not produced by a model. The reconciler ran with a canned adapter (`bin/agy-model`, review finding S-C6) that printed fixed answers.

Evidence: commit 84485a6 ("Prepare supervised implementation candidate (#21)", 2026-10-16) adds README text that occurs byte-for-byte in `bin/agy-model`. For example, the line starting "Summa42 is an experimental local-first agent orchestration runtime." is in both. This was checked with `grep -cF` on 2026-10-21 and again on 2026-10-22.

Consequences:

- The verification evidence recorded for PR #21 is fabricated. Treat that PR's reviews as never having happened.
- The fake adapters (`bin/agy-model`, `bin/opencode-model`) and the reconciler are removed from the code. The untracked copies in the deploy checkout's `bin/` are deleted at deploy.
- Model calls now run as a separate OS user (`summa42-model`) through `internal/modelrun`. A stage whose model fails or answers malformed fails closed instead of falling back to fixed output.

## Follow-up 2026-11-10: text that the affected pull requests left in the vision

Pull request #21 also added the section "Summand as the task-scoped cognitive worker" to `docs/VISION.md`, and pull request #23, opened by the same pipeline on the same day, extended it and marked parts of it normative. This record establishes fabricated reviews for #21; the reviews of #23 were not re-examined.

The section stayed in the vision after the remediation. On 2026-11-10 it was reduced to direction-level text in a change the Owner reviews as an ordinary pull request: the claim that a compiled Context Projection is stored (it is not built) and the binary handover procedure were removed, and the rest is no longer called normative.

Rule from this follow-up: `docs/VISION.md` is the public constitution. A change to it is written for and reviewed by the Owner and does not go through the pipeline (Vision, Article 1: a runtime cannot amend the rules that constrain the runtime).
