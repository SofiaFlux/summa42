package ghtriage

import (
	"testing"
	"time"
)

func TestImplementationLeaseCoversModelCallAndValidation(t *testing.T) {
	lease := ImplementationLease(30*time.Minute, 3, 10*time.Minute)
	if need := 30*time.Minute + 3*(10*time.Minute+validationKillGrace) + archiveStreamingCap; lease <= need {
		t.Fatalf("lease %s does not cover model call plus validation %s", lease, need)
	}
	if got := ImplementationLease(0, 0, 0); got != ImplementationLeaseDuration {
		t.Fatalf("unset timeouts: %s", got)
	}
	if ImplementationLease(30*time.Minute, MaxValidationCommands, 10*time.Minute) < lease {
		t.Fatal("the largest valid plan gets a shorter lease")
	}
}
