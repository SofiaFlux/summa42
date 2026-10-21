package ghtriage

import (
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

func TestDriftCheckFailsOnCanonicalError(t *testing.T) {
	bad := repoworkspace.Snapshot{}
	err := snapshotMatches(bad, bad)
	if err == nil || !strings.Contains(err.Error(), "canonicalize") {
		t.Fatalf("two non-canonicalizable snapshots must fail with a canonicalize error, got %v", err)
	}
}
