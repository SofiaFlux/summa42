package operations

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
	"github.com/SofiaFlux/summa42/internal/domain"
)

type policyGate struct {
	Decision         domain.PolicyDecision
	RequiresApproval bool
}

func validateDecisionProvenance(decision domain.PolicyDecision) error {
	if decision.ID == "" || decision.PolicySetID == "" ||
		strings.TrimSpace(decision.PolicySetHash) == "" ||
		strings.TrimSpace(decision.PolicyCapabilitiesHash) == "" {
		return fmt.Errorf("%w: incomplete policy-decision provenance", domain.ErrPolicyDenied)
	}
	return nil
}

func evaluatePrepareOutcome(decision domain.PolicyDecision) (policyGate, error) {
	switch decision.Outcome {
	case domain.PolicyAllow:
		return policyGate{Decision: decision}, nil
	case domain.PolicyRequireApproval:
		return policyGate{Decision: decision, RequiresApproval: true}, nil
	case domain.PolicyDeny:
		return policyGate{}, fmt.Errorf("%w: outcome=%s", domain.ErrPolicyDenied, decision.Outcome)
	case domain.PolicyAllowWithLimit:
		return policyGate{}, fmt.Errorf("%w: ALLOW_WITH_LIMIT has no named limit enforcer", domain.ErrPolicyDenied)
	default:
		return policyGate{}, fmt.Errorf("%w: unknown outcome=%s", domain.ErrPolicyDenied, decision.Outcome)
	}
}

func validateDispatchOutcome(decision domain.PolicyDecision) error {
	switch decision.Outcome {
	case domain.PolicyAllow, domain.PolicyRequireApproval:
		return nil
	case domain.PolicyDeny:
		return fmt.Errorf("%w: outcome=%s", domain.ErrPolicyDenied, decision.Outcome)
	case domain.PolicyAllowWithLimit:
		return fmt.Errorf("%w: ALLOW_WITH_LIMIT has no named limit enforcer", domain.ErrPolicyDenied)
	default:
		return fmt.Errorf("%w: unknown outcome=%s", domain.ErrPolicyDenied, decision.Outcome)
	}
}

func requiredApproversFor(decision domain.PolicyDecision, caller []domain.ID) []domain.ID {
	required := append([]domain.ID(nil), caller...)
	if decision.Outcome == domain.PolicyRequireApproval {
		required = append(required, decision.RequiredApprovals...)
	}
	return normalizeApprovalIDs(required)
}

func normalizeApprovalIDs(values []domain.ID) []domain.ID {
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

func approvalIDsEqual(a, b []domain.ID) bool {
	a = normalizeApprovalIDs(a)
	b = normalizeApprovalIDs(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// OperationApprovalDigest is the digest an Owner approval signs; it covers identity and intent, not caller attributes.
func OperationApprovalDigest(
	provider, descriptorType string,
	effectSlotID domain.ID,
	intentFingerprint string,
	intentRevision int64,
	canonicalIntent []byte,
) (string, error) {
	if strings.TrimSpace(provider) == "" || strings.TrimSpace(descriptorType) == "" ||
		effectSlotID == "" || strings.TrimSpace(intentFingerprint) == "" || intentRevision <= 0 ||
		len(canonicalIntent) == 0 {
		return "", errors.New("complete consequential dispatch identity is required")
	}
	canonicalHash := sha256.Sum256(canonicalIntent)
	material := fmt.Sprintf(
		"provider:%s\ndescriptor-type:%s\neffect-slot-id:%s\nintent-fingerprint:%s\nintent-revision:%d\ncanonical-intent-hash:%s\n",
		strings.TrimSpace(provider), strings.TrimSpace(descriptorType), effectSlotID,
		strings.TrimSpace(intentFingerprint), intentRevision, hex.EncodeToString(canonicalHash[:]),
	)
	digest := sha256.Sum256([]byte(material))
	return hex.EncodeToString(digest[:]), nil
}

func encodeApprovalIDs(values []domain.ID) (string, error) {
	encoded, err := json.Marshal(normalizeApprovalIDs(values))
	return string(encoded), err
}

func decodeApprovalIDs(raw string) ([]domain.ID, error) {
	var values []domain.ID
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	return normalizeApprovalIDs(values), nil
}

func (s *Service) prepareApprovalInTx(
	ctx context.Context,
	tx *sql.Tx,
	result domain.ExternalOperation,
	guardedLeaseExpiry time.Time,
	decision domain.PolicyDecision,
	callerRequired []domain.ID,
	provider Provider,
	descriptorType string,
	canonical []byte,
) (domain.ID, string, error) {
	callerRequired = normalizeApprovalIDs(callerRequired)
	callerJSON, err := encodeApprovalIDs(callerRequired)
	if err != nil {
		return "", "", err
	}
	required := requiredApproversFor(decision, callerRequired)
	requiresApproval := decision.Outcome == domain.PolicyRequireApproval || len(callerRequired) > 0
	if !requiresApproval {
		return "", callerJSON, nil
	}
	if len(required) == 0 {
		return "", "", fmt.Errorf("%w: approval required but no required approver is defined", domain.ErrPolicyDenied)
	}
	digest, err := OperationApprovalDigest(
		provider.Name(), descriptorType, result.EffectSlotID,
		result.IntentFingerprint, result.IntentRevision, canonical,
	)
	if err != nil {
		return "", "", err
	}
	record, err := s.approvals.CreateInTx(ctx, tx, approvals.CreateRequest{
		SubjectKind: "EXTERNAL_OPERATION", SubjectID: result.ID, RequestDigest: digest,
		PolicyDecisionID: decision.ID, RequiredApprovers: required,
		RequestedBy: result.AttemptID, ExpiresAt: guardedLeaseExpiry,
	})
	if err != nil {
		return "", "", err
	}
	return record.ID, callerJSON, nil
}

func (s *Service) preparedApprovalMatchesInTx(
	ctx context.Context,
	tx *sql.Tx,
	op domain.ExternalOperation,
	decision domain.PolicyDecision,
	callerRequired []domain.ID,
	provider Provider,
	descriptorType string,
	canonical []byte,
) (bool, error) {
	var rawCaller string
	if err := tx.QueryRowContext(ctx,
		"SELECT caller_required_approvers_json FROM external_operations WHERE operation_id = ?", op.ID,
	).Scan(&rawCaller); err != nil {
		return false, err
	}
	storedCaller, err := decodeApprovalIDs(rawCaller)
	if err != nil {
		return false, err
	}
	callerRequired = normalizeApprovalIDs(callerRequired)
	if !approvalIDsEqual(storedCaller, callerRequired) {
		return false, nil
	}
	required := requiredApproversFor(decision, callerRequired)
	requiresApproval := decision.Outcome == domain.PolicyRequireApproval || len(callerRequired) > 0
	if !requiresApproval {
		return op.ApprovalID == "", nil
	}
	if op.ApprovalID == "" || len(required) == 0 {
		return false, nil
	}
	record, err := s.approvals.GetInTx(ctx, tx, op.ApprovalID)
	if err != nil {
		return false, err
	}
	digest, err := OperationApprovalDigest(
		provider.Name(), descriptorType, op.EffectSlotID,
		op.IntentFingerprint, op.IntentRevision, canonical,
	)
	if err != nil {
		return false, err
	}
	if record.SubjectKind != "EXTERNAL_OPERATION" || record.SubjectID != op.ID ||
		record.RequestDigest != digest || !approvalIDsEqual(record.RequiredApprovers, required) {
		return false, nil
	}
	switch record.State {
	case domain.ApprovalPending, domain.ApprovalApproved:
		return true, nil
	default:
		return false, nil
	}
}

func (s *Service) validateAndConsumeApprovalInTx(
	ctx context.Context,
	tx *sql.Tx,
	op domain.ExternalOperation,
	decision domain.PolicyDecision,
	callerRequired []domain.ID,
	provider Provider,
	descriptorType string,
	canonical []byte,
) error {
	required := requiredApproversFor(decision, callerRequired)
	requiresApproval := decision.Outcome == domain.PolicyRequireApproval || len(callerRequired) > 0
	if !requiresApproval {
		if op.ApprovalID != "" {
			return fmt.Errorf("%w: prepared approval requirement no longer matches current policy", domain.ErrPolicyDenied)
		}
		return nil
	}
	if op.ApprovalID == "" || len(required) == 0 {
		return fmt.Errorf("%w: exact approval is required", domain.ErrPolicyDenied)
	}
	record, err := s.approvals.GetInTx(ctx, tx, op.ApprovalID)
	if err != nil {
		return fmt.Errorf("%w: load approval: %v", domain.ErrPolicyDenied, err)
	}
	digest, err := OperationApprovalDigest(
		provider.Name(), descriptorType, op.EffectSlotID,
		op.IntentFingerprint, op.IntentRevision, canonical,
	)
	if err != nil {
		return err
	}
	if record.SubjectKind != "EXTERNAL_OPERATION" || record.SubjectID != op.ID ||
		record.RequestDigest != digest || !approvalIDsEqual(record.RequiredApprovers, required) {
		return fmt.Errorf("%w: approval binding does not match current dispatch identity", domain.ErrPolicyDenied)
	}
	if record.State != domain.ApprovalApproved {
		return fmt.Errorf("%w: approval state=%s", domain.ErrPolicyDenied, record.State)
	}
	if err := s.approvals.ConsumeInTx(ctx, tx, record.ID, digest); err != nil {
		return fmt.Errorf("%w: consume exact approval: %v", domain.ErrPolicyDenied, err)
	}
	return nil
}

func mapPolicyProfileError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "SUMMA42_POLICY_PROFILE_INACTIVE") {
		return fmt.Errorf("%w: active policy profile changed", domain.ErrPolicyDenied)
	}
	return err
}
