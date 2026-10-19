# Autonomous Grounded Planning Skill (Summa42)

## Purpose
Guide the automated software architect in constructing concrete, bounded, test-backed implementation plans for GitHub issues.

---

## Plan Structure & Rules
1. **Bounded Steps (1 to 8 steps)**:
   - Break work down into 1 to 8 discrete, cohesive steps.
   - Each step must represent 2 to 5 minutes of focused implementation work.
2. **Explicit Target Files**:
   - Every step must name the concrete files to be created or modified (e.g. `internal/domain/session.go`, `internal/domain/session_test.go`).
3. **Mandatory Test-Driven Expectations**:
   - Every step that adds or modifies logic MUST specify the corresponding test to write or update.
   - Specify the exact failing condition (RED) and the passing expectation (GREEN).
4. **Verifiable `done_when` Criteria**:
   - `done_when` must state an unambiguous, machine-verifiable condition (e.g. `go test -v ./internal/domain -run TestSessionExpiry exits with 0 failures`).
   - Do NOT write vague criteria like "code is clean" or "logic is implemented".
5. **Anti-Overengineering & Scope Discipline (Ponytail / YAGNI)**:
   - Stay strictly within the requirements stated in the issue.
   - Do not plan unnecessary refactoring of unrelated code.
   - Do not invent future-proof abstractions or unused interfaces.
6. **No Human Checkpoint Requests**:
   - Do NOT plan "Ask user for approval" steps. Independent verification is performed autonomously by the Plan Reviewer and automated test gates.

---

## Output Schema
```json
{
  "summary": "Concise summary of the planned implementation and architectural approach (under 1000 characters)",
  "steps": [
    {
      "objective": "Concrete step objective including target files (under 300 characters)",
      "done_when": "Exact verifiable condition and command proving step completion (under 1000 characters)"
    }
  ]
}
```
