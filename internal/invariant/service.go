package invariant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/attention"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/company"
	"github.com/SofiaFlux/summa42/internal/containment"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Service struct {
	store       *state.Store
	clock       clock.Clock
	company     *company.Service
	containment ContainmentEmitter
	attention   AttentionEmitter
	evidence    *evidence.Store
	mu          sync.Mutex
}

func NewService(
	store *state.Store,
	clk clock.Clock,
	companySvc *company.Service,
	containmentEmitter ContainmentEmitter,
	attentionEmitter AttentionEmitter,
	evidenceStore *evidence.Store,
) *Service {
	return &Service{
		store:       store,
		clock:       clk,
		company:     companySvc,
		containment: containmentEmitter,
		attention:   attentionEmitter,
		evidence:    evidenceStore,
	}
}

// RegisterInvariant registers a new declarative business invariant.
func (s *Service) RegisterInvariant(ctx context.Context, inv BusinessInvariant) (BusinessInvariant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	inv.Name = strings.TrimSpace(inv.Name)
	if inv.Name == "" {
		return BusinessInvariant{}, errors.New("invariant name is required")
	}
	if !inv.RuleType.Valid() {
		return BusinessInvariant{}, ErrInvalidRuleType
	}
	if strings.TrimSpace(inv.TargetKind) == "" {
		inv.TargetKind = "COMPANY"
	}
	if inv.Severity == "" {
		inv.Severity = SeverityBreach
	}
	if inv.ActionType == "" {
		inv.ActionType = ActionRemediationTask
	}
	if inv.InvariantID == "" {
		inv.InvariantID = domain.NewID("inv")
	}

	now := s.clock.Now().UTC()
	nowStr := domain.FormatTime(now)
	inv.CreatedAt = now
	inv.UpdatedAt = now
	inv.Enabled = true

	ruleConfigBytes, _ := json.Marshal(inv.RuleConfig)
	actionConfigBytes, _ := json.Marshal(inv.ActionConfig)

	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO business_invariants (
			invariant_id, name, description, target_kind, rule_type,
			rule_config_json, severity, action_type, action_config_json, enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		inv.InvariantID, inv.Name, inv.Description, inv.TargetKind, inv.RuleType,
		string(ruleConfigBytes), string(inv.Severity), string(inv.ActionType),
		string(actionConfigBytes), nowStr, nowStr,
	)
	if err != nil {
		return BusinessInvariant{}, fmt.Errorf("insert business invariant: %w", err)
	}

	return inv, nil
}

// GetInvariant retrieves a business invariant by ID.
func (s *Service) GetInvariant(ctx context.Context, id domain.ID) (*BusinessInvariant, error) {
	var inv BusinessInvariant
	var ruleCfgJSON, actionCfgJSON, createdStr, updatedStr string
	var enabledInt int

	err := s.store.DB().QueryRowContext(ctx, `
		SELECT invariant_id, name, description, target_kind, rule_type,
		       rule_config_json, severity, action_type, action_config_json,
		       enabled, created_at, updated_at
		FROM business_invariants
		WHERE invariant_id = ?`,
		id,
	).Scan(
		&inv.InvariantID, &inv.Name, &inv.Description, &inv.TargetKind, &inv.RuleType,
		&ruleCfgJSON, &inv.Severity, &inv.ActionType, &actionCfgJSON,
		&enabledInt, &createdStr, &updatedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvariantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get invariant: %w", err)
	}

	_ = json.Unmarshal([]byte(ruleCfgJSON), &inv.RuleConfig)
	_ = json.Unmarshal([]byte(actionCfgJSON), &inv.ActionConfig)
	inv.Enabled = enabledInt == 1
	inv.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	inv.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)

	return &inv, nil
}

// ListInvariants returns all registered business invariants.
func (s *Service) ListInvariants(ctx context.Context) ([]*BusinessInvariant, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT invariant_id, name, description, target_kind, rule_type,
		       rule_config_json, severity, action_type, action_config_json,
		       enabled, created_at, updated_at
		FROM business_invariants
		ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list invariants: %w", err)
	}
	defer rows.Close()

	var list []*BusinessInvariant
	for rows.Next() {
		var inv BusinessInvariant
		var ruleCfgJSON, actionCfgJSON, createdStr, updatedStr string
		var enabledInt int

		if err := rows.Scan(
			&inv.InvariantID, &inv.Name, &inv.Description, &inv.TargetKind, &inv.RuleType,
			&ruleCfgJSON, &inv.Severity, &inv.ActionType, &actionCfgJSON,
			&enabledInt, &createdStr, &updatedStr,
		); err != nil {
			return nil, fmt.Errorf("scan invariant: %w", err)
		}

		_ = json.Unmarshal([]byte(ruleCfgJSON), &inv.RuleConfig)
		_ = json.Unmarshal([]byte(actionCfgJSON), &inv.ActionConfig)
		inv.Enabled = enabledInt == 1
		inv.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		inv.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)

		list = append(list, &inv)
	}
	return list, rows.Err()
}

// EvaluateAll executes deterministic evaluation across all enabled business invariants
// and emits structured remediation tasks, escalation notices, and containment signals.
func (s *Service) EvaluateAll(ctx context.Context) ([]*EvaluationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	invariants, err := s.ListInvariants(ctx)
	if err != nil {
		return nil, err
	}

	var results []*EvaluationRecord
	now := s.clock.Now().UTC()

	for _, inv := range invariants {
		if !inv.Enabled {
			continue
		}

		if strings.EqualFold(inv.TargetKind, "COMPANY") {
			var summary company.CompanySummary
			if s.company != nil {
				sm, err := s.company.GetCompanySummary(ctx)
				if err == nil {
					summary = sm
				}
			}

			evalRes, err := EvaluateCompanySummary(inv, &summary)
			if err != nil {
				return nil, fmt.Errorf("evaluate company invariant %s: %w", inv.InvariantID, err)
			}

			rec, err := s.recordAndAct(ctx, inv, "", evalRes, now)
			if err != nil {
				return nil, err
			}
			results = append(results, rec)
			continue
		}

		// Entity-targeted invariant
		entities, err := s.fetchEntitiesForTarget(ctx, inv.TargetKind)
		if err != nil {
			return nil, fmt.Errorf("fetch entities for invariant %s: %w", inv.InvariantID, err)
		}

		for _, ent := range entities {
			entVal := ent
			evalRes, err := EvaluateEntity(inv, &entVal, now)
			if err != nil {
				return nil, fmt.Errorf("evaluate entity %s for invariant %s: %w", ent.EntityID, inv.InvariantID, err)
			}

			rec, err := s.recordAndAct(ctx, inv, ent.EntityID, evalRes, now)
			if err != nil {
				return nil, err
			}
			results = append(results, rec)
		}
	}

	return results, nil
}

func (s *Service) fetchEntitiesForTarget(ctx context.Context, targetKind string) ([]company.Entity, error) {
	if s.company == nil {
		return nil, nil
	}
	if targetKind == "*" || strings.EqualFold(targetKind, "ALL") {
		return s.company.ListEntities(ctx, company.EntityFilter{})
	}
	kind := company.EntityKind(targetKind)
	return s.company.ListEntities(ctx, company.EntityFilter{Kind: &kind})
}

func (s *Service) recordAndAct(
	ctx context.Context,
	inv *BusinessInvariant,
	entityID domain.ID,
	res EvaluationResult,
	now time.Time,
) (*EvaluationRecord, error) {
	evalID := domain.NewID("eval")
	nowStr := domain.FormatTime(now)

	rec := &EvaluationRecord{
		EvaluationID:  evalID,
		InvariantID:   inv.InvariantID,
		EntityID:      entityID,
		Status:        res.Status,
		ObservedValue: res.ObservedValue,
		EvaluatedAt:   now,
	}

	if res.Status == StatusNominal {
		// Log nominal evaluation
		obsBytes, _ := json.Marshal(res.ObservedValue)
		_, err := s.store.DB().ExecContext(ctx, `
			INSERT INTO invariant_evaluations (
				evaluation_id, invariant_id, entity_id, status, observed_value_json,
				evidence_id, triggered_task_id, triggered_packet_id, containment_signal, evaluated_at
			) VALUES (?, ?, ?, 'OK', ?, '', '', '', '', ?)`,
			evalID, inv.InvariantID, entityID, string(obsBytes), nowStr,
		)
		if err != nil {
			return nil, fmt.Errorf("record nominal evaluation: %w", err)
		}
		return rec, nil
	}

	// 1. Generate Verifiable Deterministic Evidence Object
	evPayload := map[string]any{
		"evaluation_id":  evalID,
		"invariant_id":   inv.InvariantID,
		"invariant_name": inv.Name,
		"entity_id":      entityID,
		"rule_type":      inv.RuleType,
		"status":         res.Status,
		"is_severe":      res.IsSevere,
		"observed_value": res.ObservedValue,
		"reason":         res.Reason,
		"evaluated_at":   nowStr,
	}
	evBytes, _ := json.Marshal(evPayload)

	var evidenceID domain.ID
	if s.evidence != nil {
		evObj, err := s.evidence.Put(ctx, bytes.NewReader(evBytes), evidence.Metadata{
			Subject:   string(evalID),
			MediaType: "application/json",
			Kind:      "INVARIANT_EVALUATION",
		})
		if err == nil {
			evidenceID = evObj.ID
		}
	}
	if evidenceID == "" {
		hash := sha256.Sum256(evBytes)
		evidenceID = domain.ID("ev_" + hex.EncodeToString(hash[:12]))
	}
	rec.EvidenceID = evidenceID

	// 2. Synthesize Governed Remediation Task (Idempotent)
	if inv.ActionType == ActionRemediationTask || inv.ActionType == ActionCombined {
		taskID, err := s.ensureRemediationTask(ctx, inv, entityID, res, evidenceID, now)
		if err != nil {
			return nil, fmt.Errorf("ensure remediation task: %w", err)
		}
		rec.TriggeredTaskID = taskID
	}

	// 3. Emit Escalation Notice (Attention Packet for sovereign Owner)
	if s.attention != nil && (inv.ActionType == ActionEscalationNotice || inv.ActionType == ActionCombined || res.IsSevere) {
		packetID, err := s.ensureEscalationPacket(ctx, inv, entityID, res, evidenceID)
		if err != nil {
			return nil, fmt.Errorf("ensure escalation packet: %w", err)
		}
		rec.TriggeredPacketID = packetID
	}

	// 4. Emit Containment Signal under severe exposure
	if s.containment != nil && (res.IsSevere || inv.ActionType == ActionContainmentAlert || inv.ActionType == ActionCombined) {
		step := inv.ActionConfig.ContainmentStep
		if step == "" {
			if res.IsSevere {
				step = containment.StepRestrict
			} else {
				step = containment.StepWarn
			}
		}
		signalName := inv.ActionConfig.ContainmentSignalName
		if signalName == "" {
			signalName = fmt.Sprintf("INVARIANT_BREACH_%s", inv.InvariantID)
		}

		err := s.containment.ApplySignal(ctx, "", rec.TriggeredTaskID, signalName, "INVARIANT_ENGINE", step, map[string]any{
			"invariant_id": inv.InvariantID,
			"entity_id":    entityID,
			"reason":       res.Reason,
			"observed":     res.ObservedValue,
			"is_severe":    res.IsSevere,
		})
		if err == nil {
			rec.ContainmentSignal = string(step)
		}
	}

	// Record evaluation in database
	obsBytes, _ := json.Marshal(res.ObservedValue)
	_, err := s.store.DB().ExecContext(ctx, `
		INSERT INTO invariant_evaluations (
			evaluation_id, invariant_id, entity_id, status, observed_value_json,
			evidence_id, triggered_task_id, triggered_packet_id, containment_signal, evaluated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evalID, inv.InvariantID, entityID, string(res.Status), string(obsBytes),
		rec.EvidenceID, rec.TriggeredTaskID, rec.TriggeredPacketID, rec.ContainmentSignal, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("record breach evaluation: %w", err)
	}

	return rec, nil
}

func (s *Service) ensureRemediationTask(
	ctx context.Context,
	inv *BusinessInvariant,
	entityID domain.ID,
	res EvaluationResult,
	evidenceID domain.ID,
	now time.Time,
) (domain.ID, error) {
	idempotencyKey := fmt.Sprintf("inv_remed_%s_%s", inv.InvariantID, entityID)

	// Check if active remediation task already exists
	var existingTaskID, existingState string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT task_id, state FROM tasks WHERE idempotency_key = ?`, idempotencyKey,
	).Scan(&existingTaskID, &existingState)

	if err == nil {
		// If task exists and is not in terminal failed/expired state, return it idempotently
		if existingState != string(domain.TaskFailed) && existingState != string(domain.TaskCancelled) && existingState != string(domain.TaskExpired) {
			return domain.ID(existingTaskID), nil
		}
	}

	taskID := domain.NewID("task_remed")
	nowStr := domain.FormatTime(now)
	deadlineStr := domain.FormatTime(now.Add(24 * time.Hour))

	statement := inv.ActionConfig.RemediationTaskStatement
	if statement == "" {
		statement = fmt.Sprintf("Remediate business invariant breach: %s (%s)", inv.Name, res.Reason)
	}

	criteria := inv.ActionConfig.RemediationAcceptanceCriteria
	if len(criteria) == 0 {
		criteria = []string{fmt.Sprintf("Restore invariant %s to nominal state", inv.Name)}
	}

	criteriaJSON, _ := json.Marshal(criteria)
	capabilitiesJSON, _ := json.Marshal([]string{})
	authorityJSON, _ := json.Marshal([]string{})
	payloadJSON, _ := json.Marshal(map[string]any{
		"invariant_id": inv.InvariantID,
		"entity_id":    entityID,
		"evidence_id":  evidenceID,
		"reason":       res.Reason,
	})

	_, err = s.store.DB().ExecContext(ctx, `
		INSERT INTO tasks (
			task_id, purpose_kind, purpose_id, state, acceptance_criteria_json,
			required_capabilities_json, required_enforcement, authority_ceiling_json,
			resource_envelope_id, priority, deadline, objective, payload_json,
			idempotency_key, created_at, updated_at
		) VALUES (
			?, 'RECOVERY', ?, 'ELIGIBLE', ?,
			?, 'ENFORCED', ?,
			'envelope_default', 950, ?, ?, ?,
			?, ?, ?
		)`,
		taskID, inv.InvariantID, string(criteriaJSON),
		string(capabilitiesJSON), string(authorityJSON),
		deadlineStr, statement, string(payloadJSON),
		idempotencyKey, nowStr, nowStr,
	)
	if err != nil {
		return "", fmt.Errorf("insert remediation task: %w", err)
	}

	return taskID, nil
}

func (s *Service) ensureEscalationPacket(
	ctx context.Context,
	inv *BusinessInvariant,
	entityID domain.ID,
	res EvaluationResult,
	evidenceID domain.ID,
) (domain.ID, error) {
	// Check if pending decision packet for this invariant already exists
	dedupTag := fmt.Sprintf("INVARIANT:%s:%s", inv.InvariantID, entityID)
	var existingPacketID string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT packet_id FROM owner_decision_packets
		WHERE summary LIKE ? AND status = 'PENDING' LIMIT 1`,
		"%"+dedupTag+"%",
	).Scan(&existingPacketID)

	if err == nil && existingPacketID != "" {
		return domain.ID(existingPacketID), nil
	}

	category := inv.ActionConfig.AttentionCategory
	if category == "" {
		category = attention.CategoryExposureThreshold
	}

	urgency := inv.ActionConfig.AttentionUrgency
	if urgency == "" {
		if res.IsSevere {
			urgency = attention.UrgencyCritical
		} else {
			urgency = attention.UrgencyHigh
		}
	}

	pkt, err := s.attention.SubmitDecisionGroup(ctx, attention.SubmitGroupRequest{
		Title:                   fmt.Sprintf("Business Invariant Alert: %s", inv.Name),
		Category:                category,
		Urgency:                 urgency,
		EstimatedAttentionUnits: 15,
		Summary:                 fmt.Sprintf("[%s] %s", dedupTag, res.Reason),
		EvidenceIDs:             []domain.ID{evidenceID},
		ProposedAction: attention.ProposedAction{
			ActionType: "REMEDIATE_INVARIANT",
			Parameters: map[string]any{
				"invariant_id": inv.InvariantID,
				"entity_id":    entityID,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("submit attention packet: %w", err)
	}

	return pkt.PacketID, nil
}
