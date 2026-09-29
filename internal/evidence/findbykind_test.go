package evidence

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

type candidateDoc struct {
	Key  string `json:"key"`
	Note string `json:"note"`
}

func newFindStore(t *testing.T, clk *testutil.Clock) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DB().Close() })
	evidenceStore, err := New(store, filepath.Join(dir, "evidence"), clk)
	if err != nil {
		t.Fatal(err)
	}
	return evidenceStore
}

// putDoc stores one document and advances the clock, so a caller's ordering is
// created_at order rather than an arbitrary ID tiebreak.
func putDoc(t *testing.T, store *Store, clk *testutil.Clock, kind, key string) EvidenceObject {
	t.Helper()
	raw, err := json.Marshal(candidateDoc{Key: key, Note: key + " note"})
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(context.Background(), strings.NewReader(string(raw)), Metadata{MediaType: "text/plain", Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	return object
}

// keyMatch decodes each candidate and accepts the one carrying want. It is the
// decode-and-continue path in its production shape: a candidate whose bytes do
// not decode, or that belongs to another case, is rejected and the walk goes on.
func keyMatch(want string, seen *[]string) func(EvidenceObject, []byte) bool {
	return func(_ EvidenceObject, raw []byte) bool {
		var doc candidateDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			*seen = append(*seen, "undecodable")
			return false
		}
		*seen = append(*seen, doc.Key)
		return doc.Key == want
	}
}

func TestFindByKindWalksCandidatesNewestFirst(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC))
	store := newFindStore(t, clk)
	putDoc(t, store, clk, "doc", "a")
	putDoc(t, store, clk, "doc", "b")
	newest := putDoc(t, store, clk, "doc", "c")
	putDoc(t, store, clk, "other", "d")

	var seen []string
	object, raw, found, err := store.FindByKind(ctx, "doc", 10, keyMatch("c", &seen))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("newest candidate was not found")
	}
	if object.ID != newest.ID {
		t.Fatalf("found %s, want the newest %s", object.ID, newest.ID)
	}
	if object.Kind != "doc" {
		t.Fatalf("found kind %q, want doc", object.Kind)
	}
	var got candidateDoc
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Key != "c" || got.Note != "c note" {
		t.Fatalf("bytes = %+v, want the candidate's own bytes", got)
	}
	if strings.Join(seen, ",") != "c" {
		t.Fatalf("candidates offered to match = %v, want the newest first and no others", seen)
	}
}

// The store has no case column, so the caller's identity check is the only thing
// that can tell its record from another case's. A store that returned the first
// candidate it could decode would hand back the wrong record and report not
// found for the caller's own, which is why the walk continues on a rejection.
func TestFindByKindPassesOverRejectedAndUndecodableCandidates(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC))
	store := newFindStore(t, clk)
	putDoc(t, store, clk, "doc", "mine")
	putDoc(t, store, clk, "doc", "theirs")
	if _, err := store.Put(ctx, strings.NewReader("not json at all"), Metadata{MediaType: "text/plain", Kind: "doc"}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)

	var seen []string
	object, _, found, err := store.FindByKind(ctx, "doc", 10, keyMatch("mine", &seen))
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the caller's own record was not found behind another case's and an undecodable blob")
	}
	if got := strings.Join(seen, ","); got != "undecodable,theirs,mine" {
		t.Fatalf("candidates offered to match = %s, want every candidate walked newest first", got)
	}
	if object.Kind != "doc" {
		t.Fatalf("found kind %q, want doc", object.Kind)
	}
}

func TestFindByKindReportsNoMatchWhenEveryCandidateIsRejected(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC))
	store := newFindStore(t, clk)
	putDoc(t, store, clk, "doc", "a")
	putDoc(t, store, clk, "doc", "b")

	var seen []string
	object, raw, found, err := store.FindByKind(ctx, "doc", 10, keyMatch("nobody", &seen))
	if err != nil {
		t.Fatal(err)
	}
	if found || object.ID != "" || raw != nil {
		t.Fatalf("found = %v with object %+v and %d bytes, want no match at all", found, object, len(raw))
	}
	if len(seen) != 2 {
		t.Fatalf("candidates offered to match = %v, want both walked", seen)
	}
}

func TestFindByKindBoundsTheCandidatesItWalks(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC))
	store := newFindStore(t, clk)
	const candidates = 101
	for i := 0; i < candidates; i++ {
		putDoc(t, store, clk, "doc", "key")
	}

	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "explicit limit", limit: 2, want: 2},
		{name: "zero falls back to the default", limit: 0, want: 100},
		{name: "negative falls back to the default", limit: -5, want: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := 0
			_, _, found, err := store.FindByKind(ctx, "doc", tc.limit, func(EvidenceObject, []byte) bool {
				seen++
				return false
			})
			if err != nil {
				t.Fatal(err)
			}
			if found {
				t.Fatal("a match that never returns true was reported as found")
			}
			if seen != tc.want {
				t.Fatalf("candidates offered to match = %d, want %d", seen, tc.want)
			}
		})
	}
}

func TestFindByKindRejectsABlankKindOrAMissingMatch(t *testing.T) {
	ctx := context.Background()
	clk := testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC))
	store := newFindStore(t, clk)
	putDoc(t, store, clk, "doc", "a")

	for _, kind := range []string{"", "   "} {
		if _, _, _, err := store.FindByKind(ctx, kind, 10, func(EvidenceObject, []byte) bool { return true }); err == nil {
			t.Fatalf("kind %q was accepted", kind)
		}
	}
	if _, _, _, err := store.FindByKind(ctx, "doc", 10, nil); err == nil {
		t.Fatal("a nil match was accepted")
	}
}

func TestFindByKindOnAnUnconfiguredStore(t *testing.T) {
	ctx := context.Background()
	never := func(EvidenceObject, []byte) bool { t.Fatal("match ran without a store"); return false }

	var missing *Store
	if _, _, _, err := missing.FindByKind(ctx, "doc", 10, never); err == nil {
		t.Fatal("a nil store was accepted")
	}
	unconfigured := &Store{}
	if _, _, _, err := unconfigured.FindByKind(ctx, "doc", 10, never); err == nil {
		t.Fatal("a store with no state store was accepted")
	}
}
