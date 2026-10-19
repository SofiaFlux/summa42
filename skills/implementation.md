# Autonomous Implementation Skill (Summa42)

## Purpose
Guide the automated software engineer in generating exact, verified code edits that fulfill an accepted implementation plan.

---

## Implementation Rules & Disciplines
1. **Test-Driven Delivery (The Iron Law)**:
   - For every package modified or created, provide corresponding automated tests (`*_test.go`).
   - Tests must cover core functionality, boundary conditions, and error cases.
   - Code submitted without tests will be rejected by independent review.
2. **Minimal Diffs (Ponytail / YAGNI)**:
   - Modify only files required by the accepted plan and authorized paths.
   - Build the simplest solution that works cleanly. No speculative helper functions, unnecessary wrappers, or premature generalizations.
3. **Robust Error Handling**:
   - In Go, never ignore returned errors (`err != nil`).
   - Wrap errors with informative context using `fmt.Errorf("...: %w", err)`.
   - Never use `panic` or untyped shortcuts in production paths.
4. **Preserve Documentation & Comments**:
   - Preserve all existing copyright headers, license notices, package comments, docstrings, and architectural comments.
   - Add concise, meaningful docstrings for newly exported types and functions.
5. **Exact Complete File Content**:
   - Each edit entry must provide the complete, valid new content of the target file.
   - Ensure syntactically valid Go code that compiles cleanly with `go build` and passes `go test`.

---

## Output Schema
```json
{
  "summary": "Summary of implemented changes and verification evidence (under 1000 characters)",
  "edits": [
    {
      "path": "internal/example/service.go",
      "content": "package example\n\n...",
      "delete": false
    }
  ]
}
```
