package operations

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
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/resources"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type PrepareRequest struct {
	AttemptID         domain.ID
	Provider          string
	TrustedSlotKey    string
	Intent            IntentDescriptor
	Risk              string
	Attributes        map[string]any
	RequiredApprovals []domain.ID
}

type EffectSlot struct {
	ID                domain.ID
	CollectiveID      domain.ID
	TaskID            domain.ID
	TrustedSlotKey    string
	Provider          string
	DescriptorType    string
	IntentFingerprint string
	IntentRevision    int64
	CanonicalIntent   []byte
	AdapterVersion    string
	AdapterSemantic   bool
}

type Service struct {
	store        *state.Store
	clock        clock.Clock
	execution    *execution.Service
	policy       policy.PolicyEngine
	resources    *resources.Service
	approvals    *approvals.Service
	collectiveID domain.ID
	providers    map[string]Provider
}

func New(store *state.Store, clk clock.Clock, executionSvc *execution.Service, policyEngine policy.PolicyEngine, resourceSvc *resources.Service, approvalSvc *approvals.Service, collectiveID domain.ID, providers ...Provider) *Service {
	registry := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		if provider != nil && strings.TrimSpace(provider.Name()) != "" {
			registry[provider.Name()] = provider
		}
	}
	return &Service{
		store: store, clock: clk, execution: executionSvc, policy: policyEngine,
		resources: resourceSvc, approvals: approvalSvc, collectiveID: collectiveID, providers: registry,
	}
}

func (s *Service) Prepare(ctx context.Context, request PrepareRequest) (domain.ExternalOperation, error) {
	return s.PrepareWithGuard(ctx, request, nil)
}

// PrepareWithGuard checks a caller's canonical contract before reserving effects.
func (s *Service) PrepareWithGuard(ctx context.Context, request PrepareRequest, guard execution.TaskGuard) (domain.ExternalOperation, error) {
	if err := s.configured(); err != nil {
		return domain.ExternalOperation{}, err
	}
	provider, err := s.validatePrepareRequest(request)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	callerRequired := normalizeApprovalIDs(request.RequiredApprovals)
	canonical, fingerprint, err := canonicalIntent(provider, request.Intent)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	cost, err := provider.CostProfile(request.Intent)
	if err != nil {
		return domain.ExternalOperation{}, fmt.Errorf("provider cost profile: %w", err)
	}
	if cost.MaxExposure <= 0 {
		return domain.ExternalOperation{}, errors.New("provider cost profile must declare positive maximum exposure")
	}

	control, err := s.controlForAttempt(ctx, request.AttemptID)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	now := s.clock.Now().UTC()
	authorityValid := hasString(control.AuthorityCeiling, provider.Capability()) && enforcementSatisfies(provider.EnforcementLevel(), control.RequiredEnforcement)
	decision, err := s.evaluatePolicy(ctx, policyContext{
		Now: now, Risk: request.Risk, AuthorityValid: authorityValid,
		TaskID: control.TaskID, AttemptID: request.AttemptID, Provider: provider,
		DescriptorType: request.Intent.DescriptorType(), Phase: "PREPARE", Attributes: request.Attributes,
	})
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	if err := validateDecisionProvenance(decision, now); err != nil {
		return domain.ExternalOperation{}, err
	}
	gate, err := evaluatePrepareOutcome(decision)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	if (gate.RequiresApproval || len(callerRequired) > 0) && len(requiredApproversFor(decision, callerRequired)) == 0 {
		return domain.ExternalOperation{}, fmt.Errorf("%w: approval required but no required approver is defined", domain.ErrPolicyDenied)
	}

	var result domain.ExternalOperation
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		guarded, err := s.execution.GuardAttempt(ctx, tx, request.AttemptID, domain.TaskExecuting)
		if err != nil {
			return err
		}
		if guarded.TaskID != control.TaskID {
			return domain.ErrStaleAttempt
		}
		current, err := loadTaskControlTx(ctx, tx, guarded.TaskID)
		if err != nil {
			return err
		}
		if !hasString(current.AuthorityCeiling, provider.Capability()) || !enforcementSatisfies(provider.EnforcementLevel(), current.RequiredEnforcement) {
			return domain.ErrPolicyDenied
		}
		if err := validateDecisionProvenance(decision, now); err != nil {
			return err
		}
		if _, err := evaluatePrepareOutcome(decision); err != nil {
			return err
		}

		slot, err := s.ResolveEffectSlot(ctx, tx, guarded.TaskID, request.TrustedSlotKey, provider, request.Intent.DescriptorType(), fingerprint, canonical)
		if err != nil {
			result.EffectSlotID = slot.ID
			return err
		}

		latest, err := loadLatestOperationForSlotTx(ctx, tx, slot.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			switch latest.State {
			case domain.OperationDispatched, domain.OperationOutcomeUnknown, domain.OperationConfirmedEffect:
				result = latest
				return nil
			case domain.OperationPrepared:
				if latest.AttemptID == request.AttemptID {
					matches, matchErr := s.preparedApprovalMatchesInTx(
						ctx, tx, latest, decision, callerRequired, provider,
						request.Intent.DescriptorType(), canonical,
					)
					if matchErr != nil {
						return matchErr
					}
					if matches {
						result = latest
						return nil
					}
				}
				if err := s.resources.ReleaseInTx(ctx, tx, latest.ReservationID); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE external_operations SET state = ?, settled_at = ? WHERE operation_id = ? AND state = ?`,
					domain.OperationCancelled, formatTime(now), latest.ID, domain.OperationPrepared,
				); err != nil {
					return err
				}
			case domain.OperationConfirmedNoEffect, domain.OperationCancelled:
				// A proven no-effect or cancelled preparation may be retried in the same effect slot.
			default:
				return fmt.Errorf("unsupported previous operation state %s", latest.State)
			}
		}

		reservation, err := s.resources.ReserveInTx(ctx, tx, current.ResourceEnvelopeID, cost.MaxExposure, cost.Enforceability)
		if err != nil {
			return err
		}
		sequence, err := nextOperationSequence(ctx, tx, slot.ID)
		if err != nil {
			return err
		}
		result = domain.ExternalOperation{
			ID: domain.NewID("operation"), TaskID: guarded.TaskID, AttemptID: request.AttemptID,
			EffectSlotID: slot.ID, State: domain.OperationPrepared, IntentFingerprint: fingerprint,
			IntentRevision: slot.IntentRevision, Provider: provider.Name(), ReservationID: reservation.ID, CreatedAt: now,
		}
		approvalID, callerJSON, err := s.prepareApprovalInTx(
			ctx, tx, result, guarded.LeaseExpiresAt, decision, callerRequired,
			provider, request.Intent.DescriptorType(), canonical,
		)
		if err != nil {
			return err
		}
		result.ApprovalID = approvalID
		var approvalValue any
		if result.ApprovalID != "" {
			approvalValue = result.ApprovalID
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO external_operations(
				operation_id, task_id, attempt_id, effect_slot_id, operation_sequence, state,
				intent_fingerprint, intent_revision, provider, adapter_version, reservation_id, risk,
				prepare_policy_decision_id, prepare_policy_set_id, prepare_policy_set_hash,
				prepare_policy_capabilities_hash, approval_id, caller_required_approvers_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.ID, result.TaskID, result.AttemptID, result.EffectSlotID, sequence, result.State,
			result.IntentFingerprint, result.IntentRevision, result.Provider, provider.AdapterVersion(), result.ReservationID,
			strings.TrimSpace(request.Risk), decision.ID, decision.PolicySetID, decision.PolicySetHash,
			decision.PolicyCapabilitiesHash, approvalValue, callerJSON, formatTime(now),
		)
		if err != nil {
			return err
		}
		return appendOperationEvent(ctx, tx, result.TaskID, result.AttemptID, "OPERATION_PREPARED", result.ID, now)
	})
	return result, mapPolicyProfileError(err)
}

// ResolveEffectSlot resolves the durable identity for one trusted semantic effect.
// Callers must invoke it inside a transaction that has already passed GuardAttempt.
func (s *Service) ResolveEffectSlot(ctx context.Context, tx *sql.Tx, taskID domain.ID, trustedSlotKey string, provider Provider, descriptorType, fingerprint string, canonical []byte) (EffectSlot, error) {
	if tx == nil {
		return EffectSlot{}, errors.New("effect-slot resolution requires transaction")
	}
	trustedSlotKey = strings.TrimSpace(trustedSlotKey)
	if taskID == "" || trustedSlotKey == "" || provider == nil || fingerprint == "" || len(canonical) == 0 {
		return EffectSlot{}, errors.New("task, trusted slot key, provider, fingerprint, and canonical intent are required")
	}
	var slot EffectSlot
	var semantic int
	var canonicalStored string
	err := tx.QueryRowContext(ctx, `
		SELECT effect_slot_id, collective_id, task_id, trusted_slot_key, provider, descriptor_type,
		       intent_fingerprint, intent_revision, canonical_intent_json, adapter_version, adapter_version_semantic
		FROM effect_slots
		WHERE collective_id = ? AND task_id = ? AND trusted_slot_key = ?`,
		s.collectiveID, taskID, trustedSlotKey,
	).Scan(&slot.ID, &slot.CollectiveID, &slot.TaskID, &slot.TrustedSlotKey, &slot.Provider, &slot.DescriptorType,
		&slot.IntentFingerprint, &slot.IntentRevision, &canonicalStored, &slot.AdapterVersion, &semantic)
	if err == nil {
		slot.AdapterSemantic = semantic == 1
		slot.CanonicalIntent = []byte(canonicalStored)
		if slot.Provider != provider.Name() || slot.DescriptorType != descriptorType || slot.IntentFingerprint != fingerprint {
			return slot, fmt.Errorf("%w: trusted effect slot %q already binds a different semantic intent", domain.ErrIntentConflict, trustedSlotKey)
		}
		return slot, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EffectSlot{}, err
	}

	now := s.clock.Now().UTC()
	slot = EffectSlot{
		ID: domain.NewID("effectslot"), CollectiveID: s.collectiveID, TaskID: taskID,
		TrustedSlotKey: trustedSlotKey, Provider: provider.Name(), DescriptorType: descriptorType,
		IntentFingerprint: fingerprint, IntentRevision: 1, CanonicalIntent: append([]byte(nil), canonical...),
		AdapterVersion: provider.AdapterVersion(), AdapterSemantic: provider.AdapterVersionSemanticallyRelevant(),
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO effect_slots(
			effect_slot_id, collective_id, task_id, trusted_slot_key, provider, descriptor_type,
			intent_fingerprint, intent_revision, canonical_intent_json, adapter_version,
			adapter_version_semantic, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		slot.ID, slot.CollectiveID, slot.TaskID, slot.TrustedSlotKey, slot.Provider, slot.DescriptorType,
		slot.IntentFingerprint, slot.IntentRevision, string(slot.CanonicalIntent), slot.AdapterVersion,
		boolInt(slot.AdapterSemantic), formatTime(now), formatTime(now),
	)
	return slot, err
}

func (s *Service) Dispatch(ctx context.Context, operationID, attemptID domain.ID) (domain.ExternalOperation, error) {
	return s.DispatchWithGuard(ctx, operationID, attemptID, nil)
}

// DispatchWithGuard checks canonical authority both on replay and at the commit boundary.
func (s *Service) DispatchWithGuard(ctx context.Context, operationID, attemptID domain.ID, guard execution.TaskGuard) (domain.ExternalOperation, error) {
	if err := s.configured(); err != nil {
		return domain.ExternalOperation{}, err
	}
	if operationID == "" || attemptID == "" {
		return domain.ExternalOperation{}, errors.New("operation id and current attempt id are required")
	}
	op, err := s.loadOperation(ctx, operationID)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	if guard != nil {
		if err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
			current, err := s.execution.GuardAttempt(ctx, tx, attemptID, domain.TaskExecuting)
			if err != nil {
				return err
			}
			if current.TaskID != op.TaskID {
				return domain.ErrStaleAttempt
			}
			return guard(ctx, tx)
		}); err != nil {
			return op, err
		}
	}
	switch op.State {
	case domain.OperationConfirmedEffect, domain.OperationConfirmedNoEffect:
		return op, nil
	case domain.OperationCancelled:
		return op, errors.New("cancelled operation cannot be dispatched")
	case domain.OperationDispatched, domain.OperationOutcomeUnknown:
		if err := s.guardAttemptForTask(ctx, attemptID, op.TaskID); err != nil {
			return op, err
		}
		return s.reconcile(ctx, operationID)
	case domain.OperationPrepared:
		if op.AttemptID != attemptID {
			return op, domain.ErrStaleAttempt
		}
	default:
		return op, fmt.Errorf("unsupported operation state %s", op.State)
	}

	provider, err := s.providerFor(op.Provider)
	if err != nil {
		return op, err
	}
	control, err := s.controlForAttempt(ctx, attemptID)
	if err != nil {
		return op, err
	}
	if control.TaskID != op.TaskID {
		return op, domain.ErrStaleAttempt
	}
	now := s.clock.Now().UTC()
	authorityValid := hasString(control.AuthorityCeiling, provider.Capability()) && enforcementSatisfies(provider.EnforcementLevel(), control.RequiredEnforcement)
	descriptorType, err := controlDescriptorType(ctx, s.store.DB(), op.EffectSlotID)
	if err != nil {
		return op, err
	}
	risk, err := controlRisk(ctx, s.store.DB(), operationID)
	if err != nil {
		return op, err
	}
	decision, evalErr := s.evaluatePolicy(ctx, policyContext{
		Now: now, Risk: risk, AuthorityValid: authorityValid,
		TaskID: op.TaskID, AttemptID: attemptID, Provider: provider,
		DescriptorType: descriptorType, Phase: "DISPATCH",
	})
	if evalErr != nil {
		return op, evalErr
	}
	if err := validateDecisionProvenance(decision, now); err != nil || validateDispatchOutcome(decision) != nil || !authorityValid {
		if cancelErr := s.cancelPrepared(ctx, op, attemptID); cancelErr != nil {
			return op, cancelErr
		}
		cancelled, loadErr := s.loadOperation(ctx, operationID)
		if loadErr != nil {
			return op, loadErr
		}
		return cancelled, domain.ErrPolicyDenied
	}

	claim := domain.NewID("dispatch")
	var dispatchRequest ProviderDispatchRequest
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if guard != nil {
			if err := guard(ctx, tx); err != nil {
				return err
			}
		}
		guarded, err := s.execution.GuardAttempt(ctx, tx, attemptID, domain.TaskExecuting)
		if err != nil {
			return err
		}
		if guarded.TaskID != op.TaskID {
			return domain.ErrStaleAttempt
		}
		currentControl, err := loadTaskControlTx(ctx, tx, guarded.TaskID)
		if err != nil {
			return err
		}
		if !hasString(currentControl.AuthorityCeiling, provider.Capability()) || !enforcementSatisfies(provider.EnforcementLevel(), currentControl.RequiredEnforcement) {
			return domain.ErrPolicyDenied
		}
		if err := validateDecisionProvenance(decision, now); err != nil {
			return err
		}
		if err := validateDispatchOutcome(decision); err != nil {
			return err
		}
		current, err := loadOperationTx(ctx, tx, operationID)
		if err != nil {
			return err
		}
		if current.State != domain.OperationPrepared || current.AttemptID != attemptID {
			return domain.ErrStaleAttempt
		}
		var cancelRequested int
		var reservationState domain.ReservationState
		var slotFingerprint string
		var slotRevision int64
		var canonical string
		var callerRequiredJSON string
		if err := tx.QueryRowContext(ctx, `
			SELECT o.cancel_requested, r.state, s.intent_fingerprint, s.intent_revision, s.canonical_intent_json,
			       o.caller_required_approvers_json
			FROM external_operations o
			JOIN resource_reservations r ON r.reservation_id = o.reservation_id
			JOIN effect_slots s ON s.effect_slot_id = o.effect_slot_id
			WHERE o.operation_id = ?`, operationID,
		).Scan(&cancelRequested, &reservationState, &slotFingerprint, &slotRevision, &canonical, &callerRequiredJSON); err != nil {
			return err
		}
		if cancelRequested != 0 || reservationState != domain.ReservationHeld {
			return domain.ErrPolicyDenied
		}
		if slotFingerprint != current.IntentFingerprint || slotRevision != current.IntentRevision {
			return domain.ErrIntentConflict
		}
		callerRequired, err := decodeApprovalIDs(callerRequiredJSON)
		if err != nil {
			return fmt.Errorf("decode caller required approvals: %w", err)
		}
		if err := s.validateAndConsumeApprovalInTx(
			ctx, tx, current, decision, callerRequired, provider, descriptorType, []byte(canonical),
		); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE external_operations
			SET state = ?, dispatcher_claim = ?, dispatch_policy_decision_id = ?, dispatch_policy_set_id = ?,
			    dispatch_policy_set_hash = ?, dispatch_policy_capabilities_hash = ?, dispatched_at = ?
			WHERE operation_id = ? AND state = ? AND dispatcher_claim IS NULL AND cancel_requested = 0`,
			domain.OperationDispatched, claim, decision.ID, decision.PolicySetID, decision.PolicySetHash,
			decision.PolicyCapabilitiesHash, formatTime(now), operationID, domain.OperationPrepared,
		)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return domain.ErrStaleAttempt
		}
		dispatchRequest = ProviderDispatchRequest{
			OperationID: current.ID, TaskID: current.TaskID, EffectSlotID: current.EffectSlotID,
			IntentFingerprint: current.IntentFingerprint, IntentRevision: current.IntentRevision,
			CanonicalIntent: []byte(canonical),
		}
		return appendOperationEvent(ctx, tx, current.TaskID, current.AttemptID, "OPERATION_DISPATCH_COMMITTED", current.ID, now)
	})
	if err != nil {
		return op, mapPolicyProfileError(err)
	}

	outcome, dispatchErr := provider.Dispatch(ctx, dispatchRequest)
	if dispatchErr != nil {
		unknown, markErr := s.markUnknown(ctx, operationID)
		if markErr != nil {
			return unknown, markErr
		}
		return unknown, fmt.Errorf("%w: provider dispatch acknowledgement unavailable: %v", domain.ErrOutcomeUnknown, dispatchErr)
	}
	if outcome.State == domain.OperationOutcomeUnknown {
		unknown, markErr := s.markUnknown(ctx, operationID)
		if markErr != nil {
			return unknown, markErr
		}
		return unknown, domain.ErrOutcomeUnknown
	}
	if outcome.State != domain.OperationConfirmedEffect && outcome.State != domain.OperationConfirmedNoEffect {
		unknown, markErr := s.markUnknown(ctx, operationID)
		if markErr != nil {
			return unknown, markErr
		}
		return unknown, fmt.Errorf("%w: provider returned invalid outcome state %s", domain.ErrOutcomeUnknown, outcome.State)
	}
	return s.SettleOutcome(ctx, operationID, outcome)
}

func (s *Service) SettleOutcome(ctx context.Context, operationID domain.ID, outcome ProviderOutcome) (domain.ExternalOperation, error) {
	if err := s.configured(); err != nil {
		return domain.ExternalOperation{}, err
	}
	if outcome.State != domain.OperationConfirmedEffect && outcome.State != domain.OperationConfirmedNoEffect {
		return domain.ExternalOperation{}, errors.New("settlement requires confirmed effect or confirmed no-effect")
	}
	if outcome.ActualCost < 0 {
		return domain.ExternalOperation{}, errors.New("actual cost cannot be negative")
	}
	now := s.clock.Now().UTC()
	var settled domain.ExternalOperation
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := loadOperationTx(ctx, tx, operationID)
		if err != nil {
			return err
		}
		if current.State == outcome.State {
			var storedReference string
			var storedCost sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(provider_reference, ''), actual_cost FROM external_operations WHERE operation_id = ?`, operationID,
			).Scan(&storedReference, &storedCost); err != nil {
				return err
			}
			if !storedCost.Valid || storedCost.Int64 != outcome.ActualCost || storedReference != strings.TrimSpace(outcome.ProviderReference) {
				return errors.New("conflicting repeated operation settlement")
			}
			current.ProviderReference = storedReference
			settled = current
			return nil
		}
		if current.State != domain.OperationDispatched && current.State != domain.OperationOutcomeUnknown {
			return fmt.Errorf("cannot settle operation from state %s", current.State)
		}
		if err := s.resources.SettleInTx(ctx, tx, current.ReservationID, outcome.ActualCost); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE external_operations
			SET state = ?, provider_reference = ?, reconciliation_required = 0, actual_cost = ?, settled_at = ?
			WHERE operation_id = ?`,
			outcome.State, strings.TrimSpace(outcome.ProviderReference), outcome.ActualCost, formatTime(now), operationID,
		); err != nil {
			return err
		}
		current.State = outcome.State
		current.ProviderReference = strings.TrimSpace(outcome.ProviderReference)
		current.SettledAt = now
		settled = current
		return appendOperationEvent(ctx, tx, current.TaskID, current.AttemptID, "OPERATION_OUTCOME_SETTLED", current.ID, now)
	})
	return settled, err
}

func (s *Service) reconcile(ctx context.Context, operationID domain.ID) (domain.ExternalOperation, error) {
	op, err := s.loadOperation(ctx, operationID)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	switch op.State {
	case domain.OperationConfirmedEffect, domain.OperationConfirmedNoEffect:
		return op, nil
	case domain.OperationCancelled:
		return op, errors.New("cancelled operation has no outcome to reconcile")
	case domain.OperationPrepared:
		return op, errors.New("prepared operation must be revalidated at dispatch; reconciler will not send it")
	case domain.OperationDispatched:
		op, err = s.markUnknown(ctx, operationID)
		if err != nil {
			return op, err
		}
	case domain.OperationOutcomeUnknown:
		// already conservatively classified
	default:
		return op, fmt.Errorf("unsupported reconciliation state %s", op.State)
	}

	provider, err := s.providerFor(op.Provider)
	if err != nil {
		return op, err
	}
	request, err := s.providerRequest(ctx, operationID)
	if err != nil {
		return op, err
	}
	outcome, lookupErr := provider.LookupOutcome(ctx, request)
	if lookupErr != nil || outcome.State == domain.OperationOutcomeUnknown {
		unknown, markErr := s.markUnknown(ctx, operationID)
		if markErr != nil {
			return unknown, markErr
		}
		if lookupErr != nil {
			return unknown, fmt.Errorf("%w: provider lookup did not establish outcome: %v", domain.ErrOutcomeUnknown, lookupErr)
		}
		return unknown, domain.ErrOutcomeUnknown
	}
	if outcome.State != domain.OperationConfirmedEffect && outcome.State != domain.OperationConfirmedNoEffect {
		return op, fmt.Errorf("%w: provider lookup returned invalid state %s", domain.ErrOutcomeUnknown, outcome.State)
	}
	return s.SettleOutcome(ctx, operationID, outcome)
}

func (s *Service) markUnknown(ctx context.Context, operationID domain.ID) (domain.ExternalOperation, error) {
	now := s.clock.Now().UTC()
	var unknown domain.ExternalOperation
	err := s.store.WithTx(ctx, func(tx *sql.Tx) error {
		current, err := loadOperationTx(ctx, tx, operationID)
		if err != nil {
			return err
		}
		if current.State == domain.OperationConfirmedEffect || current.State == domain.OperationConfirmedNoEffect {
			unknown = current
			return nil
		}
		if current.State != domain.OperationDispatched && current.State != domain.OperationOutcomeUnknown {
			return fmt.Errorf("cannot mark operation unknown from state %s", current.State)
		}
		if err := s.resources.MarkUnresolvedInTx(ctx, tx, current.ReservationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE external_operations SET state = ?, reconciliation_required = 1 WHERE operation_id = ?`,
			domain.OperationOutcomeUnknown, operationID,
		); err != nil {
			return err
		}
		current.State = domain.OperationOutcomeUnknown
		unknown = current
		return appendOperationEvent(ctx, tx, current.TaskID, current.AttemptID, "OPERATION_OUTCOME_UNKNOWN", current.ID, now)
	})
	return unknown, err
}

func (s *Service) cancelPrepared(ctx context.Context, op domain.ExternalOperation, attemptID domain.ID) error {
	now := s.clock.Now().UTC()
	return s.execution.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(tx *sql.Tx, guarded execution.GuardedAttempt) error {
		if guarded.TaskID != op.TaskID {
			return domain.ErrStaleAttempt
		}
		current, err := loadOperationTx(ctx, tx, op.ID)
		if err != nil {
			return err
		}
		if current.State != domain.OperationPrepared {
			return nil
		}
		if err := s.resources.ReleaseInTx(ctx, tx, current.ReservationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE external_operations SET state = ?, settled_at = ? WHERE operation_id = ? AND state = ?`,
			domain.OperationCancelled, formatTime(now), current.ID, domain.OperationPrepared,
		); err != nil {
			return err
		}
		return appendOperationEvent(ctx, tx, current.TaskID, current.AttemptID, "OPERATION_CANCELLED_BEFORE_DISPATCH", current.ID, now)
	})
}

func (s *Service) guardAttemptForTask(ctx context.Context, attemptID, taskID domain.ID) error {
	return s.execution.WithGuardedAttempt(ctx, attemptID, []domain.TaskState{domain.TaskExecuting}, func(_ *sql.Tx, guarded execution.GuardedAttempt) error {
		if guarded.TaskID != taskID {
			return domain.ErrStaleAttempt
		}
		return nil
	})
}

type taskControl struct {
	TaskID              domain.ID
	ResourceEnvelopeID  domain.ID
	RequiredEnforcement domain.EnforcementLevel
	AuthorityCeiling    []string
}

func (s *Service) controlForAttempt(ctx context.Context, attemptID domain.ID) (taskControl, error) {
	var control taskControl
	var authorityJSON string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT t.task_id, t.resource_envelope_id, t.required_enforcement, t.authority_ceiling_json
		FROM attempts a JOIN tasks t ON t.task_id = a.task_id
		WHERE a.attempt_id = ?`, attemptID,
	).Scan(&control.TaskID, &control.ResourceEnvelopeID, &control.RequiredEnforcement, &authorityJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return taskControl{}, domain.ErrStaleAttempt
	}
	if err != nil {
		return taskControl{}, err
	}
	if err := json.Unmarshal([]byte(authorityJSON), &control.AuthorityCeiling); err != nil {
		return taskControl{}, fmt.Errorf("decode authority ceiling: %w", err)
	}
	return control, nil
}

func loadTaskControlTx(ctx context.Context, tx *sql.Tx, taskID domain.ID) (taskControl, error) {
	var control taskControl
	var authorityJSON string
	err := tx.QueryRowContext(ctx,
		`SELECT task_id, resource_envelope_id, required_enforcement, authority_ceiling_json FROM tasks WHERE task_id = ?`, taskID,
	).Scan(&control.TaskID, &control.ResourceEnvelopeID, &control.RequiredEnforcement, &authorityJSON)
	if err != nil {
		return taskControl{}, err
	}
	if err := json.Unmarshal([]byte(authorityJSON), &control.AuthorityCeiling); err != nil {
		return taskControl{}, fmt.Errorf("decode authority ceiling: %w", err)
	}
	return control, nil
}

type policyContext struct {
	Now            time.Time
	Risk           string
	AuthorityValid bool
	TaskID         domain.ID
	AttemptID      domain.ID
	Provider       Provider
	DescriptorType string
	Phase          string
	Attributes     map[string]any
}

func (s *Service) evaluatePolicy(ctx context.Context, pc policyContext) (domain.PolicyDecision, error) {
	attrs := map[string]any{
		"task_id": pc.TaskID, "attempt_id": pc.AttemptID, "provider": pc.Provider.Name(),
		"capability": pc.Provider.Capability(), "enforcement": pc.Provider.EnforcementLevel(),
		"descriptor_type": pc.DescriptorType, "phase": pc.Phase,
	}
	for key, value := range pc.Attributes {
		attrs[key] = value
	}
	decision, err := s.policy.Evaluate(ctx, policy.PolicyInput{
		Risk: strings.TrimSpace(pc.Risk), AuthorityValid: pc.AuthorityValid, Now: pc.Now, Attributes: attrs,
	})
	if err != nil {
		return domain.PolicyDecision{}, fmt.Errorf("policy evaluation: %w", err)
	}
	return decision, nil
}

func canonicalIntent(provider Provider, descriptor IntentDescriptor) ([]byte, string, error) {
	if descriptor == nil || strings.TrimSpace(descriptor.DescriptorType()) == "" {
		return nil, "", errors.New("typed intent descriptor is required")
	}
	raw, err := provider.CanonicalIntent(descriptor)
	if err != nil {
		return nil, "", fmt.Errorf("provider canonical intent: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", fmt.Errorf("provider canonical intent is not JSON: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	material := struct {
		Provider       string          `json:"provider"`
		DescriptorType string          `json:"descriptor_type"`
		Intent         json.RawMessage `json:"intent"`
		AdapterVersion string          `json:"adapter_version,omitempty"`
	}{Provider: provider.Name(), DescriptorType: descriptor.DescriptorType(), Intent: canonical}
	if provider.AdapterVersionSemanticallyRelevant() {
		material.AdapterVersion = provider.AdapterVersion()
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(encoded)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func (s *Service) validatePrepareRequest(request PrepareRequest) (Provider, error) {
	if request.AttemptID == "" || strings.TrimSpace(request.Provider) == "" || strings.TrimSpace(request.TrustedSlotKey) == "" || request.Intent == nil {
		return nil, errors.New("attempt, provider, trusted slot key, and typed intent are required")
	}
	return s.providerFor(request.Provider)
}

func (s *Service) providerFor(name string) (Provider, error) {
	provider := s.providers[strings.TrimSpace(name)]
	if provider == nil {
		return nil, fmt.Errorf("provider %q is not registered", name)
	}
	return provider, nil
}

func (s *Service) providerRequest(ctx context.Context, operationID domain.ID) (ProviderDispatchRequest, error) {
	var request ProviderDispatchRequest
	var canonical string
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT o.operation_id, o.task_id, o.effect_slot_id, o.intent_fingerprint, o.intent_revision, s.canonical_intent_json
		FROM external_operations o JOIN effect_slots s ON s.effect_slot_id = o.effect_slot_id
		WHERE o.operation_id = ?`, operationID,
	).Scan(&request.OperationID, &request.TaskID, &request.EffectSlotID, &request.IntentFingerprint, &request.IntentRevision, &canonical)
	if err != nil {
		return ProviderDispatchRequest{}, err
	}
	request.CanonicalIntent = []byte(canonical)
	return request, nil
}

func (s *Service) loadOperation(ctx context.Context, operationID domain.ID) (domain.ExternalOperation, error) {
	return loadOperationQuery(ctx, s.store.DB(), operationID)
}

type operationQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadOperationTx(ctx context.Context, tx *sql.Tx, operationID domain.ID) (domain.ExternalOperation, error) {
	return loadOperationQuery(ctx, tx, operationID)
}

func loadOperationQuery(ctx context.Context, q operationQuery, operationID domain.ID) (domain.ExternalOperation, error) {
	var op domain.ExternalOperation
	var createdAt string
	var dispatched, settled sql.NullString
	var providerReference string
	err := q.QueryRowContext(ctx, `
		SELECT operation_id, task_id, attempt_id, effect_slot_id, state, intent_fingerprint, intent_revision,
		       provider, COALESCE(provider_reference, ''), reservation_id, COALESCE(approval_id, ''),
		       created_at, dispatched_at, settled_at
		FROM external_operations WHERE operation_id = ?`, operationID,
	).Scan(&op.ID, &op.TaskID, &op.AttemptID, &op.EffectSlotID, &op.State, &op.IntentFingerprint, &op.IntentRevision,
		&op.Provider, &providerReference, &op.ReservationID, &op.ApprovalID, &createdAt, &dispatched, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalOperation{}, fmt.Errorf("operation %q not found", operationID)
	}
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	if op.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return domain.ExternalOperation{}, fmt.Errorf("parse operation created_at: %w", err)
	}
	op.ProviderReference = providerReference
	if dispatched.Valid {
		if op.DispatchedAt, err = time.Parse(time.RFC3339Nano, dispatched.String); err != nil {
			return domain.ExternalOperation{}, fmt.Errorf("parse operation dispatched_at: %w", err)
		}
	}
	if settled.Valid {
		if op.SettledAt, err = time.Parse(time.RFC3339Nano, settled.String); err != nil {
			return domain.ExternalOperation{}, fmt.Errorf("parse operation settled_at: %w", err)
		}
	}
	return op, nil
}

func loadLatestOperationForSlotTx(ctx context.Context, tx *sql.Tx, slotID domain.ID) (domain.ExternalOperation, error) {
	var operationID domain.ID
	err := tx.QueryRowContext(ctx,
		`SELECT operation_id FROM external_operations WHERE effect_slot_id = ? ORDER BY operation_sequence DESC LIMIT 1`, slotID,
	).Scan(&operationID)
	if err != nil {
		return domain.ExternalOperation{}, err
	}
	return loadOperationTx(ctx, tx, operationID)
}

func nextOperationSequence(ctx context.Context, tx *sql.Tx, slotID domain.ID) (int64, error) {
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(operation_sequence), 0) + 1 FROM external_operations WHERE effect_slot_id = ?`, slotID,
	).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

func appendOperationEvent(ctx context.Context, tx *sql.Tx, taskID, attemptID domain.ID, eventType string, operationID domain.ID, now time.Time) error {
	details, err := json.Marshal(map[string]any{"operation_id": operationID})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		domain.NewID("event"), taskID, attemptID, eventType, string(details), formatTime(now),
	)
	return err
}

func controlRisk(ctx context.Context, q operationQuery, operationID domain.ID) (string, error) {
	var risk string
	if err := q.QueryRowContext(ctx, `SELECT risk FROM external_operations WHERE operation_id = ?`, operationID).Scan(&risk); err != nil {
		return "", fmt.Errorf("load operation risk for %q: %w", operationID, err)
	}
	return risk, nil
}

func controlDescriptorType(ctx context.Context, q operationQuery, slotID domain.ID) (string, error) {
	var descriptorType string
	if err := q.QueryRowContext(ctx, `SELECT descriptor_type FROM effect_slots WHERE effect_slot_id = ?`, slotID).Scan(&descriptorType); err != nil {
		return "", fmt.Errorf("load descriptor type for effect slot %q: %w", slotID, err)
	}
	return descriptorType, nil
}

func hasString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func enforcementSatisfies(actual, required domain.EnforcementLevel) bool {
	return enforcementRank(actual) >= enforcementRank(required)
}

func enforcementRank(level domain.EnforcementLevel) int {
	switch level {
	case domain.EnforcementUnenforced:
		return 1
	case domain.EnforcementPartial:
		return 2
	case domain.EnforcementEnforced:
		return 3
	default:
		return 0
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil || s.execution == nil || s.policy == nil || s.resources == nil || s.approvals == nil || s.collectiveID == "" {
		return errors.New("operations service is not configured")
	}
	return nil
}
