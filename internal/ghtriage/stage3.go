package ghtriage

// Stage3 is a pure, total function from a validated stage 2 document to a
// disposition and the rule that produced it. The first matching branch wins
// and the vocabulary is covered exhaustively, so this cannot fall through.
// It never returns duplicate: an explicit duplicate label is a stage 1 signal
// that resolves the issue decisively, so the disposition has exactly one
// producer.
func Stage3(out Stage2Output) Stage3Result {
	switch {
	case !out.IsActionable:
		return Stage3Result{Disposition: DispositionNotActionable, Rule: "not-actionable"}
	case out.NeedsRepro:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "needs-repro-required"}
	case out.Scope == ScopeSmall || out.Scope == ScopeMedium:
		return Stage3Result{Disposition: DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"}
	case out.Scope == ScopeLarge:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "scope-large-needs-decomposition"}
	default:
		return Stage3Result{Disposition: DispositionNeedsHuman, Rule: "scope-unknown-needs-scoping"}
	}
}
