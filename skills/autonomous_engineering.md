# Autonomous Engineering Principles & Disciplines (Summa42)

Autonomous workers (Summands) operate without human intervention. To ensure sovereign reliability, high code quality, and verifiable outcomes, every cognitive worker must strictly follow these core engineering disciplines.

---

## 1. Test-Driven Development (TDD) — The Iron Law
> **No production code without automated tests.**

1. **Test-First Mindset**: Every new feature, bug fix, or behavioral change must be verified by automated tests (`*_test.go`).
2. **Behavior Over Implementation**: Tests must verify real requirements, boundary conditions, and error paths, not internal implementation trivia.
3. **Reproduce Before Fixing**: A bug fix must include a test that would fail on the unpatched code and passes with the fix.
4. **Never Skip Tests**: An implementation without tests is automatically rejected by independent review.

---

## 2. Verification Before Completion — Evidence Over Assertions
> **Evidence before claims, always.**

1. **Zero Hallucinated Success**: Never claim a task, step, or build passes without executing the verification command (`go test`).
2. **Complete Verification**: Verify exit code 0 and 0 failures. A linter passing does not mean the compiler or tests pass.
3. **No Speculation**: Avoid "should work", "probably passes", or "assumed clean". If it was not verified with evidence, it is incomplete.

---

## 3. Minimalist Engineering & Anti-Overengineering (Ponytail / YAGNI)
> **Build the simplest thing that solves the authorized task.**

1. **YAGNI (You Aren't Gonna Need It)**: Implement only what is directly required by the issue. Do not build speculative features, premature abstractions, or unused generic hooks.
2. **Standard Library First**: Use Go standard library or mature existing dependencies already in `go.mod`. Do not introduce new external dependencies unless strictly mandated.
3. **No Boilerplate Factories**: Avoid wrapper-over-wrapper patterns, unnecessary interfaces for single implementations, or deep inheritance-like hierarchies.
4. **Minimal Diff Size**: The best code is the code you didn't have to write. Smaller, focused diffs are easier to verify and less prone to regressions.
5. **Mark Honest Shortcuts**: If a simplification is made that intentionally limits future scope, add a clear comment naming the ceiling and migration path (`// ponytail: ...`).

---

## 4. Autonomous Non-Blocking Operation (Human Attention Budget)
> **Low-friction autonomous execution protects the Human Attention Budget.**

1. **Zero Interactive Prompts**: Never output prompts asking a human to confirm, choose, or review intermediate micro-steps.
2. **Automated Peer Gates**: Rely on independent automated review stages (`PlanReviewer`, `CodeReviewer`) and deterministic compiler/test gates for acceptance.
3. **Deterministic Verification**: Work is validated by evidence objects, test outputs, and cryptographic content hashes.

---

## 5. Documentation & Architectural Integrity
1. **Preserve Existing Context**: Never delete unrelated comments, docstrings, licenses, or architectural invariants.
2. **Canonical Alignment**: Respect the sovereign Collective / Box / Summand hierarchy, Least Cognitive Mechanism principle, and strict type safety (no untyped shortcuts or unhandled errors).
