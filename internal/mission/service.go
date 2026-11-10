package mission

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// Service provides deterministic lifecycle and governance orchestration for versioned missions.
type Service struct {
	store MissionStore
}

// NewService creates a new Mission service.
func NewService(store MissionStore) *Service {
	if store == nil {
		panic("mission store cannot be nil")
	}
	return &Service{store: store}
}

// CreateDraft creates an initial version (v1) of a mission in DRAFT state.
func (s *Service) CreateDraft(ctx context.Context, principal Principal, id domain.ID, fields CreateMissionFields) (Mission, error) {
	if strings.TrimSpace(fields.Purpose) == "" {
		return Mission{}, fmt.Errorf("mission purpose cannot be empty")
	}

	now := time.Now().UTC()
	m := Mission{
		ID:                   id,
		Version:              1,
		State:                MissionStateDraft,
		Purpose:              fields.Purpose,
		DesiredOutcomes:      cloneStringSlice(fields.DesiredOutcomes),
		Obligations:          cloneStringSlice(fields.Obligations),
		Constraints:          cloneStringSlice(fields.Constraints),
		NonGoals:             cloneStringSlice(fields.NonGoals),
		TradeOffRules:        cloneTradeOffRules(fields.TradeOffRules),
		HumanTrustBoundaries: cloneStringSlice(fields.HumanTrustBoundaries),
		Assumptions:          cloneStringSlice(fields.Assumptions),
		Unknowns:             cloneStringSlice(fields.Unknowns),
		RevisionTriggers:     cloneStringSlice(fields.RevisionTriggers),
		Provenance: MissionProvenance{
			SourceInputs:    cloneStringSlice(fields.SourceInputs),
			EvidenceRefs:    cloneStringSlice(fields.EvidenceRefs),
			AuthorPrincipal: principal.ID,
			ChangeSummary:   fields.ChangeSummary,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.Save(ctx, m); err != nil {
		return Mission{}, err
	}
	return m, nil
}

// UpdateDraft mutates an existing DRAFT mission.
// Invariant: approved, active, superseded, or proposed missions are immutable.
func (s *Service) UpdateDraft(ctx context.Context, principal Principal, id domain.ID, version int, fields CreateMissionFields) error {
	m, err := s.store.Get(ctx, id, version)
	if err != nil {
		return err
	}

	if m.State != MissionStateDraft {
		return ErrMissionImmutable
	}

	if strings.TrimSpace(fields.Purpose) != "" {
		m.Purpose = fields.Purpose
	}
	if fields.DesiredOutcomes != nil {
		m.DesiredOutcomes = cloneStringSlice(fields.DesiredOutcomes)
	}
	if fields.Obligations != nil {
		m.Obligations = cloneStringSlice(fields.Obligations)
	}
	if fields.Constraints != nil {
		m.Constraints = cloneStringSlice(fields.Constraints)
	}
	if fields.NonGoals != nil {
		m.NonGoals = cloneStringSlice(fields.NonGoals)
	}
	if fields.TradeOffRules != nil {
		m.TradeOffRules = cloneTradeOffRules(fields.TradeOffRules)
	}
	if fields.HumanTrustBoundaries != nil {
		m.HumanTrustBoundaries = cloneStringSlice(fields.HumanTrustBoundaries)
	}
	if fields.Assumptions != nil {
		m.Assumptions = cloneStringSlice(fields.Assumptions)
	}
	if fields.Unknowns != nil {
		m.Unknowns = cloneStringSlice(fields.Unknowns)
	}
	if fields.RevisionTriggers != nil {
		m.RevisionTriggers = cloneStringSlice(fields.RevisionTriggers)
	}
	if fields.SourceInputs != nil {
		m.Provenance.SourceInputs = cloneStringSlice(fields.SourceInputs)
	}
	if fields.EvidenceRefs != nil {
		m.Provenance.EvidenceRefs = cloneStringSlice(fields.EvidenceRefs)
	}
	if fields.ChangeSummary != "" {
		m.Provenance.ChangeSummary = fields.ChangeSummary
	}
	m.UpdatedAt = time.Now().UTC()

	return s.store.Update(ctx, m)
}

// CreateNextVersion creates a new incremental draft version based on existing versions.
func (s *Service) CreateNextVersion(ctx context.Context, principal Principal, id domain.ID, fields CreateMissionFields) (Mission, error) {
	if strings.TrimSpace(fields.Purpose) == "" {
		return Mission{}, fmt.Errorf("mission purpose cannot be empty")
	}

	versions, err := s.store.ListVersions(ctx, id)
	if err != nil {
		return Mission{}, err
	}
	if len(versions) == 0 {
		return Mission{}, ErrMissionNotFound
	}

	maxVersion := 0
	for _, v := range versions {
		if v.Version > maxVersion {
			maxVersion = v.Version
		}
	}

	now := time.Now().UTC()
	next := Mission{
		ID:                   id,
		Version:              maxVersion + 1,
		State:                MissionStateDraft,
		Purpose:              fields.Purpose,
		DesiredOutcomes:      cloneStringSlice(fields.DesiredOutcomes),
		Obligations:          cloneStringSlice(fields.Obligations),
		Constraints:          cloneStringSlice(fields.Constraints),
		NonGoals:             cloneStringSlice(fields.NonGoals),
		TradeOffRules:        cloneTradeOffRules(fields.TradeOffRules),
		HumanTrustBoundaries: cloneStringSlice(fields.HumanTrustBoundaries),
		Assumptions:          cloneStringSlice(fields.Assumptions),
		Unknowns:             cloneStringSlice(fields.Unknowns),
		RevisionTriggers:     cloneStringSlice(fields.RevisionTriggers),
		Provenance: MissionProvenance{
			BaseVersion:     maxVersion,
			SourceInputs:    cloneStringSlice(fields.SourceInputs),
			EvidenceRefs:    cloneStringSlice(fields.EvidenceRefs),
			AuthorPrincipal: principal.ID,
			ChangeSummary:   fields.ChangeSummary,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.Save(ctx, next); err != nil {
		return Mission{}, err
	}
	return next, nil
}

// Propose advances a mission from DRAFT to PROPOSED.
func (s *Service) Propose(ctx context.Context, principal Principal, id domain.ID, version int) (Mission, error) {
	m, err := s.store.Get(ctx, id, version)
	if err != nil {
		return Mission{}, err
	}
	if m.State != MissionStateDraft {
		return Mission{}, fmt.Errorf("%w: cannot propose from state %s", ErrInvalidStateTransition, m.State)
	}

	// Completeness gate: all required sections must be populated before proposal
	if err := m.ValidateCompleteness(); err != nil {
		return Mission{}, fmt.Errorf("%w: %s", ErrIncompleteMissionSchema, err)
	}

	now := time.Now().UTC()
	m.State = MissionStateProposed
	m.Provenance.ProposedBy = principal.ID
	m.Provenance.ProposedAt = &now
	m.UpdatedAt = now

	if err := s.store.Update(ctx, m); err != nil {
		return Mission{}, err
	}
	return m, nil
}

// Approve advances a proposed mission to APPROVED.
// Invariant: autonomous workers or research routines CANNOT self-approve; root/human authority is strictly required.
func (s *Service) Approve(ctx context.Context, principal Principal, id domain.ID, version int) (Mission, error) {
	if !principal.IsAuthorizedGovernance() {
		return Mission{}, ErrUnauthorizedApproval
	}

	m, err := s.store.Get(ctx, id, version)
	if err != nil {
		return Mission{}, err
	}
	if m.State != MissionStateProposed {
		return Mission{}, fmt.Errorf("%w: cannot approve from state %s", ErrInvalidStateTransition, m.State)
	}

	// Verify cryptographic signature if provided
	payload := []byte(fmt.Sprintf("%s:%d:%s", m.ID, m.Version, m.Purpose))
	if !principal.VerifySignature(payload) {
		return Mission{}, ErrInvalidGovernanceSignature
	}

	now := time.Now().UTC()
	m.State = MissionStateApproved
	m.Provenance.ApprovingPrincipal = principal.ID
	m.Provenance.ApprovedAt = &now
	m.UpdatedAt = now

	if err := s.store.Update(ctx, m); err != nil {
		return Mission{}, err
	}
	return m, nil
}

// Activate transitions an approved mission to ACTIVE atomically.
// Invariant: Any previously active version of this mission is automatically marked SUPERSEDED.
func (s *Service) Activate(ctx context.Context, principal Principal, id domain.ID, version int) (Mission, error) {
	if !principal.IsAuthorizedGovernance() {
		return Mission{}, ErrUnauthorizedGovernance
	}

	return s.store.ActivateVersion(ctx, id, version)
}

// GetVersion retrieves a specific version of a mission.
func (s *Service) GetVersion(ctx context.Context, id domain.ID, version int) (Mission, error) {
	return s.store.Get(ctx, id, version)
}

// GetActive retrieves the currently ACTIVE mission specification for an ID.
func (s *Service) GetActive(ctx context.Context, id domain.ID) (Mission, error) {
	return s.store.GetActive(ctx, id)
}

// ListVersions lists all historical versions of a mission.
func (s *Service) ListVersions(ctx context.Context, id domain.ID) ([]Mission, error) {
	return s.store.ListVersions(ctx, id)
}

func cloneStringSlice(s []string) []string {
	if s == nil {
		return nil
	}
	cp := make([]string, len(s))
	copy(cp, s)
	return cp
}

func cloneTradeOffRules(rules []TradeOffRule) []TradeOffRule {
	if rules == nil {
		return nil
	}
	cp := make([]TradeOffRule, len(rules))
	copy(cp, rules)
	return cp
}
