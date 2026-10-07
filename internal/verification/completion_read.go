package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/SofiaFlux/summa42/internal/domain"
)

// CompletionEvidence returns only the durable manifest for this exact Task/Attempt.
func (s *Service) CompletionEvidence(ctx context.Context, taskID, attemptID domain.ID) ([]domain.ID, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	if taskID == "" || attemptID == "" {
		return nil, errors.New("task and attempt required")
	}
	var raw, hash string
	if err := s.store.DB().QueryRowContext(ctx, `SELECT manifest_json,manifest_hash FROM attempt_completion_records WHERE task_id=? AND attempt_id=?`, taskID, attemptID).Scan(&raw, &hash); err != nil {
		return nil, err
	}
	var manifest struct {
		Version     int         `json:"version"`
		EvidenceIDs []domain.ID `json:"evidence_ids"`
	}
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(raw))
	if manifest.Version != 1 || len(manifest.EvidenceIDs) == 0 || hex.EncodeToString(digest[:]) != hash {
		return nil, errors.New("invalid completion manifest")
	}
	return manifest.EvidenceIDs, nil
}
