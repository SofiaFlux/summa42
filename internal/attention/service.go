package attention

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

type Service struct {
	store            *state.Store
	clock            clock.Clock
	ownerPrincipalID domain.ID
}

func NewService(store *state.Store, clk clock.Clock, ownerPrincipalID domain.ID) *Service {
	return &Service{
		store:            store,
		clock:            clk,
		ownerPrincipalID: ownerPrincipalID,
	}
}

func (s *Service) isAuthorizedOwner(ctx context.Context, caller domain.ID) bool {
	caller = domain.ID(strings.TrimSpace(string(caller)))
	if caller == "" {
		return false
	}
	if s.ownerPrincipalID != "" && caller == s.ownerPrincipalID {
		return true
	}
	var metaOwner, metaRoot string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT owner_principal_id, constitutional_root_principal_id
		FROM collective_metadata
		WHERE singleton = 1`,
	).Scan(&metaOwner, &metaRoot)
	if err == nil {
		if caller == domain.ID(metaOwner) || caller == domain.ID(metaRoot) {
			return true
		}
	}
	return false
}

type SubmitGroupRequest struct {
	Title                   string           `json:"title"`
	Category                DecisionCategory `json:"category"`
	Urgency                 UrgencyLevel     `json:"urgency"`
	EstimatedAttentionUnits int              `json:"estimated_attention_units"`
	Summary                 string           `json:"summary"`
	EvidenceIDs             []domain.ID      `json:"evidence_ids"`
	SubDecisions            []SubDecision    `json:"sub_decisions"`
	ProposedAction          ProposedAction   `json:"proposed_action"`
}

// SubmitDecisionGroup aggregates related sub-decisions into a single concise evidence packet.
// Evaluates delegation authority: delegated decisions are autonomously resolved; only genuine
// sovereign matters (constitutional, strategic mission, exposure threshold, safety override)
// require Owner attention.
func (s *Service) SubmitDecisionGroup(ctx context.Context, req SubmitGroupRequest) (DecisionPacket, error) {
	if strings.TrimSpace(req.Title) == "" {
		return DecisionPacket{}, errors.New("decision title is required")
	}
	if req.EstimatedAttentionUnits < 0 {
		return DecisionPacket{}, errors.New("estimated attention units cannot be negative")
	}

	if req.EvidenceIDs == nil {
		req.EvidenceIDs = []domain.ID{}
	}
	if req.SubDecisions == nil {
		req.SubDecisions = []SubDecision{}
	}

	sovereignRequired := true
	status := StatusPending
	if req.Category == CategoryDelegatedAutonomous {
		sovereignRequired = false
		status = StatusAutonomouslyResolved
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	packetID := domain.NewID("pkt")

	evidenceIDsJSON, _ := json.Marshal(req.EvidenceIDs)
	subDecisionsJSON, _ := json.Marshal(req.SubDecisions)
	proposedActionJSON, _ := json.Marshal(req.ProposedAction)

	var resolvedAt *time.Time
	var resolvedBy domain.ID
	if !sovereignRequired {
		resolvedAt = &now
		resolvedBy = "system.autonomous_delegation"
	}

	var resolvedAtVal any
	var resolvedByVal any
	if resolvedAt != nil {
		resolvedAtVal = nowStr
		resolvedByVal = string(resolvedBy)
	}

	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO owner_decision_packets (
			packet_id, title, category, urgency, sovereign_required,
			status, estimated_attention_units, summary, evidence_ids_json,
			sub_decisions_json, proposed_action_json, resolved_at, resolved_by_principal_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		packetID, req.Title, req.Category, req.Urgency, sovereignRequired,
		status, req.EstimatedAttentionUnits, req.Summary, string(evidenceIDsJSON),
		string(subDecisionsJSON), string(proposedActionJSON), resolvedAtVal, resolvedByVal,
		nowStr, nowStr,
	)
	if err != nil {
		return DecisionPacket{}, fmt.Errorf("insert decision packet: %w", err)
	}

	return DecisionPacket{
		PacketID:                packetID,
		Title:                   req.Title,
		Category:                req.Category,
		Urgency:                 req.Urgency,
		SovereignRequired:       sovereignRequired,
		Status:                  status,
		EstimatedAttentionUnits: req.EstimatedAttentionUnits,
		Summary:                 req.Summary,
		EvidenceIDs:             req.EvidenceIDs,
		SubDecisions:            req.SubDecisions,
		ProposedAction:          req.ProposedAction,
		ResolvedAt:              resolvedAt,
		ResolvedByPrincipalID:   resolvedBy,
		CreatedAt:               now,
		UpdatedAt:               now,
	}, nil
}

// ResolveDecision allows the sovereign Owner to approve or reject a decision packet,
// automatically debiting the estimated attention units from the daily budget.
func (s *Service) ResolveDecision(ctx context.Context, packetID domain.ID, resolverID domain.ID, resolution PacketStatus) error {
	if resolution != StatusApproved && resolution != StatusRejected {
		return errors.New("resolution must be APPROVED or REJECTED")
	}

	if !s.isAuthorizedOwner(ctx, resolverID) {
		return ErrUnauthorized
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)

	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin resolve tx: %w", err)
	}
	defer tx.Rollback()

	var currentStatus string
	var sovereignRequired int
	var attentionUnits int
	err = tx.QueryRowContext(ctx, `
		SELECT status, sovereign_required, estimated_attention_units
		FROM owner_decision_packets
		WHERE packet_id = ?`,
		packetID,
	).Scan(&currentStatus, &sovereignRequired, &attentionUnits)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPacketNotFound
		}
		return fmt.Errorf("query decision packet: %w", err)
	}

	if currentStatus != string(StatusPending) {
		return ErrAlreadyResolved
	}
	if sovereignRequired == 0 {
		return ErrNotSovereignDecision
	}

	// Read and verify budget ceiling with 24h period rollover
	var dailyBudget, usedBudget int
	var periodStartStr string
	err = tx.QueryRowContext(ctx, `
		SELECT daily_budget_units, used_budget_units, period_start
		FROM owner_attention_budgets
		WHERE singleton = 1`,
	).Scan(&dailyBudget, &usedBudget, &periodStartStr)
	if err != nil {
		return fmt.Errorf("query attention budget: %w", err)
	}

	periodStart, _ := time.Parse(time.RFC3339Nano, periodStartStr)
	if !periodStart.IsZero() && now.Sub(periodStart) >= 24*time.Hour {
		usedBudget = 0
		periodStartStr = nowStr
	}

	if usedBudget+attentionUnits > dailyBudget {
		return ErrAttentionBudgetExceeded
	}

	// Update decision packet status
	_, err = tx.ExecContext(ctx, `
		UPDATE owner_decision_packets
		SET status = ?,
		    resolved_at = ?,
		    resolved_by_principal_id = ?,
		    updated_at = ?
		WHERE packet_id = ?`,
		resolution, nowStr, string(resolverID), nowStr, packetID,
	)
	if err != nil {
		return fmt.Errorf("update packet resolution: %w", err)
	}

	// Debit attention units from the singleton budget
	res, err := tx.ExecContext(ctx, `
		UPDATE owner_attention_budgets
		SET used_budget_units = ?,
		    period_start = ?,
		    updated_at = ?
		WHERE singleton = 1`,
		usedBudget+attentionUnits, periodStartStr, nowStr,
	)
	if err != nil {
		return fmt.Errorf("debit attention budget: %w", err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil || rowsAffected == 0 {
		return fmt.Errorf("failed to update attention budget: rows affected %d", rowsAffected)
	}

	return tx.Commit()
}

// GetAttentionBudget retrieves the current owner attention budget and handles daily period rollover.
func (s *Service) GetAttentionBudget(ctx context.Context) (AttentionBudget, error) {
	now := s.clock.Now().UTC()
	var budget AttentionBudget
	var periodStart, updatedAt string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT daily_budget_units, used_budget_units, period_start, updated_at
		FROM owner_attention_budgets
		WHERE singleton = 1`,
	).Scan(&budget.DailyBudgetUnits, &budget.UsedBudgetUnits, &periodStart, &updatedAt)
	if err != nil {
		return AttentionBudget{}, fmt.Errorf("query attention budget: %w", err)
	}

	budget.PeriodStart, _ = time.Parse(time.RFC3339Nano, periodStart)
	budget.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)

	if !budget.PeriodStart.IsZero() && now.Sub(budget.PeriodStart) >= 24*time.Hour {
		budget.UsedBudgetUnits = 0
		budget.PeriodStart = now
		budget.UpdatedAt = now
		nowStr := domain.FormatTime(now)
		_, _ = s.store.DB().ExecContext(ctx, `
			UPDATE owner_attention_budgets
			SET used_budget_units = 0,
			    period_start = ?,
			    updated_at = ?
			WHERE singleton = 1`,
			nowStr, nowStr,
		)
	}

	return budget, nil
}

// GetOwnerSummary aggregates company health, active risks, invariant pressures, and pending sovereign items.
func (s *Service) GetOwnerSummary(ctx context.Context) (OwnerSummaryProjection, error) {
	now := s.clock.Now().UTC()
	budget, err := s.GetAttentionBudget(ctx)
	if err != nil {
		return OwnerSummaryProjection{}, err
	}

	// 1. Company Health
	var health CompanyHealthStatus
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT mission_id, statement FROM missions WHERE active = 1 ORDER BY created_at DESC LIMIT 1`,
	).Scan(&health.ActiveMissionID, &health.MissionStatement)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query active mission: %w", err)
	}

	err = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM workflow_cases WHERE state != 'CLOSED'`,
	).Scan(&health.ActiveWorkflowCases)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query active workflow cases: %w", err)
	}

	// Check emergency state
	var breakGlassState string
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT break_glass_state FROM emergency_controls WHERE singleton = 1`,
	).Scan(&breakGlassState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query break glass state: %w", err)
	}
	health.BreakGlassFrozen = (breakGlassState == "FROZEN")

	var deadManState string
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT state FROM dead_man_switch WHERE singleton = 1`,
	).Scan(&deadManState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query dead man state: %w", err)
	}
	health.SafeModeActive = (deadManState == "SAFE_MODE")

	// Check cube health
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM cubes WHERE state = 'ACTIVE'`,
	).Scan(&health.HealthyCubes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query active cubes: %w", err)
	}

	err = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM cubes WHERE state = 'QUARANTINED'`,
	).Scan(&health.QuarantinedCubes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query quarantined cubes: %w", err)
	}

	// 2. Pending Sovereign Decision Items
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT packet_id, title, category, urgency, sovereign_required,
		       status, estimated_attention_units, summary, evidence_ids_json,
		       sub_decisions_json, proposed_action_json, created_at, updated_at
		FROM owner_decision_packets
		WHERE status = 'PENDING' AND sovereign_required = 1
		ORDER BY CASE urgency
			WHEN 'CRITICAL' THEN 1
			WHEN 'HIGH' THEN 2
			WHEN 'MEDIUM' THEN 3
			ELSE 4 END, created_at ASC`,
	)
	if err != nil {
		return OwnerSummaryProjection{}, fmt.Errorf("query pending decision packets: %w", err)
	}
	defer rows.Close()

	var pendingItems []DecisionPacket
	for rows.Next() {
		var p DecisionPacket
		var evJSON, subJSON, propJSON, crAt, upAt string
		if scanErr := rows.Scan(
			&p.PacketID, &p.Title, &p.Category, &p.Urgency, &p.SovereignRequired,
			&p.Status, &p.EstimatedAttentionUnits, &p.Summary, &evJSON,
			&subJSON, &propJSON, &crAt, &upAt,
		); scanErr != nil {
			return OwnerSummaryProjection{}, fmt.Errorf("scan decision packet: %w", scanErr)
		}
		_ = json.Unmarshal([]byte(evJSON), &p.EvidenceIDs)
		_ = json.Unmarshal([]byte(subJSON), &p.SubDecisions)
		_ = json.Unmarshal([]byte(propJSON), &p.ProposedAction)
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, crAt)
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, upAt)
		pendingItems = append(pendingItems, p)
	}
	if err := rows.Err(); err != nil {
		return OwnerSummaryProjection{}, fmt.Errorf("iterate decision packets: %w", err)
	}

	// 3. Autonomous resolution count
	var autoResolutions int
	err = s.store.DB().QueryRowContext(ctx, `
		SELECT count(*) FROM owner_decision_packets WHERE status = 'AUTONOMOUSLY_RESOLVED'`,
	).Scan(&autoResolutions)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return OwnerSummaryProjection{}, fmt.Errorf("query auto resolutions: %w", err)
	}

	// 4. Invariant Pressures & Unmitigated Exposure
	var invariantPressures []InvariantPressureMetric
	if health.BreakGlassFrozen {
		invariantPressures = append(invariantPressures, InvariantPressureMetric{
			Name: "EMERGENCY_FREEZE", Level: "CRITICAL", Description: "Collective execution is frozen by Break Glass",
		})
	}
	if health.SafeModeActive {
		invariantPressures = append(invariantPressures, InvariantPressureMetric{
			Name: "DEAD_MAN_SAFE_MODE", Level: "HIGH", Description: "Dead Man Switch expired; strategic spend suspended",
		})
	}
	if health.QuarantinedCubes > 0 {
		invariantPressures = append(invariantPressures, InvariantPressureMetric{
			Name: "CUBE_QUARANTINE", Level: "ELEVATED", Description: fmt.Sprintf("%d cube(s) currently quarantined", health.QuarantinedCubes),
		})
	}

	available := budget.DailyBudgetUnits - budget.UsedBudgetUnits
	if available < 0 {
		available = 0
	}

	return OwnerSummaryProjection{
		GeneratedAt:           now,
		CompanyHealth:         health,
		AttentionBudget:       budget,
		AvailableAttention:    available,
		InvariantPressures:    invariantPressures,
		UnmitigatedExposure:   0,
		PendingSovereignItems: pendingItems,
		AutonomousResolutions: autoResolutions,
	}, nil
}
