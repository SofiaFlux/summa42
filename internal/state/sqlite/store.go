package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	_ "modernc.org/sqlite"
)

const policyProfileInactiveMarker = "SUMMA42_POLICY_PROFILE_INACTIVE"

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	dsn, err := localFileDSN(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("create sqlite parent directory: %w", err)
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection per process serializes this process only; other processes
	// are kept correct by BEGIN IMMEDIATE transactions and guards, not by this.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Store{db: db}, nil
}

func localFileDSN(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("sqlite path must not be empty")
	}
	if path == ":memory:" || strings.HasPrefix(path, "file:") {
		return "", errors.New("sqlite path must be a local filesystem path")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashPath := filepath.ToSlash(abs)
	if filepath.VolumeName(abs) != "" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}

	u := url.URL{Scheme: "file", Path: slashPath}
	q := u.Query()
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_journal_mode", "WAL")
	q.Set("_synchronous", "FULL")
	// Every WithTx starts BEGIN IMMEDIATE: the write lock is taken up front, so
	// busy_timeout applies and a stale deferred snapshot cannot fail mid-guard.
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// IsBusy reports whether err is SQLITE_BUSY or SQLITE_LOCKED, including their
// extended codes (the low byte is the primary result code). Other processes
// (Box, run-* subcommands) may hold the write lock past busy_timeout.
func IsBusy(err error) bool {
	var coded interface{ Code() int }
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & 0xff {
	case 5, 6:
		return true
	}
	return false
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) WithTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if err := fn(tx); err != nil {
		return mapCanonicalInvariantError(err)
	}
	if err := tx.Commit(); err != nil {
		return mapCanonicalInvariantError(err)
	}
	committed = true
	return nil
}

func mapCanonicalInvariantError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), policyProfileInactiveMarker) {
		return fmt.Errorf("%w: active policy profile no longer matches decision: %v", domain.ErrPolicyDenied, err)
	}
	return err
}
