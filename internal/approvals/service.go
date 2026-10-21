package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type CreateRequest struct {
	SubjectKind       string
	SubjectID         domain.ID
	RequestDigest     string
	PolicyDecisionID  domain.ID
	RequiredApprovers []domain.ID
	RequestedBy       domain.ID
	ExpiresAt         time.Time
}

type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (domain.ApprovalRequestRecord, error) {
	if err := s.configured(); err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	var out domain.ApprovalRequestRecord
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = s.CreateInTx(ctx, tx, request)
		return err
	})
	return out, err
}

func (s *Service) CreateInTx(ctx context.Context, tx *sql.Tx, request CreateRequest) (domain.ApprovalRequestRecord, error) {
	if err := s.configured(); err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	if tx == nil {
		return domain.ApprovalRequestRecord{}, errors.New("approval creation requires transaction")
	}
	request, err := s.validateCreate(request)
	if err != nil {
		return domain.ApprovalRequestRecord{}, err
	}

	existing, err := loadBySubjectDigest(ctx, tx, request.SubjectKind, request.SubjectID, request.RequestDigest)
	if err == nil {
		if !sameCreate(existing, request) {
			return domain.ApprovalRequestRecord{}, errors.New("existing approval subject/digest has different immutable definition")
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ApprovalRequestRecord{}, err
	}

	now := s.clock.Now().UTC()
	requiredJSON, err := json.Marshal(request.RequiredApprovers)
	if err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	record := domain.ApprovalRequestRecord{
		ID: domain.NewID("approval"), SubjectKind: request.SubjectKind, SubjectID: request.SubjectID,
		RequestDigest: request.RequestDigest, PolicyDecisionID: request.PolicyDecisionID,
		RequiredApprovers: append([]domain.ID(nil), request.RequiredApprovers...), RequestedBy: request.RequestedBy,
		State: domain.ApprovalPending, ExpiresAt: request.ExpiresAt, CreatedAt: now,
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO approval_requests(
			approval_id, subject_kind, subject_id, request_digest, policy_decision_id,
			required_approvers_json, requested_by, state, expires_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.SubjectKind, record.SubjectID, record.RequestDigest, record.PolicyDecisionID,
		string(requiredJSON), record.RequestedBy, record.State, formatTime(record.ExpiresAt), formatTime(now),
	)
	if err != nil {
		return domain.ApprovalRequestRecord{}, fmt.Errorf("create approval: %w", err)
	}
	return record, nil
}

func (s *Service) Get(ctx context.Context, id domain.ID) (domain.ApprovalRequestRecord, error) {
	if err := s.configured(); err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	return s.get(ctx, s.store.DB(), id)
}

func (s *Service) GetInTx(ctx context.Context, tx *sql.Tx, id domain.ID) (domain.ApprovalRequestRecord, error) {
	if err := s.configured(); err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	if tx == nil {
		return domain.ApprovalRequestRecord{}, errors.New("approval read requires transaction")
	}
	return s.get(ctx, tx, id)
}

func (s *Service) get(ctx context.Context, q interface {
	rowQueryer
	rowsQueryer
}, id domain.ID) (domain.ApprovalRequestRecord, error) {
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return domain.ApprovalRequestRecord{}, errors.New("approval id is required")
	}
	record, err := loadByID(ctx, q, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ApprovalRequestRecord{}, fmt.Errorf("approval %s not found", id)
	}
	if err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	record.Decisions, err = loadDecisions(ctx, q, id)
	if err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	return record, nil
}

func (s *Service) Pending(ctx context.Context) ([]domain.ApprovalRequestRecord, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT approval_id, subject_kind, subject_id, request_digest, policy_decision_id,
		       required_approvers_json, requested_by, state, expires_at,
		       decided_at, approver_id, decision_action, created_at
		FROM approval_requests
		WHERE state = ? AND expires_at > ?
		ORDER BY created_at, approval_id`,
		domain.ApprovalPending, formatTime(s.clock.Now().UTC()),
	)
	if err != nil {
		return nil, err
	}
	var out []domain.ApprovalRequestRecord
	for rows.Next() {
		record, err := scanApproval(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Decisions, err = loadDecisions(ctx, s.store.DB(), out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) Approve(ctx context.Context, id, approver domain.ID, requestDigest string) error {
	return s.decide(ctx, id, approver, requestDigest, "APPROVE")
}

func (s *Service) Reject(ctx context.Context, id, approver domain.ID, requestDigest string) error {
	return s.decide(ctx, id, approver, requestDigest, "REJECT")
}

func (s *Service) ConsumeInTx(ctx context.Context, tx *sql.Tx, id domain.ID, requestDigest string) error {
	if err := s.configured(); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("approval consume requires transaction")
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	requestDigest = strings.TrimSpace(requestDigest)
	if id == "" || requestDigest == "" {
		return errors.New("approval id and request digest are required")
	}
	now := s.clock.Now().UTC()
	record, err := loadByID(ctx, tx, id)
	if err != nil {
		return err
	}
	if !sameDigest(record.RequestDigest, requestDigest) {
		return errors.New("approval request digest mismatch")
	}
	if !record.ExpiresAt.After(now) {
		return errors.New("approval request expired")
	}
	if record.State != domain.ApprovalApproved {
		return fmt.Errorf("approval %s is %s, not APPROVED", id, record.State)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE approval_requests SET state = ? WHERE approval_id = ? AND state = ?`,
		domain.ApprovalConsumed, id, domain.ApprovalApproved,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("approval was concurrently changed")
	}
	return nil
}

func (s *Service) decide(ctx context.Context, id, approver domain.ID, requestDigest, action string) error {
	if err := s.configured(); err != nil {
		return err
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	approver = domain.ID(strings.TrimSpace(string(approver)))
	requestDigest = strings.TrimSpace(requestDigest)
	if id == "" || approver == "" || requestDigest == "" {
		return errors.New("approval id, approver, and request digest are required")
	}
	if action != "APPROVE" && action != "REJECT" {
		return errors.New("approval action must be APPROVE or REJECT")
	}
	now := s.clock.Now().UTC()
	return s.store.WithTx(ctx, func(tx *sql.Tx) error {
		record, err := loadByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sameDigest(record.RequestDigest, requestDigest) {
			return errors.New("approval request digest mismatch")
		}
		if record.State != domain.ApprovalPending {
			return fmt.Errorf("approval %s is %s, not PENDING", id, record.State)
		}
		if !record.ExpiresAt.After(now) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE approval_requests SET state = ? WHERE approval_id = ? AND state = ?`,
				domain.ApprovalExpired, id, domain.ApprovalPending,
			); err != nil {
				return err
			}
			return errors.New("approval request expired")
		}
		if !containsID(record.RequiredApprovers, approver) {
			return errors.New("principal is not a required approver")
		}

		existing, err := loadDecision(ctx, tx, id, approver)
		if err == nil {
			if existing.DecisionAction == action && sameDigest(existing.RequestDigest, requestDigest) {
				return nil
			}
			return errors.New("approver already recorded a different decision")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO approval_decisions(
				approval_id, approver_id, request_digest, decision_action, decided_at
			) VALUES (?, ?, ?, ?, ?)`,
			id, approver, requestDigest, action, formatTime(now),
		); err != nil {
			return fmt.Errorf("record approval decision: %w", err)
		}

		if action == "REJECT" {
			return terminalize(ctx, tx, id, domain.ApprovalRejected, approver, action, now)
		}

		var approvedCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM approval_decisions
			WHERE approval_id = ? AND decision_action = 'APPROVE'`, id,
		).Scan(&approvedCount); err != nil {
			return err
		}
		if approvedCount < len(record.RequiredApprovers) {
			return nil
		}
		if approvedCount != len(record.RequiredApprovers) {
			return errors.New("approval decision count exceeds required approvers")
		}
		return terminalize(ctx, tx, id, domain.ApprovalApproved, approver, action, now)
	})
}

func terminalize(ctx context.Context, tx *sql.Tx, id domain.ID, target domain.ApprovalState, actor domain.ID, action string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE approval_requests
		SET state = ?, decided_at = ?, approver_id = ?, decision_action = ?
		WHERE approval_id = ? AND state = ?`,
		target, formatTime(now), actor, action, id, domain.ApprovalPending,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("approval was concurrently changed")
	}
	return nil
}

func (s *Service) validateCreate(request CreateRequest) (CreateRequest, error) {
	request.SubjectKind = strings.TrimSpace(request.SubjectKind)
	request.SubjectID = domain.ID(strings.TrimSpace(string(request.SubjectID)))
	request.RequestDigest = strings.TrimSpace(request.RequestDigest)
	request.PolicyDecisionID = domain.ID(strings.TrimSpace(string(request.PolicyDecisionID)))
	request.RequestedBy = domain.ID(strings.TrimSpace(string(request.RequestedBy)))
	request.RequiredApprovers = cleanIDs(request.RequiredApprovers)
	if request.SubjectKind == "" || request.SubjectID == "" || request.RequestDigest == "" ||
		request.RequestedBy == "" || len(request.RequiredApprovers) == 0 {
		return CreateRequest{}, errors.New("approval subject, digest, requester, and required approvers are required")
	}
	if request.SubjectKind == "EXTERNAL_OPERATION" && request.PolicyDecisionID == "" {
		return CreateRequest{}, errors.New("external-operation approval requires policy decision provenance")
	}
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(s.clock.Now().UTC()) {
		return CreateRequest{}, errors.New("approval expiry must be in the future")
	}
	request.ExpiresAt = request.ExpiresAt.UTC()
	return request, nil
}

func sameCreate(record domain.ApprovalRequestRecord, request CreateRequest) bool {
	if record.SubjectKind != request.SubjectKind || record.SubjectID != request.SubjectID ||
		record.RequestDigest != request.RequestDigest || record.PolicyDecisionID != request.PolicyDecisionID ||
		record.RequestedBy != request.RequestedBy || !record.ExpiresAt.Equal(request.ExpiresAt) ||
		len(record.RequiredApprovers) != len(request.RequiredApprovers) {
		return false
	}
	for i := range record.RequiredApprovers {
		if record.RequiredApprovers[i] != request.RequiredApprovers[i] {
			return false
		}
	}
	return true
}

func cleanIDs(values []domain.ID) []domain.ID {
	set := make(map[domain.ID]struct{}, len(values))
	for _, value := range values {
		value = domain.ID(strings.TrimSpace(string(value)))
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]domain.ID, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func containsID(values []domain.ID, target domain.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sameDigest(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return a != "" && a == b
}

type scanner interface {
	Scan(dest ...any) error
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type rowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadByID(ctx context.Context, q rowQueryer, id domain.ID) (domain.ApprovalRequestRecord, error) {
	return scanApproval(q.QueryRowContext(ctx, `
		SELECT approval_id, subject_kind, subject_id, request_digest, policy_decision_id,
		       required_approvers_json, requested_by, state, expires_at,
		       decided_at, approver_id, decision_action, created_at
		FROM approval_requests WHERE approval_id = ?`, id))
}

func loadBySubjectDigest(ctx context.Context, q rowQueryer, kind string, subjectID domain.ID, digest string) (domain.ApprovalRequestRecord, error) {
	return scanApproval(q.QueryRowContext(ctx, `
		SELECT approval_id, subject_kind, subject_id, request_digest, policy_decision_id,
		       required_approvers_json, requested_by, state, expires_at,
		       decided_at, approver_id, decision_action, created_at
		FROM approval_requests
		WHERE subject_kind = ? AND subject_id = ? AND request_digest = ?`,
		kind, subjectID, digest))
}

func loadDecision(ctx context.Context, q rowQueryer, approvalID, approverID domain.ID) (domain.ApprovalDecisionRecord, error) {
	var out domain.ApprovalDecisionRecord
	var decidedAt string
	err := q.QueryRowContext(ctx, `
		SELECT approver_id, request_digest, decision_action, decided_at
		FROM approval_decisions
		WHERE approval_id = ? AND approver_id = ?`,
		approvalID, approverID,
	).Scan(&out.ApproverID, &out.RequestDigest, &out.DecisionAction, &decidedAt)
	if err != nil {
		return domain.ApprovalDecisionRecord{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, decidedAt)
	if err != nil {
		return domain.ApprovalDecisionRecord{}, fmt.Errorf("parse approval decision time: %w", err)
	}
	out.DecidedAt = parsed
	return out, nil
}

func loadDecisions(ctx context.Context, q rowsQueryer, approvalID domain.ID) ([]domain.ApprovalDecisionRecord, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT approver_id, request_digest, decision_action, decided_at
		FROM approval_decisions
		WHERE approval_id = ?
		ORDER BY decided_at, approver_id`, approvalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ApprovalDecisionRecord
	for rows.Next() {
		var decision domain.ApprovalDecisionRecord
		var decidedAt string
		if err := rows.Scan(&decision.ApproverID, &decision.RequestDigest, &decision.DecisionAction, &decidedAt); err != nil {
			return nil, err
		}
		decision.DecidedAt, err = time.Parse(time.RFC3339Nano, decidedAt)
		if err != nil {
			return nil, fmt.Errorf("parse approval decision time: %w", err)
		}
		out = append(out, decision)
	}
	return out, rows.Err()
}

func scanApproval(row scanner) (domain.ApprovalRequestRecord, error) {
	var record domain.ApprovalRequestRecord
	var requiredJSON, state, expiresAt, createdAt string
	var decidedAt, approverID, decisionAction sql.NullString
	if err := row.Scan(
		&record.ID, &record.SubjectKind, &record.SubjectID, &record.RequestDigest, &record.PolicyDecisionID,
		&requiredJSON, &record.RequestedBy, &state, &expiresAt,
		&decidedAt, &approverID, &decisionAction, &createdAt,
	); err != nil {
		return domain.ApprovalRequestRecord{}, err
	}
	record.State = domain.ApprovalState(state)
	if err := json.Unmarshal([]byte(requiredJSON), &record.RequiredApprovers); err != nil {
		return domain.ApprovalRequestRecord{}, fmt.Errorf("decode required approvers: %w", err)
	}
	var err error
	if record.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt); err != nil {
		return domain.ApprovalRequestRecord{}, fmt.Errorf("parse approval expiry: %w", err)
	}
	if record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return domain.ApprovalRequestRecord{}, fmt.Errorf("parse approval created_at: %w", err)
	}
	if decidedAt.Valid {
		if record.DecidedAt, err = time.Parse(time.RFC3339Nano, decidedAt.String); err != nil {
			return domain.ApprovalRequestRecord{}, fmt.Errorf("parse approval decided_at: %w", err)
		}
	}
	record.ApproverID = domain.ID(approverID.String)
	record.DecisionAction = decisionAction.String
	return record, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.store.DB() == nil || s.clock == nil {
		return errors.New("approval service is not configured")
	}
	return nil
}
