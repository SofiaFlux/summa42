# Supervised GitHub candidate publication

Owner goal: continue Summa42 self-development functionality. Today Summa42 must not participate in actual development/publication. Implement and test the pipeline ourselves, with Astra review. Existing PR #20 stays draft, no merge.

## Scope and chosen architecture

After independent code acceptance, a supervised `run-gh-publish` command publishes an exact candidate into an immutable managed branch and a draft PR. It requires owner configuration of source repository, base branch, credentials and explicit current Case grant/actions for `github.repo.publish` and `github.pr.create`. Implementation grants alone never permit publication. No model chooses remote, branch, permissions, title or marker. Case remains on implementation Work; a separate publication Task shares Mission/resource envelope.

Use two existing protected ExternalOperation slots: candidate branch and draft PR. This keeps a confirmed branch independent of an uncertain PR acknowledgement. Direct SDK writes outside Operations, or one composite branch+PR slot, were rejected because they bypass policy/economics or conflate separately recoverable effects.

## Exact bytes and identity

Export only changed regular UTF-8 file contents/modes/deletions from the immutable candidate commit. At most 64 paths and 256 KiB new file contents. Carry base/candidate/tree SHAs, source base tree, exact author/committer/date and message. Support only unsigned ordinary single-parent commits representable by the GitHub Git database REST API; reject unsupported headers instead of silently rewriting identity. Reconstruct and hash the exact commit representation locally. GitHub-created tree and commit SHAs must match the accepted candidate before a ref is created. Preserve executable modes and deletions. Never push local credentials or working files.

Managed branch is `summa42/issue-<number>-<SHA256(work ID) first 16 hex>` and never force-updated. Existing exact branch can be confirmed; an existing different SHA fails closed. Owner-selected base branch must still identify pinned base SHA before either operation. PR is always draft and cites issue and immutable evidence without auto-closing issues. Marker is deterministic per Work/candidate. PR lookup paginates all states and matches exact marker, repository/head/base identity, expected SHAs, title/body and draft flag. Missing, conflicting or ambiguous results stay unknown; no blind duplicate write.

## Authority and recovery

Load the exact accepted triage → plan → plan-review → implementation → code-review chain, completion manifests and acceptances, including current Task/Attempt/fence. Observe local source/candidate before every new dispatch. Add optional transactional guards to Operations preparation and dispatch so current Case/Work/revision/grant and accepted upstream states are checked in the same canonical transaction as reservation/commit. Existing unguarded callers keep their behavior. External dispatch committed before revocation can still finish; filesystem/remote changes cannot be atomically locked by SQLite.

Claim publication Task before effects. Ten-minute lease, nine-minute run timeout. Policy-required approvals and resource reservations remain authoritative; no approvals are fabricated. HTTP provider uses credentials only inside transport, HTTPS except explicit loopback test endpoint, disabled redirects, bounded 1 MiB response, sanitized errors, 20-second requests. Providers are mediated/enforced effects, not a hostile-host execution sandbox.

Branch adapter creates content-addressed tree/commit then ref, with read-back. PR adapter reads exact branch/base, checks for existing marker before one create, and validates response/read-back. Provider errors or inconclusive writes follow existing OUTCOME_UNKNOWN reconciliation. Lookup is read-only. Unreferenced content-addressed objects are acceptable partial preparation; absent refs/PRs do not prove a delayed write can never complete. Unknown outcomes never trigger a blind replacement effect.

Confirmed results stage a receipt indexed by publication Attempt before guarded Task completion. Receipt binds implementation/review evidence, candidate, destination, Task/Attempt/fence and operation IDs/states/references. Interrupted completion resumes the same live Attempt; expired staged receipts require operator recovery. Completed replay returns a historical publication receipt under current canonical guards, not a promise that nobody later edited/deleted a remote PR. Publication completion remains awaiting verification; no CI acceptance, merge or issue closure. Busy/pending/held states are explicit JSON and pending/held yields nonzero CLI status.

## Verification and boundaries

Use real Git/SQLite and deterministic HTTP RoundTripper fixtures; do not use Summa to publish anything live today. Test exact commit identity, modes/deletions, unsupported export, mismatched remote tree/commit, no ref overwrite, pagination/ambiguous marker, lost acknowledgement read-only recovery, no redirect/token leak, missing grant/acceptance, upstream challenge and revocation before dispatch, concurrent claim and staged completion recovery. Full available suite, race, vet/build/diff and Astra branch review precede extension publication via our GitHub connector.

No autonomous worker composition, automatic repair after rejected review, GitHub CI/merge management, live provider certification, actual monetary currency settlement or non-Linux certification. Related #3, #4, #15; no broad issue is closed.

Primary API references: https://docs.github.com/en/rest/git/commits ; https://docs.github.com/en/rest/git/trees ; https://docs.github.com/en/rest/git/refs ; https://docs.github.com/en/rest/pulls/pulls .
