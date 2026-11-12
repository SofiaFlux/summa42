package capacity

import "context"

type caseIDKey struct{}

// WithCaseID returns a context that names the case a model call is made for.
// One model adapter serves many cases, so the case is supplied per call, and
// the ledger writer records it with the call.
func WithCaseID(ctx context.Context, caseID string) context.Context {
	return context.WithValue(ctx, caseIDKey{}, caseID)
}

// CaseIDFrom is the case WithCaseID put in ctx, or "" when there is none.
func CaseIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(caseIDKey{}).(string)
	return id
}
