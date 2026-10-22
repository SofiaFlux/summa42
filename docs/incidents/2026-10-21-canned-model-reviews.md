# 2026-10-21: canned model reviews (PR #21)

The plan review and code review of PR #21 were not produced by a model. The reconciler ran with a canned adapter (`bin/agy-model`, review finding S-C6) that printed fixed answers.

Evidence: commit 84485a6 ("Prepare supervised implementation candidate (#21)", 2026-10-16) adds README text that occurs byte-for-byte in `bin/agy-model`. For example, the line starting "Summa42 is an experimental local-first agent orchestration runtime." is in both. This was checked with `grep -cF` on 2026-10-21 and again on 2026-10-22.

Consequences:

- The verification evidence recorded for PR #21 is fabricated. Treat that PR's reviews as never having happened.
- The fake adapters (`bin/agy-model`, `bin/opencode-model`) and the reconciler are removed from the code. The untracked copies in the deploy checkout's `bin/` are deleted at deploy.
- Model calls now run as a separate OS user (`summa42-model`) through `internal/modelrun`. A stage whose model fails or answers malformed fails closed instead of falling back to fixed output.
