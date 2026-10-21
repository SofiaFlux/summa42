package control

import (
	"context"
	"net/http"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/experience"
)

type ExperienceGrantRequestDTO struct {
	RequestID  domain.ID `json:"request_id"`
	Digest     string    `json:"digest"`
	ApprovalID domain.ID `json:"approval_id"`
}

type ExperienceGrantCreateRequest struct {
	ScopeKey                   string    `json:"scope_key"`
	AllowedExecutors           []string  `json:"allowed_executors"`
	MinVerifiedSamples         int       `json:"min_verified_samples"`
	MaxAcceptanceRegressionBps int       `json:"max_acceptance_regression_bps"`
	MaxCostRegressionBps       int       `json:"max_cost_regression_bps"`
	ExpiresAt                  time.Time `json:"expires_at"`
}

type ExperienceProposalCreateRequest struct {
	GrantID                domain.ID   `json:"grant_id"`
	PreferredExecutor      string      `json:"preferred_executor"`
	EvidenceObservationIDs []domain.ID `json:"evidence_observation_ids"`
}

type ExperienceOutcomeCreateRequest struct {
	TaskID   domain.ID `json:"task_id"`
	Accepted bool      `json:"accepted"`
}

type ExperienceOutcomeDTO struct {
	TaskID domain.ID `json:"task_id"`
	Status string    `json:"status"`
}

type ExperienceProposalDTO struct {
	ID                     domain.ID                  `json:"id"`
	GrantID                domain.ID                  `json:"grant_id"`
	ScopeKey               string                     `json:"scope_key"`
	GenericTaskClass       domain.GenericTaskClass    `json:"generic_task_class"`
	PreferredExecutor      string                     `json:"preferred_executor"`
	State                  domain.ExperienceRuleState `json:"state"`
	EvidenceObservationIDs []domain.ID                `json:"evidence_observation_ids,omitempty"`
}

type AdaptationGrantDTO struct {
	ID               domain.ID `json:"id"`
	RequestID        domain.ID `json:"request_id"`
	Kind             string    `json:"kind"`
	ScopeKey         string    `json:"scope_key"`
	AllowedExecutors []string  `json:"allowed_executors"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type ExperienceRuleDTO struct {
	ID                domain.ID                  `json:"id"`
	ProposalID        domain.ID                  `json:"proposal_id"`
	GrantID           domain.ID                  `json:"grant_id"`
	Version           int                        `json:"version"`
	ScopeKey          string                     `json:"scope_key"`
	GenericTaskClass  domain.GenericTaskClass    `json:"generic_task_class"`
	PreferredExecutor string                     `json:"preferred_executor"`
	State             domain.ExperienceRuleState `json:"state"`
	VerifiedSamples   int                        `json:"verified_samples"`
}

type ExperienceService interface {
	RequestGrant(context.Context, experience.GrantInput, domain.ID) (experience.GrantRequest, error)
	ActivateGrant(context.Context, domain.ID) (domain.AdaptationGrant, error)
	Propose(context.Context, experience.ProposalInput) (domain.ExperienceProposal, error)
	ObserveVerifiedOutcome(context.Context, experience.VerifiedOutcome) error
	Evaluate(context.Context, domain.ID) (domain.ExperienceRule, error)
	Rules(context.Context) ([]domain.ExperienceRule, error)
	Rule(context.Context, domain.ID) (domain.ExperienceRule, error)
}

func (s *Server) handleExperienceGrantRequest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	var request ExperienceGrantCreateRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.deps.Experience.RequestGrant(r.Context(), experience.GrantInput{
		Kind: experience.AdaptationExecutorPreference, ScopeKey: request.ScopeKey, AllowedExecutors: request.AllowedExecutors,
		MinVerifiedSamples: request.MinVerifiedSamples, MaxAcceptanceRegressionBps: request.MaxAcceptanceRegressionBps,
		MaxCostRegressionBps: request.MaxCostRegressionBps, ExpiresAt: request.ExpiresAt,
	}, s.config.OwnerPrincipalID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ExperienceGrantRequestDTO{RequestID: created.ID, Digest: created.Digest, ApprovalID: created.ApprovalID})
}

func (s *Server) handleExperienceGrantActivate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	grant, err := s.deps.Experience.ActivateGrant(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, adaptationGrantDTO(grant))
}

func (s *Server) handleExperienceProposalCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	var request ExperienceProposalCreateRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proposal, err := s.deps.Experience.Propose(r.Context(), experience.ProposalInput{
		GrantID: request.GrantID, PreferredExecutor: request.PreferredExecutor,
		EvidenceObservationIDs: append([]domain.ID(nil), request.EvidenceObservationIDs...),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, experienceProposalDTO(proposal))
}

func (s *Server) handleExperienceOutcomeCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	var request ExperienceOutcomeCreateRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.deps.Experience.ObserveVerifiedOutcome(r.Context(), experience.VerifiedOutcome{
		TaskID: request.TaskID, Accepted: request.Accepted,
	}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, ExperienceOutcomeDTO{TaskID: request.TaskID, Status: "VERIFIED_OUTCOME_RECORDED"})
}

func (s *Server) handleExperienceEvaluate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rule, err := s.deps.Experience.Evaluate(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, experienceRuleDTO(rule))
}

func (s *Server) handleExperienceList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	rules, err := s.deps.Experience.Rules(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ExperienceRuleDTO, 0, len(rules))
	for _, rule := range rules {
		out = append(out, experienceRuleDTO(rule))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExperienceGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Experience == nil {
		writeError(w, http.StatusServiceUnavailable, "experience service is not configured")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	rule, err := s.deps.Experience.Rule(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, experienceRuleDTO(rule))
}

func adaptationGrantDTO(grant domain.AdaptationGrant) AdaptationGrantDTO {
	return AdaptationGrantDTO{ID: grant.ID, RequestID: grant.RequestID, Kind: grant.Kind, ScopeKey: grant.ScopeKey, AllowedExecutors: append([]string(nil), grant.AllowedExecutors...), ExpiresAt: grant.ExpiresAt}
}
func experienceRuleDTO(rule domain.ExperienceRule) ExperienceRuleDTO {
	return ExperienceRuleDTO{ID: rule.ID, ProposalID: rule.ProposalID, GrantID: rule.GrantID, Version: rule.Version, ScopeKey: rule.ScopeKey, GenericTaskClass: rule.GenericTaskClass, PreferredExecutor: rule.PreferredExecutor, State: rule.State, VerifiedSamples: rule.VerifiedSamples}
}

func experienceProposalDTO(proposal domain.ExperienceProposal) ExperienceProposalDTO {
	return ExperienceProposalDTO{
		ID: proposal.ID, GrantID: proposal.GrantID, ScopeKey: proposal.ScopeKey,
		GenericTaskClass: proposal.GenericTaskClass, PreferredExecutor: proposal.PreferredExecutor,
		State: proposal.State, EvidenceObservationIDs: append([]domain.ID(nil), proposal.EvidenceObservationIDs...),
	}
}
