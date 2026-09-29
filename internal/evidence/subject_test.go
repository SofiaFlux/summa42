package evidence

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/testutil"
)

func TestSubjectLookupIsScopedAndImmutable(t *testing.T) {
	ctx := context.Background()
	store := newFindStore(t, testutil.NewClock(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)))
	first, err := store.Put(ctx, strings.NewReader("first"), Metadata{MediaType: "text/plain", Kind: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(ctx, strings.NewReader("second"), Metadata{MediaType: "text/plain", Kind: "accepted"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LinkSubject(ctx, "accepted", "case-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.LinkSubject(ctx, "accepted", "case-2", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.LinkSubject(ctx, "accepted", "case-1", first.ID); err != nil {
		t.Fatalf("idempotent link: %v", err)
	}
	if err := store.LinkSubject(ctx, "accepted", "case-1", second.ID); err == nil {
		t.Fatal("conflicting subject link accepted")
	}

	for _, tc := range []struct{ kind, subject, want string }{
		{"accepted", "case-1", "first"},
		{"accepted", "case-2", "second"},
		{"other", "case-1", ""},
		{"accepted", "case-3", ""},
	} {
		_, raw, found, err := store.FindBySubject(ctx, tc.kind, tc.subject)
		if err != nil {
			t.Fatal(err)
		}
		if found != (tc.want != "") || string(raw) != tc.want {
			t.Fatalf("FindBySubject(%q, %q) = %q, %v; want %q", tc.kind, tc.subject, raw, found, tc.want)
		}
	}
}

func TestSubjectRejectsWrongEvidenceKind(t *testing.T) {
	ctx := context.Background()
	store := newFindStore(t, testutil.NewClock(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)))
	object, err := store.Put(ctx, strings.NewReader("wrong"), Metadata{MediaType: "text/plain", Kind: "decision"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.LinkSubject(ctx, "accepted", "case-1", object.ID); err == nil {
		t.Fatal("linked an accepted subject to a decision object")
	}
}
