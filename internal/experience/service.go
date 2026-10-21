package experience

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/audit"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

const (
	AdaptationExecutorPreference = "EXECUTOR_PREFERENCE"
)

type GrantInput struct {
	Kind                       string    `json:"kind"`
	ScopeKey                   string    `json:"scope_key"`
	AllowedExecutors           []string  `json:"allowed_executors"`
	MinVerifiedSamples         int       `json:"min_verified_samples"`
	MaxAcceptanceRegressionBps int       `json:"max_acceptance_regression_bps"`
	MaxCostRegressionBps       int       `json:"max_cost_regression_bps"`
	ExpiresAt                  time.Time `json:"expires_at"`
}

type GrantRequest struct {
	ID         domain.ID
	Digest     string
	ApprovalID domain.ID
}

type ProposalInput struct {
	GrantID                domain.ID
	GenericTaskClass       domain.GenericTaskClass
	ScopeKey               string
	PreferredExecutor      string
	EvidenceObservationIDs []domain.ID
}

type VerifiedOutcome struct {
	TaskID            domain.ID
	GenericTaskClass  domain.GenericTaskClass
	ScopeKey          string
	ExecutorKind      string
	Accepted          bool
	HumanIntervention bool
	RetryCount        int64
	CostUnits         *int64
	LatencyMs         *int64
}

type PreferenceQuery struct {
	ScopeKey         string
	GenericTaskClass domain.GenericTaskClass
}

type Preference struct {
	RuleID       domain.ID
	ExecutorKind string
}

type Service struct {
	store     *state.Store
	clock     clock.Clock
	approvals *approvals.Service
	audit     *audit.Service
	ownerID   domain.ID
}

func New(store *state.Store, clk clock.Clock, approvalSvc *approvals.Service, auditSvc *audit.Service, ownerPrincipalID domain.ID) *Service {
	return &Service{
		store: store, clock: clk, approvals: approvalSvc, audit: auditSvc,
		ownerID: domain.ID(strings.TrimSpace(string(ownerPrincipalID))),
	}
}

func (s *Service) RequestGrant(ctx context.Context, input GrantInput, requestingActor domain.ID) (GrantRequest, error) {
	if err := s.configured(); err != nil {
		return GrantRequest{}, err
	}
	requestingActor = domain.ID(strings.TrimSpace(string(requestingActor)))
	if requestingActor == "" {
		return GrantRequest{}, errors.New("requesting actor is required")
	}
	normalized, definition, digest, err := normalizeGrantInput(input, s.clock.Now().UTC())
	if err != nil {
		return GrantRequest{}, err
	}

	var result GrantRequest
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var existingID, approvalID domain.ID
		err := tx.QueryRowContext(ctx,
			"SELECT grant_request_id, approval_id FROM adaptation_grant_requests WHERE grant_digest = ?", digest,
		).Scan(&existingID, &approvalID)
		if err == nil {
			var existingActor domain.ID
			if err := tx.QueryRowContext(ctx,
				"SELECT requested_by FROM adaptation_grant_requests WHERE grant_request_id = ?", existingID,
			).Scan(&existingActor); err != nil {
				return err
			}
			if existingActor != requestingActor {
				return errors.New("existing grant request digest belongs to a different requester")
			}
			result = GrantRequest{ID: existingID, Digest: digest, ApprovalID: approvalID}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		requestID := domain.NewID("grant-request")
		approval, err := s.approvals.CreateInTx(ctx, tx, approvals.CreateRequest{
			SubjectKind: "ADAPTATION_GRANT", SubjectID: requestID, RequestDigest: digest,
			RequiredApprovers: []domain.ID{s.ownerID}, RequestedBy: requestingActor,
			ExpiresAt: normalized.ExpiresAt,
		})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO adaptation_grant_requests(grant_request_id, grant_digest, definition_json, approval_id, requested_by, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			requestID, digest, string(definition), approval.ID, requestingActor, formatTime(s.clock.Now().UTC()),
		); err != nil {
			return fmt.Errorf("insert adaptation grant request: %w", err)
		}
		result = GrantRequest{ID: requestID, Digest: digest, ApprovalID: approval.ID}
		return nil
	})
	return result, err
}

func (s *Service) ActivateGrant(ctx context.Context, requestID domain.ID) (domain.AdaptationGrant, error) {
	if err := s.configured(); err != nil {
		return domain.AdaptationGrant{}, err
	}
	requestID = domain.ID(strings.TrimSpace(string(requestID)))
	if requestID == "" {
		return domain.AdaptationGrant{}, errors.New("grant request id is required")
	}
	var out domain.AdaptationGrant
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if existing, found, err := loadGrantByRequest(ctx, tx, requestID); err != nil {
			return err
		} else if found {
			out = existing
			return nil
		}
		var digest, definitionJSON string
		var approvalID domain.ID
		if err := tx.QueryRowContext(ctx,
			"SELECT grant_digest, definition_json, approval_id FROM adaptation_grant_requests WHERE grant_request_id = ?", requestID,
		).Scan(&digest, &definitionJSON, &approvalID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("adaptation grant request %s not found", requestID)
			}
			return err
		}
		var definition GrantInput
		if err := json.Unmarshal([]byte(definitionJSON), &definition); err != nil {
			return fmt.Errorf("decode grant definition: %w", err)
		}
		normalized, _, computed, err := normalizeGrantInput(definition, s.clock.Now().UTC())
		if err != nil {
			return err
		}
		if computed != digest {
			return errors.New("adaptation grant definition digest mismatch")
		}
		if !normalized.ExpiresAt.After(s.clock.Now().UTC()) {
			return errors.New("adaptation grant request expired")
		}

		approval, err := s.approvals.GetInTx(ctx, tx, approvalID)
		if err != nil {
			return err
		}
		if approval.SubjectKind != "ADAPTATION_GRANT" || approval.SubjectID != requestID || approval.RequestDigest != digest {
			return errors.New("adaptation grant approval binding mismatch")
		}
		if len(approval.RequiredApprovers) != 1 || approval.RequiredApprovers[0] != s.ownerID {
			return errors.New("adaptation grant approval is not Owner-authorized")
		}
		if approval.State != domain.ApprovalApproved {
			return fmt.Errorf("adaptation grant approval is %s", approval.State)
		}
		if err := s.approvals.ConsumeInTx(ctx, tx, approval.ID, digest); err != nil {
			return err
		}

		allowedJSON, _ := json.Marshal(normalized.AllowedExecutors)
		out = domain.AdaptationGrant{
			ID: domain.NewID("grant"), RequestID: requestID, Kind: normalized.Kind, ScopeKey: normalized.ScopeKey,
			AllowedExecutors:           append([]string(nil), normalized.AllowedExecutors...),
			MinVerifiedSamples:         normalized.MinVerifiedSamples,
			MaxAcceptanceRegressionBps: normalized.MaxAcceptanceRegressionBps,
			MaxCostRegressionBps:       normalized.MaxCostRegressionBps,
			OwnerPrincipalID:           s.ownerID, ExpiresAt: normalized.ExpiresAt, ActivatedAt: s.clock.Now().UTC(),
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO adaptation_grants(grant_id, grant_request_id, adaptation_kind, scope_key, allowed_executors_json, min_verified_samples, max_acceptance_regression_bps, max_cost_regression_bps, owner_principal_id, expires_at, activated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			out.ID, out.RequestID, out.Kind, out.ScopeKey, string(allowedJSON), out.MinVerifiedSamples,
			out.MaxAcceptanceRegressionBps, out.MaxCostRegressionBps, out.OwnerPrincipalID,
			formatTime(out.ExpiresAt), formatTime(out.ActivatedAt),
		); err != nil {
			return fmt.Errorf("activate adaptation grant: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.AdaptationGrant{}, err
	}
	_, _ = s.audit.Append(ctx, audit.Event{
		Kind: "ADAPTATION_GRANT_ACTIVATED", ActorID: s.ownerID, SubjectID: out.ID,
		Payload: map[string]any{"grant_request_id": out.RequestID, "kind": out.Kind, "scope_key": out.ScopeKey},
	})
	return out, nil
}

func (s *Service) Propose(ctx context.Context, input ProposalInput) (domain.ExperienceProposal, error) {
	if err := s.configured(); err != nil {
		return domain.ExperienceProposal{}, err
	}
	input.GrantID = domain.ID(strings.TrimSpace(string(input.GrantID)))
	input.ScopeKey = strings.TrimSpace(input.ScopeKey)
	input.PreferredExecutor = strings.TrimSpace(input.PreferredExecutor)
	input.EvidenceObservationIDs = cleanIDs(input.EvidenceObservationIDs)
	if input.GrantID == "" || input.PreferredExecutor == "" || len(input.EvidenceObservationIDs) == 0 {
		return domain.ExperienceProposal{}, errors.New("proposal requires grant, preferred executor, and evidence")
	}
	grant, err := s.Grant(ctx, input.GrantID)
	if err != nil {
		return domain.ExperienceProposal{}, err
	}
	if !grant.ExpiresAt.After(s.clock.Now().UTC()) {
		return domain.ExperienceProposal{}, errors.New("adaptation grant expired")
	}
	if grant.Kind != AdaptationExecutorPreference {
		return domain.ExperienceProposal{}, errors.New("grant does not authorize adaptation")
	}
	canonicalScope := strings.TrimSpace(grant.ScopeKey)
	canonicalClass := genericTaskClassForScope(canonicalScope)
	if input.ScopeKey != "" && input.ScopeKey != canonicalScope {
		return domain.ExperienceProposal{}, errors.New("proposal scope does not match canonical grant scope")
	}
	if input.GenericTaskClass != "" && input.GenericTaskClass != canonicalClass {
		return domain.ExperienceProposal{}, errors.New("proposal generic task class does not match canonical scope classification")
	}
	if !containsString(grant.AllowedExecutors, input.PreferredExecutor) {
		return domain.ExperienceProposal{}, errors.New("preferred executor is not allowed by grant")
	}
	input.ScopeKey = canonicalScope
	input.GenericTaskClass = canonicalClass
	now := s.clock.Now().UTC()
	out := domain.ExperienceProposal{
		ID: domain.NewID("experience-proposal"), GrantID: grant.ID, GenericTaskClass: input.GenericTaskClass,
		ScopeKey: input.ScopeKey, PreferredExecutor: input.PreferredExecutor,
		EvidenceObservationIDs: append([]domain.ID(nil), input.EvidenceObservationIDs...),
		State:                  domain.ExperienceCandidate, CreatedAt: now,
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		for _, id := range out.EvidenceObservationIDs {
			var one int
			if err := tx.QueryRowContext(ctx, "SELECT 1 FROM field_observations WHERE observation_id = ?", id).Scan(&one); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("proposal evidence observation %s not found", id)
				}
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO experience_proposals(proposal_id, grant_id, generic_task_class, scope_key, preferred_executor, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			out.ID, out.GrantID, out.GenericTaskClass, out.ScopeKey, out.PreferredExecutor, out.State, formatTime(now), formatTime(now),
		); err != nil {
			return err
		}
		for _, id := range out.EvidenceObservationIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO experience_proposal_evidence(proposal_id, observation_id, linked_at) VALUES (?, ?, ?)",
				out.ID, id, formatTime(now)); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) ObserveVerifiedOutcome(ctx context.Context, input VerifiedOutcome) error {
	if err := s.configured(); err != nil {
		return err
	}
	input.TaskID = domain.ID(strings.TrimSpace(string(input.TaskID)))
	input.ScopeKey = strings.TrimSpace(input.ScopeKey)
	input.ExecutorKind = strings.TrimSpace(input.ExecutorKind)
	if input.TaskID == "" {
		return errors.New("verified outcome requires task")
	}
	if input.GenericTaskClass != "" && !input.GenericTaskClass.Valid() {
		return errors.New("verified outcome generic task class is invalid")
	}
	if input.RetryCount < 0 {
		return errors.New("retry count cannot be negative")
	}
	if input.CostUnits != nil && *input.CostUnits < 0 {
		return errors.New("cost cannot be negative")
	}
	if input.LatencyMs != nil && *input.LatencyMs < 0 {
		return errors.New("latency cannot be negative")
	}

	var taskClass string
	var currentAttempt domain.ID
	var actualExecutor string
	var taskState domain.TaskState
	if err := s.store.DB().QueryRowContext(ctx,
		"SELECT t.task_class, t.current_attempt_id, t.state, a.executor_kind FROM tasks t JOIN attempts a ON a.attempt_id = t.current_attempt_id WHERE t.task_id = ?", input.TaskID,
	).Scan(&taskClass, &currentAttempt, &taskState, &actualExecutor); err != nil {
		return fmt.Errorf("load canonical task attempt: %w", err)
	}
	taskClass = strings.TrimSpace(taskClass)
	if taskClass == "" {
		return errors.New("verified outcome task has no canonical TaskClass")
	}
	canonicalClass := genericTaskClassForScope(taskClass)
	if input.ScopeKey != "" && input.ScopeKey != taskClass {
		return errors.New("verified outcome scope does not match canonical TaskClass")
	}
	if input.GenericTaskClass != "" && input.GenericTaskClass != canonicalClass {
		return errors.New("verified outcome class does not match canonical TaskClass")
	}
	if input.ExecutorKind != "" && input.ExecutorKind != actualExecutor {
		return errors.New("verified outcome executor does not match canonical attempt")
	}
	input.ScopeKey = taskClass
	input.GenericTaskClass = canonicalClass
	input.ExecutorKind = actualExecutor

	if input.Accepted {
		if taskState != domain.TaskSucceeded {
			return errors.New("accepted outcome requires canonical SUCCEEDED task")
		}
		var one int
		if err := s.store.DB().QueryRowContext(ctx,
			"SELECT 1 FROM acceptance_records WHERE task_id = ? AND attempt_id = ?", input.TaskID, currentAttempt,
		).Scan(&one); err != nil {
			return errors.New("accepted outcome lacks canonical acceptance for current Attempt")
		}
	} else {
		if taskState != domain.TaskChallenged {
			return errors.New("negative outcome requires canonical CHALLENGED task")
		}
		var one int
		if err := s.store.DB().QueryRowContext(ctx,
			"SELECT 1 FROM execution_events WHERE task_id = ? AND attempt_id = ? AND event_type = 'TASK_CHALLENGED' ORDER BY created_at DESC LIMIT 1",
			input.TaskID, currentAttempt,
		).Scan(&one); err != nil {
			return errors.New("negative outcome lacks canonical challenge for current Attempt")
		}
	}
	var existing int
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT 1 FROM experience_outcomes WHERE task_id = ? AND generic_task_class = ? AND scope_key = ? AND executor_kind = ? AND accepted = ?",
		input.TaskID, input.GenericTaskClass, input.ScopeKey, input.ExecutorKind, boolToInt(input.Accepted),
	).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.store.DB().ExecContext(ctx,
		"INSERT INTO experience_outcomes(outcome_id, task_id, generic_task_class, scope_key, executor_kind, accepted, human_intervention, retry_count, cost_units, latency_ms, recorded_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		domain.NewID("experience-outcome"), input.TaskID, input.GenericTaskClass, input.ScopeKey, input.ExecutorKind,
		boolToInt(input.Accepted), boolToInt(input.HumanIntervention), input.RetryCount,
		nullableInt(input.CostUnits), nullableInt(input.LatencyMs), formatTime(s.clock.Now().UTC()),
	)
	if err == nil {
		return nil
	}
	if checkErr := s.store.DB().QueryRowContext(ctx,
		"SELECT 1 FROM experience_outcomes WHERE task_id = ? AND generic_task_class = ? AND scope_key = ? AND executor_kind = ? AND accepted = ?",
		input.TaskID, input.GenericTaskClass, input.ScopeKey, input.ExecutorKind, boolToInt(input.Accepted),
	).Scan(&existing); checkErr == nil {
		return nil
	}
	return err
}

func (s *Service) Grant(ctx context.Context, id domain.ID) (domain.AdaptationGrant, error) {
	if err := s.configured(); err != nil {
		return domain.AdaptationGrant{}, err
	}
	var out domain.AdaptationGrant
	var allowedJSON, expires, activated string
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT grant_id, grant_request_id, adaptation_kind, scope_key, allowed_executors_json, min_verified_samples, max_acceptance_regression_bps, max_cost_regression_bps, owner_principal_id, expires_at, activated_at FROM adaptation_grants WHERE grant_id = ?", id,
	).Scan(&out.ID, &out.RequestID, &out.Kind, &out.ScopeKey, &allowedJSON, &out.MinVerifiedSamples, &out.MaxAcceptanceRegressionBps, &out.MaxCostRegressionBps, &out.OwnerPrincipalID, &expires, &activated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdaptationGrant{}, fmt.Errorf("adaptation grant %s not found", id)
	}
	if err != nil {
		return domain.AdaptationGrant{}, err
	}
	if err := json.Unmarshal([]byte(allowedJSON), &out.AllowedExecutors); err != nil {
		return domain.AdaptationGrant{}, err
	}
	out.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return domain.AdaptationGrant{}, err
	}
	out.ActivatedAt, err = time.Parse(time.RFC3339Nano, activated)
	return out, err
}

func loadGrantByRequest(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, requestID domain.ID) (domain.AdaptationGrant, bool, error) {
	var grantID domain.ID
	err := q.QueryRowContext(ctx, "SELECT grant_id FROM adaptation_grants WHERE grant_request_id = ?", requestID).Scan(&grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AdaptationGrant{}, false, nil
	}
	if err != nil {
		return domain.AdaptationGrant{}, false, err
	}
	var out domain.AdaptationGrant
	var allowedJSON, expires, activated string
	err = q.QueryRowContext(ctx,
		"SELECT grant_id, grant_request_id, adaptation_kind, scope_key, allowed_executors_json, min_verified_samples, max_acceptance_regression_bps, max_cost_regression_bps, owner_principal_id, expires_at, activated_at FROM adaptation_grants WHERE grant_id = ?", grantID,
	).Scan(&out.ID, &out.RequestID, &out.Kind, &out.ScopeKey, &allowedJSON, &out.MinVerifiedSamples, &out.MaxAcceptanceRegressionBps, &out.MaxCostRegressionBps, &out.OwnerPrincipalID, &expires, &activated)
	if err != nil {
		return domain.AdaptationGrant{}, false, err
	}
	if err := json.Unmarshal([]byte(allowedJSON), &out.AllowedExecutors); err != nil {
		return domain.AdaptationGrant{}, false, err
	}
	out.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return domain.AdaptationGrant{}, false, err
	}
	out.ActivatedAt, err = time.Parse(time.RFC3339Nano, activated)
	if err != nil {
		return domain.AdaptationGrant{}, false, err
	}
	return out, true, nil
}

func normalizeGrantInput(input GrantInput, now time.Time) (GrantInput, []byte, string, error) {
	input.Kind = strings.TrimSpace(input.Kind)
	input.ScopeKey = strings.TrimSpace(input.ScopeKey)
	input.AllowedExecutors = cleanStrings(input.AllowedExecutors)
	if input.Kind != AdaptationExecutorPreference {
		return GrantInput{}, nil, "", errors.New("only EXECUTOR_PREFERENCE adaptation is permitted")
	}
	if input.ScopeKey == "" || len(input.AllowedExecutors) == 0 || input.MinVerifiedSamples <= 0 {
		return GrantInput{}, nil, "", errors.New("grant requires scope, allowed executors, and positive sample threshold")
	}
	for _, executor := range input.AllowedExecutors {
		if !safeExecutorName(executor) {
			return GrantInput{}, nil, "", fmt.Errorf("invalid executor kind %q", executor)
		}
	}
	if input.MaxAcceptanceRegressionBps < 0 || input.MaxAcceptanceRegressionBps > 10000 || input.MaxCostRegressionBps < 0 {
		return GrantInput{}, nil, "", errors.New("grant regression thresholds are invalid")
	}
	if !input.ExpiresAt.After(now) {
		return GrantInput{}, nil, "", errors.New("grant expiry must be in the future")
	}
	input.ExpiresAt = input.ExpiresAt.UTC()
	definition, err := json.Marshal(input)
	if err != nil {
		return GrantInput{}, nil, "", err
	}
	d := sha256.Sum256(definition)
	return input, definition, hex.EncodeToString(d[:]), nil
}

func cleanStrings(values []string) []string {
	set := map[string]struct{}{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
func cleanIDs(values []domain.ID) []domain.ID {
	set := map[domain.ID]struct{}{}
	for _, v := range values {
		v = domain.ID(strings.TrimSpace(string(v)))
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]domain.ID, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}
func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
func formatTime(v time.Time) string { return v.UTC().Format(time.RFC3339Nano) }

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil || s.clock == nil || s.approvals == nil || s.audit == nil || s.ownerID == "" {
		return errors.New("experience service is not configured")
	}
	return nil
}

func safeExecutorName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for i, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			if i == 0 && (r == '.' || r == '_' || r == '-') {
				return false
			}
			continue
		}
		return false
	}
	return true
}
