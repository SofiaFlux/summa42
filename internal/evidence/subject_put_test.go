package evidence

import (
	"github.com/SofiaFlux/summa42/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestPutSubjectIsAtomicWithObject(t *testing.T) {
	s := newFindStore(t, testutil.NewClock(time.Now()))
	ctx := t.Context()
	_, err := s.state.DB().ExecContext(ctx, `CREATE TRIGGER reject_subject BEFORE INSERT ON evidence_subjects BEGIN SELECT RAISE(ABORT,'injected subject failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, strings.NewReader("result"), Metadata{Kind: "result", Subject: "attempt-1", MediaType: "text/plain"}); err == nil {
		t.Fatal("subject failure ignored")
	}
	var count int
	s.state.DB().QueryRowContext(ctx, `SELECT count(*) FROM evidence_objects`).Scan(&count)
	if count != 0 {
		t.Fatal("unindexed object escaped transaction")
	}
	s.state.DB().ExecContext(ctx, `DROP TRIGGER reject_subject`)
	obj, err := s.Put(ctx, strings.NewReader("result"), Metadata{Kind: "result", Subject: "attempt-1", MediaType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	found, raw, ok, err := s.FindBySubject(ctx, "result", "attempt-1")
	if err != nil || !ok || found.ID != obj.ID || string(raw) != "result" {
		t.Fatalf("%+v %v %v", found, ok, err)
	}
}
