package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type DecisionRecord struct {
	ID                 domain.ID
	Trigger            string
	Alternatives       []string
	BasisClass         string
	EvidenceIDs        []domain.ID
	PolicyDecisionID   domain.ID
	AuthorityIDs       []domain.ID
	ExpectedOutcome    string
	Confidence         float64
	ResourceEnvelopeID domain.ID
	ActorID            domain.ID
	CreatedAt          time.Time
}

type Event struct {
	ID        domain.ID
	Kind      string
	ActorID   domain.ID
	SubjectID domain.ID
	Payload   map[string]any
	CreatedAt time.Time
}

type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

func (s *Service) RecordDecision(ctx context.Context, input DecisionRecord) (DecisionRecord, error) {
	if err := s.configured(); err != nil {
		return DecisionRecord{}, err
	}
	input, err := validateDecision(input)
	if err != nil {
		return DecisionRecord{}, err
	}
	if input.ID == "" {
		input.ID = domain.NewID("decision")
	}
	input.CreatedAt = s.clock.Now().UTC()
	alternativesJSON, err := json.Marshal(input.Alternatives)
	if err != nil {
		return DecisionRecord{}, err
	}
	evidenceJSON, err := json.Marshal(input.EvidenceIDs)
	if err != nil {
		return DecisionRecord{}, err
	}
	authorityJSON, err := json.Marshal(input.AuthorityIDs)
	if err != nil {
		return DecisionRecord{}, err
	}
	_, err = s.store.DB().ExecContext(ctx, `
		INSERT INTO decision_records(
			decision_id, trigger, alternatives_json, basis_class, evidence_ids_json,
			policy_decision_id, authority_ids_json, expected_outcome, confidence,
			resource_envelope_id, actor_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.ID, input.Trigger, string(alternativesJSON), input.BasisClass, string(evidenceJSON),
		input.PolicyDecisionID, string(authorityJSON), input.ExpectedOutcome, input.Confidence,
		input.ResourceEnvelopeID, input.ActorID, formatTime(input.CreatedAt),
	)
	if err != nil {
		return DecisionRecord{}, fmt.Errorf("record decision: %w", err)
	}
	return input, nil
}

func (s *Service) Decision(ctx context.Context, id domain.ID) (DecisionRecord, error) {
	if err := s.configured(); err != nil {
		return DecisionRecord{}, err
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return DecisionRecord{}, errors.New("decision id is required")
	}
	var record DecisionRecord
	var alternativesJSON, evidenceJSON, authorityJSON, createdAt string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT decision_id, trigger, alternatives_json, basis_class, evidence_ids_json,
		       policy_decision_id, authority_ids_json, expected_outcome, confidence,
		       resource_envelope_id, actor_id, created_at
		FROM decision_records WHERE decision_id = ?`, id).
		Scan(&record.ID, &record.Trigger, &alternativesJSON, &record.BasisClass, &evidenceJSON,
			&record.PolicyDecisionID, &authorityJSON, &record.ExpectedOutcome, &record.Confidence,
			&record.ResourceEnvelopeID, &record.ActorID, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DecisionRecord{}, fmt.Errorf("decision %s not found", id)
	}
	if err != nil {
		return DecisionRecord{}, err
	}
	if err := json.Unmarshal([]byte(alternativesJSON), &record.Alternatives); err != nil {
		return DecisionRecord{}, fmt.Errorf("decode decision alternatives: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &record.EvidenceIDs); err != nil {
		return DecisionRecord{}, fmt.Errorf("decode decision evidence ids: %w", err)
	}
	if err := json.Unmarshal([]byte(authorityJSON), &record.AuthorityIDs); err != nil {
		return DecisionRecord{}, fmt.Errorf("decode decision authority ids: %w", err)
	}
	record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return DecisionRecord{}, fmt.Errorf("parse decision created_at: %w", err)
	}
	return record, nil
}

func (s *Service) Append(ctx context.Context, input Event) (Event, error) {
	if err := s.configured(); err != nil {
		return Event{}, err
	}
	input.ID = domain.ID(strings.TrimSpace(string(input.ID)))
	input.Kind = strings.TrimSpace(input.Kind)
	input.ActorID = domain.ID(strings.TrimSpace(string(input.ActorID)))
	input.SubjectID = domain.ID(strings.TrimSpace(string(input.SubjectID)))
	if input.Kind == "" || input.ActorID == "" || input.SubjectID == "" {
		return Event{}, errors.New("audit kind, actor id, and subject id are required")
	}
	if input.ID == "" {
		input.ID = domain.NewID("audit")
	}
	if input.Payload == nil {
		input.Payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(input.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("encode audit payload: %w", err)
	}
	input.CreatedAt = s.clock.Now().UTC()
	_, err = s.store.DB().ExecContext(ctx, `
		INSERT INTO audit_events(audit_id, kind, actor_id, subject_id, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, input.ID, input.Kind, input.ActorID, input.SubjectID, string(payloadJSON), formatTime(input.CreatedAt))
	if err != nil {
		return Event{}, fmt.Errorf("append audit event: %w", err)
	}
	return input, nil
}

func validateDecision(input DecisionRecord) (DecisionRecord, error) {
	input.ID = domain.ID(strings.TrimSpace(string(input.ID)))
	input.Trigger = strings.TrimSpace(input.Trigger)
	input.BasisClass = strings.TrimSpace(input.BasisClass)
	input.PolicyDecisionID = domain.ID(strings.TrimSpace(string(input.PolicyDecisionID)))
	input.ExpectedOutcome = strings.TrimSpace(input.ExpectedOutcome)
	input.ResourceEnvelopeID = domain.ID(strings.TrimSpace(string(input.ResourceEnvelopeID)))
	input.ActorID = domain.ID(strings.TrimSpace(string(input.ActorID)))
	if input.Trigger == "" || input.BasisClass == "" || input.PolicyDecisionID == "" || input.ExpectedOutcome == "" || input.ResourceEnvelopeID == "" || input.ActorID == "" {
		return DecisionRecord{}, errors.New("decision trigger, basis, policy decision, expected outcome, resource envelope, and actor are required")
	}
	if len(input.Alternatives) == 0 {
		return DecisionRecord{}, errors.New("decision must record alternatives considered")
	}
	alternatives := make([]string, 0, len(input.Alternatives))
	for _, alternative := range input.Alternatives {
		alternative = strings.TrimSpace(alternative)
		if alternative == "" {
			return DecisionRecord{}, errors.New("decision alternative must not be empty")
		}
		alternatives = append(alternatives, alternative)
	}
	input.Alternatives = alternatives
	if len(input.EvidenceIDs) == 0 {
		return DecisionRecord{}, errors.New("decision must cite evidence")
	}
	input.EvidenceIDs = cleanIDs(input.EvidenceIDs)
	if len(input.AuthorityIDs) == 0 {
		return DecisionRecord{}, errors.New("decision must record authority ids")
	}
	input.AuthorityIDs = cleanIDs(input.AuthorityIDs)
	if input.Confidence < 0 || input.Confidence > 1 {
		return DecisionRecord{}, errors.New("decision confidence must be between 0 and 1")
	}
	return input, nil
}

func cleanIDs(ids []domain.ID) []domain.ID {
	seen := map[domain.ID]struct{}{}
	out := make([]domain.ID, 0, len(ids))
	for _, id := range ids {
		id = domain.ID(strings.TrimSpace(string(id)))
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func formatTime(t time.Time) string {
	return domain.FormatTime(t)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil || s.clock == nil {
		return errors.New("audit service is not configured")
	}
	return nil
}
