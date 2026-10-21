package evidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
)

func newIntegrityStore(t *testing.T) (*Store, *state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DB().Close() })
	root := filepath.Join(dir, "evidence")
	s, err := New(st, root, testutil.NewClock(time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatal(err)
	}
	return s, st, root
}

func TestGetVerifiesReturnedBytes(t *testing.T) {
	ctx := context.Background()
	s, st, root := newIntegrityStore(t)
	obj, err := s.Put(ctx, strings.NewReader("payload"), Metadata{MediaType: "text/plain", Kind: "K"})
	if err != nil {
		t.Fatal(err)
	}
	// size_bytes disagreeing with the stored bytes is corruption.
	if _, err := st.DB().ExecContext(ctx, `UPDATE evidence_objects SET size_bytes = 999 WHERE evidence_id = ?`, obj.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, obj.ID); !errors.Is(err, ErrEvidenceCorrupt) {
		t.Fatalf("size mismatch: err = %v, want ErrEvidenceCorrupt", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE evidence_objects SET size_bytes = ? WHERE evidence_id = ?`, obj.SizeBytes, obj.ID); err != nil {
		t.Fatal(err)
	}
	// Same-length tampering is caught by the hash of the returned bytes.
	blob := filepath.Join(root, "blobs", "sha256", obj.ContentHash[:2], obj.ContentHash)
	if err := os.WriteFile(blob, []byte("PAYLOAD"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(ctx, obj.ID); !errors.Is(err, ErrEvidenceCorrupt) {
		t.Fatalf("hash mismatch: err = %v, want ErrEvidenceCorrupt", err)
	}
}

func TestCorruptBlobIsReplaced(t *testing.T) {
	ctx := context.Background()
	s, _, root := newIntegrityStore(t)
	obj, err := s.Put(ctx, strings.NewReader("payload"), Metadata{MediaType: "text/plain", Kind: "K"})
	if err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(root, "blobs", "sha256", obj.ContentHash[:2], obj.ContentHash)
	if err := os.WriteFile(blob, []byte("garbage!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := s.Put(ctx, strings.NewReader("payload"), Metadata{MediaType: "text/plain", Kind: "K"})
	if err != nil {
		t.Fatalf("re-put over a corrupt blob must succeed: %v", err)
	}
	_, data, err := s.Get(ctx, again.ID)
	if err != nil || string(data) != "payload" {
		t.Fatalf("Get after replace = %q, %v", data, err)
	}
	quarantined, err := os.ReadDir(filepath.Join(root, "blobs", "quarantine"))
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("quarantine = %v, %v; want exactly one corrupt blob", quarantined, err)
	}
}

func TestOpenSweepsStaleTmp(t *testing.T) {
	s, st, root := newIntegrityStore(t)
	_ = s
	stale := filepath.Join(root, "tmp", "evidence-stale")
	fresh := filepath.Join(root, "tmp", "evidence-fresh")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := New(st, root, testutil.NewClock(time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale tmp file survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh tmp file must be kept (may belong to a live Put): %v", err)
	}
}
