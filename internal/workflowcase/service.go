package workflowcase

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
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflow"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type Observation struct {
	MissionID       domain.ID
	Source          string
	ObjectID        string
	RevisionID      string
	EvidenceID      string
	FirstWork       workflow.WorkProposal
	Grant           workflow.Grant
	MaxSteps        int
	RemainingBudget int64
}

type State string

const (
	Active               State = "ACTIVE"
	Blocked              State = "BLOCKED"
	ReadyForVerification State = "READY_FOR_VERIFICATION"
	Closed               State = "CLOSED"
)

type Case struct {
	ID, MissionID, CurrentWorkID                        domain.ID
	Source, ObjectID, RevisionID, ObservationEvidenceID string
	State                                               State
	NextWork                                            workflow.WorkProposal
	Grant                                               workflow.Grant
	CompletedSteps, MaxSteps                            int
	RemainingBudget                                     int64
	ProgressSignature                                   string
}

type AssessmentRecord struct {
	ID, WorkID  string
	RequestJSON string
	ResultJSON  string
	CreatedAt   time.Time
}

type VerificationRequest struct {
	CaseID       domain.ID
	VerifierID   domain.ID
	VerifierType string
	SnapshotHash string
	SnapshotJSON string
	EvidenceIDs  []domain.ID
}

type VerificationRecord struct {
	ID           domain.ID
	CaseID       domain.ID
	VerifierID   domain.ID
	VerifierType string
	SnapshotHash string
	SnapshotJSON string
	EvidenceIDs  []domain.ID
	CreatedAt    time.Time
}

const caseColumns = `case_id, mission_id, source, object_id, revision_id,
		observation_evidence_id, state, current_work_id, next_work_json, grant_json,
		completed_steps, max_steps, remaining_budget, progress_signature, initial_request_json`

const verificationColumns = `verification_id, case_id, verifier_id, verifier_type, snapshot_hash,
		snapshot_json, evidence_ids_json, created_at`

const closeRetryAttempts = 20

type Service struct {
	store    *state.Store
	clock    clock.Clock
	purposes *purpose.Service
}

func New(store *state.Store, clk clock.Clock, purposes *purpose.Service) *Service {
	return &Service{store: store, clock: clk, purposes: purposes}
}

func (s *Service) Get(ctx context.Context, caseID domain.ID) (Case, error) {
	if s == nil || s.store == nil {
		return Case{}, errors.New("workflow case service is not configured")
	}
	if strings.TrimSpace(string(caseID)) == "" {
		return Case{}, errors.New("case ID is required")
	}
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE case_id = ?`, caseID)
	c, _, err := scanCase(row)
	if err != nil {
		return Case{}, fmt.Errorf("get workflow case: %w", err)
	}
	return c, nil
}

func (s *Service) FindVerification(ctx context.Context, caseID domain.ID) (VerificationRecord, bool, error) {
	if s == nil || s.store == nil {
		return VerificationRecord{}, false, errors.New("workflow case service is not configured")
	}
	caseID = domain.ID(strings.TrimSpace(string(caseID)))
	if caseID == "" {
		return VerificationRecord{}, false, errors.New("case ID is required")
	}
	record, err := scanVerification(s.store.DB().QueryRowContext(ctx,
		`SELECT `+verificationColumns+` FROM workflow_verifications WHERE case_id = ?`, caseID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return VerificationRecord{}, false, nil
	}
	if err != nil {
		return VerificationRecord{}, false, err
	}
	return record, true, nil
}

func (s *Service) ListActive(ctx context.Context, missionID domain.ID) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	if missionID == "" {
		return nil, errors.New("mission ID is required")
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT `+caseColumns+` FROM workflow_cases WHERE state = ? AND mission_id = ? ORDER BY case_id`,
		Active, missionID)
	if err != nil {
		return nil, fmt.Errorf("list active workflow cases: %w", err)
	}
	defer rows.Close()
	cases := make([]Case, 0)
	for rows.Next() {
		c, _, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan active workflow case: %w", err)
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active workflow cases: %w", err)
	}
	return cases, nil
}

func (s *Service) ListReadyForVerification(ctx context.Context, missionID domain.ID) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	if missionID == "" {
		return nil, errors.New("mission ID is required")
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT `+caseColumns+` FROM workflow_cases WHERE state = ? AND mission_id = ? ORDER BY case_id`,
		ReadyForVerification, missionID)
	if err != nil {
		return nil, fmt.Errorf("list workflow cases ready for verification: %w", err)
	}
	defer rows.Close()
	cases := make([]Case, 0)
	for rows.Next() {
		c, _, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow case ready for verification: %w", err)
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow cases ready for verification: %w", err)
	}
	return cases, nil
}

func (s *Service) ListAssessments(ctx context.Context, caseID domain.ID) ([]AssessmentRecord, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	caseID = domain.ID(strings.TrimSpace(string(caseID)))
	if caseID == "" {
		return nil, errors.New("case ID is required")
	}
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT assessment_id, work_id, request_json, result_json, created_at
		 FROM workflow_assessments WHERE case_id = ? ORDER BY created_at, assessment_id`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list workflow assessments: %w", err)
	}
	defer rows.Close()
	records := make([]AssessmentRecord, 0)
	for rows.Next() {
		var record AssessmentRecord
		var createdAt string
		if err := rows.Scan(&record.ID, &record.WorkID, &record.RequestJSON, &record.ResultJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan workflow assessment: %w", err)
		}
		record.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse workflow assessment %s created_at: %w", record.ID, err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow assessments: %w", err)
	}
	return records, nil
}

// ListByObject returns every case of one object across all states. Supersession
// cannot be built on ListActive: once an assessment clears current_work_id the
// case is BLOCKED, so the accepted decision that identifies the newest revision
// disappears from an active-only scan and the driver forgets newer work after a
// restart.
func (s *Service) ListByObject(ctx context.Context, missionID domain.ID, source, objectID string) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	source = strings.TrimSpace(source)
	objectID = strings.TrimSpace(objectID)
	if missionID == "" || source == "" || objectID == "" {
		return nil, errors.New("mission, source, and object ID are required")
	}
	return s.listWhere(ctx,
		`mission_id = ? AND source = ? AND object_id = ?`, missionID, source, objectID)
}

// ListBySource returns every case of one source across all states, which the
// reviewer needs because a superseded case it must re-read is BLOCKED.
func (s *Service) ListBySource(ctx context.Context, missionID domain.ID, source string) ([]Case, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("workflow case service is not configured")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	source = strings.TrimSpace(source)
	if missionID == "" || source == "" {
		return nil, errors.New("mission and source are required")
	}
	return s.listWhere(ctx, `mission_id = ? AND source = ?`, missionID, source)
}

func (s *Service) listWhere(ctx context.Context, predicate string, args ...any) ([]Case, error) {
	rows, err := s.store.DB().QueryContext(ctx,
		`SELECT `+caseColumns+` FROM workflow_cases WHERE `+predicate+` ORDER BY revision_id, case_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow cases: %w", err)
	}
	defer rows.Close()
	cases := make([]Case, 0)
	for rows.Next() {
		c, _, err := scanCase(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow case: %w", err)
		}
		cases = append(cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow cases: %w", err)
	}
	return cases, nil
}

func (s *Service) Close(ctx context.Context, request VerificationRequest) (VerificationRecord, error) {
	if s == nil || s.store == nil || s.clock == nil || s.purposes == nil {
		return VerificationRecord{}, errors.New("workflow case service is not configured")
	}
	request.CaseID = domain.ID(strings.TrimSpace(string(request.CaseID)))
	request.VerifierID = domain.ID(strings.TrimSpace(string(request.VerifierID)))
	request.VerifierType = strings.TrimSpace(request.VerifierType)
	request.SnapshotHash = strings.TrimSpace(request.SnapshotHash)
	if request.CaseID == "" || request.VerifierID == "" || request.VerifierType == "" || request.SnapshotHash == "" || strings.TrimSpace(request.SnapshotJSON) == "" {
		return VerificationRecord{}, errors.New("case, verifier, snapshot hash, and snapshot JSON are required")
	}
	evidenceIDs := normalizeWorkflowEvidenceIDs(request.EvidenceIDs)

	var result VerificationRecord
	var err error
	for attempt := 0; attempt < closeRetryAttempts; attempt++ {
		err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
			row := tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE case_id = ?`, request.CaseID)
			current, _, err := scanCase(row)
			if err != nil {
				return err
			}
			if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: current.MissionID}); err != nil {
				return err
			}

			existing, err := scanVerification(tx.QueryRowContext(ctx, `SELECT `+verificationColumns+` FROM workflow_verifications WHERE case_id = ?`, request.CaseID))
			if err == nil {
				result, err = compareWorkflowVerification(existing, request, evidenceIDs)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if current.State != ReadyForVerification {
				return fmt.Errorf("case %s is not ready for verification", request.CaseID)
			}
			if err := requireWorkflowEvidence(ctx, tx, evidenceIDs); err != nil {
				return err
			}
			evidenceJSON, err := json.Marshal(evidenceIDs)
			if err != nil {
				return fmt.Errorf("encode verification evidence: %w", err)
			}
			now := s.clock.Now().UTC()
			record := VerificationRecord{
				ID: domain.NewID("verification"), CaseID: request.CaseID, VerifierID: request.VerifierID,
				VerifierType: request.VerifierType, SnapshotHash: request.SnapshotHash, SnapshotJSON: request.SnapshotJSON,
				EvidenceIDs: evidenceIDs, CreatedAt: now,
			}
			if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_verifications (
				verification_id, case_id, verifier_id, verifier_type, snapshot_hash,
				snapshot_json, evidence_ids_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				record.ID, record.CaseID, record.VerifierID, record.VerifierType, record.SnapshotHash,
				record.SnapshotJSON, string(evidenceJSON), formatWorkflowTime(now),
			); err != nil {
				return err
			}
			updated, err := tx.ExecContext(ctx, `
			UPDATE workflow_cases
			SET state = ?, current_work_id = '', next_work_json = '{}', updated_at = ?
			WHERE case_id = ? AND state = ?`,
				Closed, formatWorkflowTime(now), request.CaseID, ReadyForVerification,
			)
			if err != nil {
				return err
			}
			changed, err := updated.RowsAffected()
			if err != nil {
				return err
			}
			if changed != 1 {
				return errors.New("workflow case closure lost state race")
			}
			result = record
			return nil
		})
		if err == nil {
			return result, nil
		}
		if !isSQLiteCloseRace(err) {
			return VerificationRecord{}, fmt.Errorf("close workflow case: %w", err)
		}
		replayed, replayErr := s.replayWorkflowVerification(ctx, request, evidenceIDs)
		if replayErr == nil {
			return replayed, nil
		}
		if !errors.Is(replayErr, sql.ErrNoRows) && !isSQLiteCloseRace(replayErr) {
			return VerificationRecord{}, fmt.Errorf("close workflow case: %w", replayErr)
		}
	}
	return VerificationRecord{}, fmt.Errorf("close workflow case: %w", err)
}

func compareWorkflowVerification(existing VerificationRecord, request VerificationRequest, evidenceIDs []domain.ID) (VerificationRecord, error) {
	if existing.SnapshotHash != request.SnapshotHash || !equalWorkflowEvidenceIDs(existing.EvidenceIDs, evidenceIDs) {
		return VerificationRecord{}, fmt.Errorf("%w: verification already exists for case %s", domain.ErrIntentConflict, request.CaseID)
	}
	return existing, nil
}

func (s *Service) replayWorkflowVerification(ctx context.Context, request VerificationRequest, evidenceIDs []domain.ID) (VerificationRecord, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		record, err := scanVerification(s.store.DB().QueryRowContext(ctx, `SELECT `+verificationColumns+` FROM workflow_verifications WHERE case_id = ?`, request.CaseID))
		if err == nil {
			return compareWorkflowVerification(record, request, evidenceIDs)
		}
		lastErr = err
		if !errors.Is(err, sql.ErrNoRows) && !isSQLiteCloseRace(err) {
			return VerificationRecord{}, err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return VerificationRecord{}, ctx.Err()
		case <-timer.C:
		}
	}
	return VerificationRecord{}, lastErr
}

func isSQLiteCloseRace(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code()
	return code&0xff == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || strings.Contains(sqliteErr.Error(), "UNIQUE constraint")
}

func (s *Service) Reject(ctx context.Context, caseID domain.ID, reason string, evidenceIDs []domain.ID) (VerificationRecord, error) {
	if s == nil || s.store == nil || s.clock == nil || s.purposes == nil {
		return VerificationRecord{}, errors.New("workflow case service is not configured")
	}
	caseID = domain.ID(strings.TrimSpace(string(caseID)))
	reason = strings.TrimSpace(reason)
	if caseID == "" || reason == "" {
		return VerificationRecord{}, errors.New("case and rejection reason are required")
	}
	evidenceIDs = normalizeWorkflowEvidenceIDs(evidenceIDs)
	snapshotJSON, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("encode rejection reason: %w", err)
	}

	var result VerificationRecord
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE case_id = ?`, caseID)
		current, _, err := scanCase(row)
		if err != nil {
			return err
		}
		if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: current.MissionID}); err != nil {
			return err
		}
		if current.State != ReadyForVerification {
			return fmt.Errorf("case %s is not ready for rejection", caseID)
		}
		if err := requireWorkflowEvidence(ctx, tx, evidenceIDs); err != nil {
			return err
		}
		evidenceJSON, err := json.Marshal(evidenceIDs)
		if err != nil {
			return fmt.Errorf("encode rejection evidence: %w", err)
		}
		now := s.clock.Now().UTC()
		record := VerificationRecord{
			ID: domain.NewID("verification"), CaseID: caseID, VerifierType: "REJECT",
			SnapshotJSON: string(snapshotJSON), EvidenceIDs: evidenceIDs, CreatedAt: now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_verifications (
				verification_id, case_id, verifier_id, verifier_type, snapshot_hash,
				snapshot_json, evidence_ids_json, created_at
			) VALUES (?, ?, '', 'REJECT', '', ?, ?, ?)`,
			record.ID, record.CaseID, record.SnapshotJSON, string(evidenceJSON), formatWorkflowTime(now),
		); err != nil {
			return err
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE workflow_cases
			SET state = ?, current_work_id = '', next_work_json = '{}', updated_at = ?
			WHERE case_id = ? AND state = ?`,
			Blocked, formatWorkflowTime(now), caseID, ReadyForVerification,
		)
		if err != nil {
			return err
		}
		changed, err := updated.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("workflow case rejection lost state race")
		}
		result = record
		return nil
	})
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("reject workflow case: %w", err)
	}
	return result, nil
}

type preparedObservation struct {
	observation Observation
	workID      domain.ID
	requestJSON string
	grantJSON   string
	workJSON    string
	now         string
}

func (s *Service) prepareObservation(observation Observation) (preparedObservation, error) {
	var prepared preparedObservation
	if strings.TrimSpace(string(observation.MissionID)) == "" ||
		strings.TrimSpace(observation.Source) == "" ||
		strings.TrimSpace(observation.ObjectID) == "" ||
		strings.TrimSpace(observation.RevisionID) == "" ||
		strings.TrimSpace(observation.EvidenceID) == "" ||
		strings.TrimSpace(observation.FirstWork.Kind) == "" ||
		observation.MaxSteps <= 0 || observation.RemainingBudget <= 0 {
		return prepared, errors.New("observation identity, evidence, first work, and positive limits are required")
	}
	decision, err := workflow.Decide(workflow.Input{
		Assessment: workflow.Assessment{
			Verdict: workflow.Continue, EvidenceIDs: []string{observation.EvidenceID}, Next: &observation.FirstWork,
		},
		Grant:          observation.Grant,
		Limits:         workflow.Limits{MaxSteps: observation.MaxSteps, RemainingBudget: observation.RemainingBudget},
		CompletedSteps: 0,
	})
	if err != nil {
		return prepared, fmt.Errorf("invalid first work: %w", err)
	}
	if decision.Outcome != workflow.OutcomeContinue {
		return prepared, fmt.Errorf("first work decision is %s", decision.Outcome)
	}
	requestJSON, err := json.Marshal(observation)
	if err != nil {
		return prepared, fmt.Errorf("encode observation: %w", err)
	}
	grantJSON, err := json.Marshal(observation.Grant)
	if err != nil {
		return prepared, fmt.Errorf("encode grant: %w", err)
	}
	workJSON, err := json.Marshal(observation.FirstWork)
	if err != nil {
		return prepared, fmt.Errorf("encode first work: %w", err)
	}
	prepared.observation = observation
	prepared.workID = domain.NewID("work")
	prepared.requestJSON = string(requestJSON)
	prepared.grantJSON = string(grantJSON)
	prepared.workJSON = string(workJSON)
	prepared.now = s.clock.Now().UTC().Format(time.RFC3339Nano)
	return prepared, nil
}

func (s *Service) ensureTx(ctx context.Context, tx *sql.Tx, prepared preparedObservation) (Case, error) {
	observation := prepared.observation
	if err := s.purposes.ValidatePurposeTx(ctx, tx, domain.PurposeRef{Kind: domain.PurposeMission, ID: observation.MissionID}); err != nil {
		return Case{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_cases (
		case_id, mission_id, source, object_id, revision_id, observation_evidence_id,
		initial_request_json, grant_json, state, current_work_id, next_work_json,
		completed_steps, max_steps, remaining_budget, progress_signature, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(mission_id, source, object_id, revision_id) DO NOTHING`,
		domain.NewID("case"), observation.MissionID, observation.Source, observation.ObjectID,
		observation.RevisionID, observation.EvidenceID, prepared.requestJSON, prepared.grantJSON, Active,
		prepared.workID, prepared.workJSON, 0, observation.MaxSteps, observation.RemainingBudget, "", prepared.now, prepared.now,
	); err != nil {
		return Case{}, err
	}
	row := tx.QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE mission_id = ? AND source = ? AND object_id = ? AND revision_id = ?`,
		observation.MissionID, observation.Source, observation.ObjectID, observation.RevisionID)
	result, storedRequest, err := scanCase(row)
	if err != nil {
		return Case{}, err
	}
	if storedRequest != prepared.requestJSON {
		return Case{}, errors.New("observation revision already exists with a different initial request")
	}
	return result, nil
}

func (s *Service) Ensure(ctx context.Context, observation Observation) (Case, error) {
	if s == nil || s.store == nil || s.clock == nil || s.purposes == nil {
		return Case{}, errors.New("workflow case service is not configured")
	}
	prepared, err := s.prepareObservation(observation)
	if err != nil {
		return Case{}, err
	}
	var result Case
	if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = s.ensureTx(ctx, tx, prepared)
		return err
	}); err != nil {
		return Case{}, fmt.Errorf("ensure workflow case: %w", err)
	}
	return result, nil
}

func (s *Service) Find(ctx context.Context, missionID domain.ID, source, objectID, revisionID string) (Case, bool, error) {
	if s == nil || s.store == nil {
		return Case{}, false, errors.New("workflow case service is not configured")
	}
	row := s.store.DB().QueryRowContext(ctx, `SELECT `+caseColumns+` FROM workflow_cases WHERE mission_id = ? AND source = ? AND object_id = ? AND revision_id = ?`,
		missionID, source, objectID, revisionID)
	c, _, err := scanCase(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Case{}, false, nil
	}
	if err != nil {
		return Case{}, false, fmt.Errorf("find workflow case: %w", err)
	}
	return c, true, nil
}

type rowScanner interface{ Scan(...any) error }

func scanCase(row rowScanner) (Case, string, error) {
	var c Case
	var workJSON, grantJSON, requestJSON string
	err := row.Scan(&c.ID, &c.MissionID, &c.Source, &c.ObjectID, &c.RevisionID,
		&c.ObservationEvidenceID, &c.State, &c.CurrentWorkID, &workJSON, &grantJSON,
		&c.CompletedSteps, &c.MaxSteps, &c.RemainingBudget, &c.ProgressSignature, &requestJSON)
	if err != nil {
		return Case{}, "", err
	}
	if err := json.Unmarshal([]byte(workJSON), &c.NextWork); err != nil {
		return Case{}, "", fmt.Errorf("decode next work: %w", err)
	}
	if err := json.Unmarshal([]byte(grantJSON), &c.Grant); err != nil {
		return Case{}, "", fmt.Errorf("decode grant: %w", err)
	}
	return c, requestJSON, nil
}

func normalizeWorkflowEvidenceIDs(ids []domain.ID) []domain.ID {
	set := make(map[domain.ID]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	normalized := make([]domain.ID, 0, len(set))
	for id := range set {
		normalized = append(normalized, id)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i] < normalized[j] })
	return normalized
}

func equalWorkflowEvidenceIDs(left, right []domain.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func requireWorkflowEvidence(ctx context.Context, tx *sql.Tx, ids []domain.ID) error {
	for _, id := range ids {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM evidence_objects WHERE evidence_id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("evidence %q not found", id)
			}
			return err
		}
	}
	return nil
}

func scanVerification(row rowScanner) (VerificationRecord, error) {
	var record VerificationRecord
	var evidenceJSON, createdAt string
	if err := row.Scan(
		&record.ID, &record.CaseID, &record.VerifierID, &record.VerifierType, &record.SnapshotHash,
		&record.SnapshotJSON, &evidenceJSON, &createdAt,
	); err != nil {
		return VerificationRecord{}, err
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &record.EvidenceIDs); err != nil {
		return VerificationRecord{}, fmt.Errorf("decode verification evidence: %w", err)
	}
	record.EvidenceIDs = normalizeWorkflowEvidenceIDs(record.EvidenceIDs)
	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return VerificationRecord{}, fmt.Errorf("parse workflow verification %s created_at: %w", record.ID, err)
	}
	record.CreatedAt = created
	return record, nil
}

func formatWorkflowTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
