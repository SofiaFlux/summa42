# Independent candidate review

Owner goal: Summa42 develops itself through bounded, evidence-backed issue work. Continue PR #20 without merging it. Select recommended decisions autonomously per AGENTS.md.

## Chosen scope

Add a supervised `run-gh-code-review --case ID --source-repo ABS --model-binary WRAPPER` command. A separate, authority-free review Task/Attempt is created with a deterministic key derived from the implementation Work. Keep the Case on its existing implementation Work; acceptance is a verification concern, not a new implementation or publication grant. This avoids advancing the workflow before independent review exists. Alternatives were a new Case Work or an unclaimed model call; both add unnecessary transitions or duplicate-call risk for this step.

The reviewer receives the original issue, exact accepted plan/context, immutable implementation record, and complete before/after contents and modes of every changed file. The projection is pinned to base/candidate SHA, rechecks candidate identity and cleanliness, supports additions/deletions, rejects nonregular/binary/invalid UTF-8 content, and caps aggregate contents at 512 KiB and 64 paths. Oversized projections fail closed without calling the model. Never truncate code into a reviewable candidate.

## Contract and lifecycle

Strict output is `{verdict: ACCEPT|REVISE|BLOCK, reason: bounded text}`. Independent means a separate reviewer invocation and durable Task/Attempt, not proof that different providers/models were configured. Owner must configure trusted wrappers; native execution remains UNENFORCED.

Before model invocation validate the full accepted-plan chain, implementation completion manifest, exact task/attempt/fence and current revision/grant, source Git/data fingerprint, and actual candidate. Create an authority-free Task using CreateTaskWithGuard, original Mission and resource envelope, class `github.issue.code.review`, key `code-review:<implementation Work ID>`. Claim a five-minute lease and allow four minutes for the model. No tests are rerun.

Save the verdict bound to implementation evidence/hash, candidate identity, review Task/Attempt/fence and Case/Work. Atomically index it by review Attempt for interrupted completion recovery. Persistence errors after verdict staging retain the Attempt, avoiding repeated paid review. Expired staged Attempts require operator recovery. Completed review calls reuse exact persisted evidence. Review failure before staging uses bounded existing retry semantics.

Guard completion and acceptance in SQLite against latest active Case/Work/revision/authority and exact current Attempts. Retain the exact accepted triage and plan-review Task/Attempt/fence chain in every guard; upstream challenge blocks completion/acceptance. Guarded acceptance replays an identical saved acceptance inside the same transaction after its guard succeeds; it never falls back to an unguarded historical record. Review Task acceptance validates the verdict binding, independently of its substantive verdict. Implementation Task succeeds only for ACCEPT plus all passing recorded owner validations. REVISE/BLOCK or failed validations produce a held result and leave implementation awaiting verification. They cannot create GitHub operations or silently regenerate code. Retrying the same Work returns the original verdict; new review requires new authorized work.

Observe source and candidate again after model invocation and before applying saved results. These are drift observations under trusted native execution, not an atomic filesystem lock or hostile-wrapper sandbox. Review evidence is immutable after completion; replay validates current canonical bindings and available source/candidate rather than trusting a stale stored success alone. Acceptance cites both implementation and review evidence and the exact review verifier type/id.

## Out of scope

GitHub publication/merge, worker automatic composition, hostile-host containment, actual monetary settlement, automatic revision after rejection and non-Linux runtime certification. No broad issue is closed; related #3, #4, #15.

## Verification

Real Git fixtures and SQLite transitions exercise ACCEPT, REVISE/BLOCK, failing validation, source/candidate drift, new revision/grant revocation, concurrent callers, interrupted completion and acceptance, expired staged lease, replay without model calls and strict CLI protocol. Full suite, affected race tests, vet, build, diff checks and an Astra branch review precede publishing the extension.
