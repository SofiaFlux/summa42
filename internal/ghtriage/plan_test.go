package ghtriage_test

import (
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

func TestParsePlanOutputRequiresAConcreteBoundedPlan(t *testing.T) {
	valid := `{"summary":"Fix the save crash","steps":[{"objective":"Reproduce the crash","done_when":"A test fails on the reported input"},{"objective":"Fix save","done_when":"The regression test passes"}]}`
	got, err := ghtriage.ParsePlanOutput([]byte(valid))
	if err != nil || got.Summary != "Fix the save crash" || len(got.Steps) != 2 {
		t.Fatalf("valid plan = %+v, %v", got, err)
	}
	for _, raw := range []string{
		`{"steps":[{"objective":"Fix","done_when":"Test passes"}]}`,
		`{"summary":"Fix","steps":[]}`,
		`{"summary":"Fix","steps":[{"objective":"","done_when":"Test passes"}]}`,
		`{"summary":"Fix","steps":[{"objective":"Fix","done_when":""}]}`,
		`{"summary":"Fix","steps":[{"objective":"Fix","done_when":"Test passes","command":"rm -rf /"}]}`,
		valid + valid,
	} {
		if _, err := ghtriage.ParsePlanOutput([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid plan %q", raw)
		}
	}
	tooMany := `{"summary":"Fix","steps":[` + strings.Repeat(`{"objective":"Step","done_when":"Done"},`, 8) + `{"objective":"Step","done_when":"Done"}]}`
	if _, err := ghtriage.ParsePlanOutput([]byte(tooMany)); err == nil {
		t.Fatal("accepted a plan with nine steps")
	}
}
