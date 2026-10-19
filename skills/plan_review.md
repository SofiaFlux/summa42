# Autonomous Plan Review Skill (Summa42)

## Purpose
Guide the independent plan reviewer in verifying that a proposed issue implementation plan meets strict engineering and architectural standards before any code is written.

---

## Evaluation Gate Criteria
Evaluate the proposed plan against the issue requirements using the following strict checklist:

1. **Test Coverage Verification**:
   - Does every implementation step specify automated tests to be written or updated?
   - If a plan modifies code without specifying tests, return `REVISE` with the reason "Plan lacks test-driven verification steps".
2. **Concrete & Bounded Steps**:
   - Are the steps concrete, naming specific files and verifiable `done_when` criteria?
   - Are there between 1 and 8 steps? If steps are vague, return `REVISE`.
3. **Anti-Overengineering Check (Ponytail / YAGNI)**:
   - Does the plan introduce unnecessary layers, premature generic abstractions, or out-of-scope refactorings?
   - If over-engineering is detected, return `REVISE` demanding the simplest working design.
4. **Scope & Authorization**:
   - Does the plan stay strictly within the issue requirements and allowed workspace paths?
   - If the plan exceeds authorized scope, return `REVISE` or `BLOCK`.
5. **No Interactive Human Checkpoints**:
   - Does the plan rely on human pauses or approvals? If so, return `REVISE` demanding autonomous machine-verifiable gates.

---

## Output Schema
```json
{
  "verdict": "ACCEPT",
  "reason": "Detailed, objective justification for acceptance or actionable revision requirements (under 1000 characters)"
}
```
*Note: Valid verdicts are `ACCEPT`, `REVISE`, or `BLOCK`.*
