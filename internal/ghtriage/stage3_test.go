package ghtriage

import "testing"

func TestStage3RuleTableIsTotal(t *testing.T) {
	cases := []struct {
		name     string
		out      Stage2Output
		wantDisp Disposition
		wantRule string
	}{
		{"not actionable wins first", Stage2Output{IsActionable: false, NeedsRepro: true, Scope: ScopeSmall}, DispositionNotActionable, "not-actionable"},
		{"needs repro", Stage2Output{IsActionable: true, NeedsRepro: true, Scope: ScopeSmall}, DispositionNeedsHuman, "needs-repro-required"},
		{"small is ready", Stage2Output{IsActionable: true, Scope: ScopeSmall}, DispositionReadyToPlan, "actionable-without-repro-small-or-medium"},
		{"medium is ready", Stage2Output{IsActionable: true, Scope: ScopeMedium}, DispositionReadyToPlan, "actionable-without-repro-small-or-medium"},
		{"large needs decomposition", Stage2Output{IsActionable: true, Scope: ScopeLarge}, DispositionNeedsHuman, "scope-large-needs-decomposition"},
		{"unknown needs scoping", Stage2Output{IsActionable: true, Scope: ScopeUnknown}, DispositionNeedsHuman, "scope-unknown-needs-scoping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Stage3(tc.out)
			if got.Disposition != tc.wantDisp {
				t.Fatalf("disposition = %q, want %q", got.Disposition, tc.wantDisp)
			}
			if got.Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", got.Rule, tc.wantRule)
			}
			if !got.Disposition.valid() {
				t.Fatalf("disposition %q is outside the closed set", got.Disposition)
			}
		})
	}
}

func TestStage3NeverProducesDuplicate(t *testing.T) {
	for _, scope := range []Scope{ScopeSmall, ScopeMedium, ScopeLarge, ScopeUnknown} {
		if got := Stage3(Stage2Output{IsActionable: true, Scope: scope}); got.Disposition == DispositionDuplicate {
			t.Fatalf("stage 3 produced duplicate for scope %q", scope)
		}
	}
}
