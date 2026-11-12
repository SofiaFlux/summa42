// Package backup copies a Collective's state to a directory, checks such a
// copy, and restores it (development plan, card M3-T4, local part).
//
// A backup holds the database (a consistent snapshot taken with VACUUM INTO,
// so the Box may keep running), the evidence blobs the snapshot refers to, a
// manifest of hashes, and the Owner's signature over the manifest.
//
// A backup never holds keys, config.json (it has the control token), tokens,
// webhook or credential files: nothing but the database and the evidence
// blobs is ever read. The Owner keeps keys separately.
//
// Restore verifies the Owner's signature first, with a public key the Owner
// gives: a restore from a tampered copy could otherwise bring back forged
// approvals. Restore only writes into an empty home and leaves the restored
// Collective FROZEN, so nothing resumes, and nothing is dispatched from
// restored state, before the Owner has looked at the operations whose outcome
// was open at backup time and resumed it.
package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

const (
	Schema        = "summa42.backup.v1"
	ManifestName  = "manifest.json"
	SignatureName = "manifest.sig"
	DatabaseName  = "state.db"
	evidencePath  = "evidence/blobs/sha256"
)

// ErrSignature is returned when the manifest is not signed by the given Owner key.
var ErrSignature = errors.New("backup manifest is not signed by the given Owner key")

// Signer signs the manifest. identity.LocalEd25519 satisfies it.
type Signer interface {
	PrincipalID() domain.ID
	Sign(message []byte) ([]byte, error)
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	Schema           string    `json:"schema"`
	CreatedAt        time.Time `json:"created_at"`
	CollectiveID     string    `json:"collective_id"`
	OwnerPrincipalID string    `json:"owner_principal_id"`
	Files            []File    `json:"files"`
}

func emptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, 0o700)
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%s is not empty", path)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// copyFile copies src to dst (created private, never overwritten) and
// returns the hash and size of what was written.
func copyFile(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

// openSnapshot opens a database file for reading only; nothing is written to
// it, not even a journal.
func openSnapshot(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", "file:"+filepath.ToSlash(abs)+"?mode=ro&immutable=1")
}

func evidenceHashes(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT content_hash FROM evidence_objects ORDER BY content_hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			// Not a stored blob (some rows only cite a hash); nothing to copy.
			continue
		}
		hashes = append(hashes, hash)
	}
	return hashes, rows.Err()
}

func collectiveOf(ctx context.Context, db *sql.DB) (collectiveID, ownerID string, publicKey []byte, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT m.collective_id, m.owner_principal_id, p.public_key
		FROM collective_metadata m JOIN principals p ON p.principal_id = m.owner_principal_id
		WHERE m.singleton = 1`).Scan(&collectiveID, &ownerID, &publicKey)
	return
}

// Create writes a backup of the store and its evidence into dir, which must
// be empty or absent, and signs the manifest with the Owner key.
func Create(ctx context.Context, store *state.Store, evidenceRoot, dir string, signer Signer, clk clock.Clock) (Manifest, error) {
	if store == nil || signer == nil || clk == nil {
		return Manifest{}, errors.New("backup needs the store, the Owner signer and a clock")
	}
	collectiveID, ownerID, publicKey, err := collectiveOf(ctx, store.DB())
	if err != nil {
		return Manifest{}, fmt.Errorf("load the Collective's Owner: %w", err)
	}
	if signer.PrincipalID() != domain.ID(ownerID) {
		return Manifest{}, errors.New("a backup is signed with the Collective's Owner key")
	}
	if err := emptyDirectory(dir); err != nil {
		return Manifest{}, fmt.Errorf("backup directory: %w", err)
	}
	snapshot := filepath.Join(dir, DatabaseName)
	// VACUUM INTO writes a consistent copy while other connections keep
	// writing; it takes the path as a string literal.
	if _, err := store.DB().ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(snapshot, `'`, `''`)+`'`); err != nil {
		return Manifest{}, fmt.Errorf("snapshot the database: %w", err)
	}
	if err := os.Chmod(snapshot, 0o600); err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{Schema: Schema, CreatedAt: clk.Now().UTC(), CollectiveID: collectiveID, OwnerPrincipalID: ownerID}
	hash, size, err := hashFile(snapshot)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Files = append(manifest.Files, File{Path: DatabaseName, SHA256: hash, Size: size})

	// The blobs are the ones the snapshot refers to, not whatever the live
	// database refers to now: the two must match each other.
	db, err := openSnapshot(snapshot)
	if err != nil {
		return Manifest{}, err
	}
	hashes, err := evidenceHashes(ctx, db)
	db.Close()
	if err != nil {
		return Manifest{}, fmt.Errorf("read evidence of the snapshot: %w", err)
	}
	for _, blob := range hashes {
		relative := evidencePath + "/" + blob[:2] + "/" + blob
		source := filepath.Join(evidenceRoot, "blobs", "sha256", blob[:2], blob)
		written, size, err := copyFile(source, filepath.Join(dir, filepath.FromSlash(relative)))
		if err != nil {
			return Manifest{}, fmt.Errorf("copy evidence %s: %w", blob, err)
		}
		if written != blob {
			return Manifest{}, fmt.Errorf("evidence blob %s is corrupt in the live store (it hashes to %s); the backup was not completed", blob, written)
		}
		manifest.Files = append(manifest.Files, File{Path: relative, SHA256: written, Size: size})
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	raw = append(raw, '\n')
	signature, err := signer.Sign(raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("sign the manifest: %w", err)
	}
	if !ed25519.Verify(publicKey, raw, signature) {
		return Manifest{}, errors.New("the signing key is not the Owner key recorded in the Collective")
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), raw, 0o600); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, SignatureName), []byte(hex.EncodeToString(signature)+"\n"), 0o600); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Verify checks a backup directory: the Owner's signature over the manifest,
// every file against the manifest (nothing missing, nothing extra, nothing
// changed), the database's own integrity, that it belongs to that Owner, and
// that every evidence blob it refers to is there.
func Verify(ctx context.Context, dir string, ownerPublicKey ed25519.PublicKey) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	signatureText, err := os.ReadFile(filepath.Join(dir, SignatureName))
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrSignature, err)
	}
	signature, err := hex.DecodeString(strings.TrimSpace(string(signatureText)))
	if err != nil || len(ownerPublicKey) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize || !ed25519.Verify(ownerPublicKey, raw, signature) {
		return Manifest{}, ErrSignature
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Schema != Schema {
		return Manifest{}, fmt.Errorf("unknown backup schema %q", manifest.Schema)
	}
	listed := map[string]File{}
	for _, file := range manifest.Files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
		if clean != file.Path || strings.HasPrefix(clean, "../") || filepath.IsAbs(file.Path) || (file.Path != DatabaseName && !strings.HasPrefix(file.Path, evidencePath+"/")) {
			return Manifest{}, fmt.Errorf("manifest lists a path outside the backup layout: %q", file.Path)
		}
		if _, twice := listed[file.Path]; twice {
			return Manifest{}, fmt.Errorf("manifest lists %s twice", file.Path)
		}
		listed[file.Path] = file
	}
	if _, ok := listed[DatabaseName]; !ok {
		return Manifest{}, errors.New("manifest does not list the database")
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(dir, path)
		relative = filepath.ToSlash(relative)
		if entry.IsDir() || relative == ManifestName || relative == SignatureName {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", relative)
		}
		want, ok := listed[relative]
		if !ok {
			return fmt.Errorf("%s is not in the manifest", relative)
		}
		hash, size, err := hashFile(path)
		if err != nil {
			return err
		}
		if hash != want.SHA256 || size != want.Size {
			return fmt.Errorf("%s differs from the manifest", relative)
		}
		seen[relative] = true
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	for path := range listed {
		if !seen[path] {
			return Manifest{}, fmt.Errorf("%s is in the manifest but missing", path)
		}
	}

	db, err := openSnapshot(filepath.Join(dir, DatabaseName))
	if err != nil {
		return Manifest{}, err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return Manifest{}, fmt.Errorf("database integrity check: %q %v", integrity, err)
	}
	collectiveID, ownerID, recordedKey, err := collectiveOf(ctx, db)
	if err != nil {
		return Manifest{}, fmt.Errorf("read the backup's Collective: %w", err)
	}
	if collectiveID != manifest.CollectiveID || ownerID != manifest.OwnerPrincipalID || !ed25519.PublicKey(recordedKey).Equal(ownerPublicKey) {
		return Manifest{}, errors.New("the database belongs to another Collective or Owner than the manifest and key say")
	}
	hashes, err := evidenceHashes(ctx, db)
	if err != nil {
		return Manifest{}, err
	}
	for _, blob := range hashes {
		if !seen[evidencePath+"/"+blob[:2]+"/"+blob] {
			return Manifest{}, fmt.Errorf("evidence blob %s is referred to by the database but not in the backup", blob)
		}
	}
	return manifest, nil
}

// OpenOperation is an external operation whose outcome was not settled when
// the backup was taken.
type OpenOperation struct {
	ID    string `json:"operation_id"`
	State string `json:"state"`
}

type RestoreResult struct {
	Manifest       Manifest        `json:"manifest"`
	DatabasePath   string          `json:"database_path"`
	EvidencePath   string          `json:"evidence_path"`
	OpenOperations []OpenOperation `json:"open_operations"`
}

// Restore verifies the backup and writes it into home, which must be empty
// or absent: the database to state/summa42.db and the evidence to evidence/.
// The restored Collective is left FROZEN. Keys and config.json are not part
// of a backup; the Owner puts them back.
func Restore(ctx context.Context, dir, home string, ownerPublicKey ed25519.PublicKey, clk clock.Clock) (RestoreResult, error) {
	manifest, err := Verify(ctx, dir, ownerPublicKey)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := emptyDirectory(home); err != nil {
		return RestoreResult{}, fmt.Errorf("restore target: %w", err)
	}
	result := RestoreResult{Manifest: manifest, DatabasePath: filepath.Join(home, "state", "summa42.db"), EvidencePath: filepath.Join(home, "evidence"), OpenOperations: []OpenOperation{}}
	for _, file := range manifest.Files {
		target := result.DatabasePath
		if file.Path != DatabaseName {
			target = filepath.Join(result.EvidencePath, filepath.FromSlash(strings.TrimPrefix(file.Path, "evidence/")))
		}
		hash, _, err := copyFile(filepath.Join(dir, filepath.FromSlash(file.Path)), target)
		if err != nil {
			return RestoreResult{}, fmt.Errorf("restore %s: %w", file.Path, err)
		}
		if hash != file.SHA256 {
			return RestoreResult{}, fmt.Errorf("%s changed while it was restored", file.Path)
		}
	}
	for _, sub := range []string{"tmp", filepath.Join("blobs", "quarantine"), filepath.Join("blobs", "sha256")} {
		if err := os.MkdirAll(filepath.Join(result.EvidencePath, sub), 0o700); err != nil {
			return RestoreResult{}, err
		}
	}

	// Open the restored database (this applies migrations a newer binary
	// brings) and freeze it before anything else can use it.
	store, err := state.Open(ctx, result.DatabasePath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("open the restored database: %w", err)
	}
	defer store.DB().Close()
	stamp := domain.FormatTime(clk.Now())
	reason := "restored from the backup of " + manifest.CreatedAt.UTC().Format(time.RFC3339) + "; check open operations, then resume"
	err = store.WithTx(ctx, func(tx *sql.Tx) error {
		updated, err := tx.ExecContext(ctx, `UPDATE emergency_controls SET break_glass_state = 'FROZEN', frozen_at = ?, frozen_by_principal_id = ?, freeze_reason = ?, updated_at = ? WHERE singleton = 1`,
			stamp, manifest.OwnerPrincipalID, reason, stamp)
		if err != nil {
			return err
		}
		if n, _ := updated.RowsAffected(); n == 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO emergency_controls (singleton, break_glass_state, frozen_at, frozen_by_principal_id, freeze_reason, updated_at) VALUES (1, 'FROZEN', ?, ?, ?, ?)`,
				stamp, manifest.OwnerPrincipalID, reason, stamp)
		}
		return err
	})
	if err != nil {
		return RestoreResult{}, fmt.Errorf("freeze the restored Collective: %w", err)
	}
	rows, err := store.DB().QueryContext(ctx, `SELECT operation_id, state FROM external_operations WHERE state IN ('PREPARED', 'DISPATCHED', 'OUTCOME_UNKNOWN') ORDER BY operation_id`)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("list open operations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var operation OpenOperation
		if err := rows.Scan(&operation.ID, &operation.State); err != nil {
			return RestoreResult{}, err
		}
		result.OpenOperations = append(result.OpenOperations, operation)
	}
	return result, rows.Err()
}
