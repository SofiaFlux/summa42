package ghtriage

import (
	"strings"
	"testing"
)

// The model only sees the question: it must name the fields and the length
// limits PlanOutput.Validate enforces, or long answers fail every pass.
func TestPlanQuestionNamesFieldsAndLimits(t *testing.T) {
	for _, want := range []string{`"summary"`, `"steps"`, `"objective"`, `"done_when"`, "at most 300 characters", "at most 1000 characters", "1 to 8 steps"} {
		if !strings.Contains(PlanQuestion, want) {
			t.Errorf("PlanQuestion lacks %q", want)
		}
	}
	if strings.HasSuffix(PlanQuestion, `"`) {
		t.Error("PlanQuestion ends with a stray quote")
	}
	long := PlanOutput{Summary: "s", Steps: []PlanStep{{Objective: strings.Repeat("o", 301), DoneWhen: "d"}}}
	if long.Validate() == nil {
		t.Fatal("the 300-character objective limit the question names is not enforced")
	}
}
