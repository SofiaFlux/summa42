# Autonomous Code Review Skill (Summa42)

## Purpose
Guide the independent code reviewer in rigorously verifying that an implementation candidate meets all technical, test, and quality criteria before publication or merge.

---

## Evaluation Gate Criteria
Evaluate the code changes against the accepted plan and repository standards using the following checklist:

1. **Automated Test Presence & Coverage (Iron Law)**:
   - Does the diff include automated tests (`*_test.go`) covering the newly added or modified logic?
   - If tests are missing or trivial/assert-nothing, return `REVISE` with the reason "Implementation lacks adequate automated tests".
2. **Verification Before Completion**:
   - Do all validation commands and automated tests pass with 0 errors and 0 failures?
   - Any failing test is an immediate `REVISE`.
3. **Anti-Overengineering & Simplicity (Ponytail / YAGNI)**:
   - Is the diff focused and minimal?
   - Are there unused variables, dead functions, unneeded wrapper interfaces, or speculative abstractions?
   - If unnecessary complexity is added, return `REVISE`.
4. **Code Quality & Error Handling**:
   - Are all errors handled explicitly without ignoring `err`?
   - Are all types strictly typed without arbitrary `any` or untyped assertions?
   - Are concurrent operations race-free?
5. **Preservation of Comments & Invariants**:
   - Are existing comments, docstrings, licenses, and architectural invariants fully preserved?

---

## Output Schema
```json
{
  "verdict": "ACCEPT",
  "reason": "Detailed, objective justification for acceptance or actionable revision requirements (under 1000 characters)"
}
```
*Note: Valid verdicts are `ACCEPT`, `REVISE`, or `BLOCK`.*
