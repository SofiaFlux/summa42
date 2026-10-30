package ghtriage

import "testing"

func TestPlanPathOutsideContract(t *testing.T) {
	allowed := []string{"internal/a/x.go", "internal/a/x_test.go", "internal/ctx/y.go"}
	cases := []struct {
		name    string
		plan    PlanOutput
		outside string
	}{
		{"allowed path in backticks", PlanOutput{Summary: "Edit `internal/a/x.go`.", Steps: []PlanStep{{Objective: "Fix", DoneWhen: "x_test.go passes"}}}, ""},
		{"context file is named", PlanOutput{Summary: "Read internal/ctx/y.go first", Steps: []PlanStep{{Objective: "Fix", DoneWhen: "Done"}}}, ""},
		{"URL is ignored", PlanOutput{Summary: "See https://example.com/docs/page.html", Steps: []PlanStep{{Objective: "Fix", DoneWhen: "Done"}}}, ""},
		{"slash-joined file list is not one path", PlanOutput{Summary: "Tests in engine_test.go/idempotency_test.go", Steps: []PlanStep{{Objective: "Fix", DoneWhen: "Done"}}}, ""},
		{"path outside contract", PlanOutput{Summary: "Fix", Steps: []PlanStep{{Objective: "Touch internal/b/z.go", DoneWhen: "Done"}}}, "internal/b/z.go"},
	}
	for _, tc := range cases {
		got, outside := tc.plan.pathOutside(allowed)
		if (outside && got != tc.outside) || (!outside && tc.outside != "") {
			t.Fatalf("%s: pathOutside = %q, %v; want %q", tc.name, got, outside, tc.outside)
		}
	}
}
