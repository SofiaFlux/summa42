package main

import (
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
)

// An object the driver could not advance has to reach the operator as a non-zero
// exit. A permanently stalled case - a supersession with no evidence to record,
// a decision that cannot be decoded - is otherwise a tick that reports success
// on stdout forever, which is the one failure mode a scheduler cannot notice.
func TestGHTriageDriverTickFailureFailsTheCommandOnAPerObjectFailure(t *testing.T) {
	if err := ghtriageDriverTickFailure(ghtriage.DriverResult{Blocked: 1, Superseded: 1}); err != nil {
		t.Fatalf("a tick that only blocked and superseded returned %v, want nil", err)
	}
	err := ghtriageDriverTickFailure(ghtriage.DriverResult{
		Accepted: 1, Failures: []string{"o/r#42: no evidence to record", "o/r#43: read provenance"},
	})
	if err == nil {
		t.Fatal("a tick with per-object failures returned nil")
	}
	for _, want := range []string{"2", "o/r#42: no evidence to record", "o/r#43: read provenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}
