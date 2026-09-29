package evidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Metadata struct {
	MediaType string
	Kind      string
}

var (
	ErrEvidenceNotFound = errors.New("evidence not found")
	ErrEvidenceCorrupt  = errors.New("evidence corrupt")
)

type EvidenceObject struct {
	ID          domain.ID
	ContentHash string
	MediaType   string
	Kind        string
	SizeBytes   int64
	CreatedAt   time.Time
}

type Store struct {
	state *state.Store
	root  string
	clock clock.Clock
}

func New(store *state.Store, root string, clk clock.Clock) (*Store, error) {
	if store == nil || clk == nil {
		return nil, errors.New("evidence store requires state store and clock")
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("evidence root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "blobs", "sha256"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(abs, "tmp"), 0o700); err != nil {
		return nil, err
	}
	return &Store{state: store, root: abs, clock: clk}, nil
}

func (s *Store) Put(ctx context.Context, r io.Reader, metadata Metadata) (EvidenceObject, error) {
	if s == nil || s.state == nil || s.clock == nil {
		return EvidenceObject{}, errors.New("evidence store is not configured")
	}
	if r == nil {
		return EvidenceObject{}, errors.New("evidence reader is required")
	}
	metadata.MediaType = strings.TrimSpace(metadata.MediaType)
	metadata.Kind = strings.TrimSpace(metadata.Kind)
	if metadata.MediaType == "" || metadata.Kind == "" {
		return EvidenceObject{}, errors.New("evidence media type and kind are required")
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "evidence-*")
	if err != nil {
		return EvidenceObject{}, err
	}
	tmpPath := tmp.Name()
	keepTemp := true
	defer func() {
		_ = tmp.Close()
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return EvidenceObject{}, err
	}
	if err := tmp.Sync(); err != nil {
		return EvidenceObject{}, err
	}
	if err := tmp.Close(); err != nil {
		return EvidenceObject{}, err
	}

	contentHash := hex.EncodeToString(h.Sum(nil))
	if err := verifyHash(tmpPath, contentHash); err != nil {
		return EvidenceObject{}, fmt.Errorf("verify staged evidence: %w", err)
	}
	blobDir := filepath.Join(s.root, "blobs", "sha256", contentHash[:2])
	if err := os.MkdirAll(blobDir, 0o700); err != nil {
		return EvidenceObject{}, err
	}
	blobPath := filepath.Join(blobDir, contentHash)

	if err := persistBlob(tmpPath, blobPath, contentHash, size); err != nil {
		return EvidenceObject{}, err
	}
	keepTemp = false
	if err := syncDirectory(blobDir); err != nil {
		return EvidenceObject{}, err
	}

	now := s.clock.Now().UTC()
	object := EvidenceObject{
		ID: domain.NewID("evidence"), ContentHash: contentHash, MediaType: metadata.MediaType,
		Kind: metadata.Kind, SizeBytes: size, CreatedAt: now,
	}
	if _, err := s.state.DB().ExecContext(ctx,
		`INSERT INTO evidence_objects(evidence_id, content_hash, media_type, kind, size_bytes, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		object.ID, object.ContentHash, object.MediaType, object.Kind, object.SizeBytes, now.Format(time.RFC3339Nano),
	); err != nil {
		return EvidenceObject{}, err
	}
	return object, nil
}

func (s *Store) FindByContentHash(ctx context.Context, contentHash, kind string) (EvidenceObject, bool, error) {
	if s == nil || s.state == nil {
		return EvidenceObject{}, false, errors.New("evidence store is not configured")
	}
	contentHash = strings.TrimSpace(contentHash)
	kind = strings.TrimSpace(kind)
	if contentHash == "" || kind == "" {
		return EvidenceObject{}, false, errors.New("content hash and kind are required")
	}
	var id domain.ID
	if err := s.state.DB().QueryRowContext(ctx, `
		SELECT evidence_id FROM evidence_objects
		WHERE content_hash = ? AND kind = ?
		ORDER BY created_at, evidence_id LIMIT 1`, contentHash, kind,
	).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EvidenceObject{}, false, nil
		}
		return EvidenceObject{}, false, err
	}
	object, _, err := s.Get(ctx, id)
	if err != nil {
		return EvidenceObject{}, false, err
	}
	return object, true, nil
}

// FindByKind returns the most recent evidence object of a kind that match
// accepts, walking newest first and reading that object's bytes. The evidence
// store has no case or revision column, so a caller that must find a document by
// a business key decodes each candidate and checks the key itself; match
// returning false is how a candidate is passed over. That is not decoration:
// several objects of one mission each write a record of the same kind, so a
// lookup that stopped at the first decodable candidate would hand back another
// object's record and never find its own. A candidate whose bytes do not decode
// is simply one more candidate the caller rejects.
//
// The limit bounds both the query and the number of blobs read, so one lookup
// cannot become unbounded as the store grows.
func (s *Store) FindByKind(ctx context.Context, kind string, limit int, match func(EvidenceObject, []byte) bool) (EvidenceObject, []byte, bool, error) {
	if s == nil || s.state == nil {
		return EvidenceObject{}, nil, false, errors.New("evidence store is not configured")
	}
	if strings.TrimSpace(kind) == "" {
		return EvidenceObject{}, nil, false, errors.New("evidence kind is required")
	}
	if match == nil {
		return EvidenceObject{}, nil, false, errors.New("evidence match is required")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.state.DB().QueryContext(ctx,
		`SELECT evidence_id, content_hash, media_type, kind, size_bytes, created_at
		 FROM evidence_objects WHERE kind = ?
		 ORDER BY created_at DESC, evidence_id DESC LIMIT ?`, kind, limit)
	if err != nil {
		return EvidenceObject{}, nil, false, fmt.Errorf("list evidence of kind %s: %w", kind, err)
	}
	defer rows.Close()
	candidates := make([]EvidenceObject, 0)
	for rows.Next() {
		var object EvidenceObject
		var createdAt string
		if err := rows.Scan(&object.ID, &object.ContentHash, &object.MediaType,
			&object.Kind, &object.SizeBytes, &createdAt); err != nil {
			return EvidenceObject{}, nil, false, fmt.Errorf("scan evidence of kind %s: %w", kind, err)
		}
		object.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return EvidenceObject{}, nil, false, fmt.Errorf("parse evidence created_at %q: %w", createdAt, err)
		}
		candidates = append(candidates, object)
	}
	if err := rows.Err(); err != nil {
		return EvidenceObject{}, nil, false, err
	}
	for _, candidate := range candidates {
		_, raw, err := s.Get(ctx, candidate.ID)
		if err != nil {
			return EvidenceObject{}, nil, false, err
		}
		if match(candidate, raw) {
			return candidate, raw, true, nil
		}
	}
	return EvidenceObject{}, nil, false, nil
}

func (s *Store) Get(ctx context.Context, id domain.ID) (EvidenceObject, []byte, error) {
	if s == nil || s.state == nil {
		return EvidenceObject{}, nil, errors.New("evidence store is not configured")
	}
	var object EvidenceObject
	var createdAt string
	object.ID = id
	if err := s.state.DB().QueryRowContext(ctx,
		`SELECT content_hash, media_type, kind, size_bytes, created_at FROM evidence_objects WHERE evidence_id = ?`, id,
	).Scan(&object.ContentHash, &object.MediaType, &object.Kind, &object.SizeBytes, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EvidenceObject{}, nil, fmt.Errorf("%w: evidence %q not found", ErrEvidenceNotFound, id)
		}
		return EvidenceObject{}, nil, err
	}
	var err error
	if object.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return EvidenceObject{}, nil, fmt.Errorf("%w: parse evidence timestamp: %v", ErrEvidenceCorrupt, err)
	}
	if len(object.ContentHash) != 64 {
		return EvidenceObject{}, nil, fmt.Errorf("%w: evidence %q has invalid content hash length %d", ErrEvidenceCorrupt, id, len(object.ContentHash))
	}
	if _, err := hex.DecodeString(object.ContentHash); err != nil {
		return EvidenceObject{}, nil, fmt.Errorf("%w: evidence %q has invalid content hash: %v", ErrEvidenceCorrupt, id, err)
	}
	path := filepath.Join(s.root, "blobs", "sha256", object.ContentHash[:2], object.ContentHash)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return EvidenceObject{}, nil, fmt.Errorf("%w: evidence %q blob is missing: %v", ErrEvidenceCorrupt, id, err)
		}
		return EvidenceObject{}, nil, fmt.Errorf("read evidence blob: %w", err)
	}
	if err := verifyHash(path, object.ContentHash); err != nil {
		return EvidenceObject{}, nil, fmt.Errorf("verify evidence blob: %w", err)
	}
	return object, data, nil
}

func persistBlob(tempPath, blobPath, expectedHash string, expectedSize int64) error {
	if info, err := os.Stat(blobPath); err == nil {
		if info.Size() != expectedSize {
			return fmt.Errorf("existing evidence blob size mismatch for %s", expectedHash)
		}
		if err := verifyHash(blobPath, expectedHash); err != nil {
			return err
		}
		return os.Remove(tempPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Rename(tempPath, blobPath); err != nil {
		if info, statErr := os.Stat(blobPath); statErr == nil && info.Size() == expectedSize {
			if verifyErr := verifyHash(blobPath, expectedHash); verifyErr == nil {
				_ = os.Remove(tempPath)
				return nil
			}
		}
		return err
	}
	return verifyHash(blobPath, expectedHash)
}

func verifyHash(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return err
	}
	if got := hashString(h); got != expected {
		return fmt.Errorf("%w: evidence blob hash mismatch: got %s want %s", ErrEvidenceCorrupt, got, expected)
	}
	return nil
}

func hashString(h hash.Hash) string {
	return hex.EncodeToString(h.Sum(nil))
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
