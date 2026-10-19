package capabilities

import (
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

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type GapReason string

const (
	GapReasonMissingCapability   GapReason = "MISSING_CAPABILITY"
	GapReasonSuboptimalCost      GapReason = "SUBOPTIMAL_COST"
	GapReasonDegradedHealth      GapReason = "DEGRADED_HEALTH"
	GapReasonInsufficientEnforce GapReason = "INSUFFICIENT_ENFORCEMENT"
)

type CapabilityGap struct {
	SemanticName         string                  `json:"semantic_name"`
	RequiredEnforcement  domain.EnforcementLevel `json:"required_enforcement"`
	MaxAcceptableCost    float64                 `json:"max_acceptable_cost,omitempty"`
	CurrentIncumbent     string                  `json:"current_incumbent,omitempty"`
	IncumbentCost        float64                 `json:"incumbent_cost,omitempty"`
	IncumbentEnforcement domain.EnforcementLevel `json:"incumbent_enforcement,omitempty"`
	Reason               GapReason               `json:"reason"`
}

type GapRequirements struct {
	MinimumEnforcement domain.EnforcementLevel
	MaxAcceptableCost  float64
}

type CandidateProvider struct {
	ProviderName          string                             `json:"provider_name"`
	SemanticName          string                             `json:"semantic_name"`
	SemanticVersion       string                             `json:"semantic_version"`
	AccessContext         string                             `json:"access_context"`
	AuthorityRequirements []string                           `json:"authority_requirements"`
	MinimumEnforcement    domain.EnforcementLevel            `json:"minimum_enforcement"`
	Description           string                             `json:"description"`
	EstimatedCost         float64                            `json:"estimated_cost"`
	QualityScore          float64                            `json:"quality_score"` // 0.0 - 1.0 benchmarked score
	LatencyMs             int64                              `json:"latency_ms"`
	License               string                             `json:"license"`
	Source                string                             `json:"source"`
	Factory               func() (AssessableProvider, error) `json:"-"`
}

type DiscoverySource interface {
	Name() string
	Discover(ctx context.Context, gap CapabilityGap) ([]CandidateProvider, error)
}

type CandidateEvaluation struct {
	Candidate         CandidateProvider `json:"candidate"`
	ObservedProbe     ProbeResult       `json:"observed_probe"`
	CostRatio         float64           `json:"cost_ratio"` // relative to incumbent (1.0 = equal, 0.63 = 37% cheaper)
	QualityScore      float64           `json:"quality_score"`
	LatencyMs         int64             `json:"latency_ms"`
	LicenseAcceptable bool              `json:"license_acceptable"`
	MeetsEnforcement  bool              `json:"meets_enforcement"`
	Recommended       bool              `json:"recommended"`
	Evidence          []string          `json:"evidence"`
}

type ProposalState string

const (
	ProposalProposed        ProposalState = "PROPOSED"
	ProposalApprovalPending ProposalState = "APPROVAL_PENDING"
	ProposalApproved        ProposalState = "APPROVED"
	ProposalActivated       ProposalState = "ACTIVATED"
	ProposalRejected        ProposalState = "REJECTED"
)

type AcquisitionProposal struct {
	ID                    domain.ID               `json:"proposal_id"`
	SemanticName          string                  `json:"semantic_name"`
	CandidateProvider     string                  `json:"candidate_provider"`
	CandidateVersion      string                  `json:"candidate_version"`
	IncumbentProvider     string                  `json:"incumbent_provider,omitempty"`
	Rationale             string                  `json:"rationale"`
	EvaluationEvidence    []string                `json:"evaluation_evidence"`
	AuthorityRequirements []string                `json:"authority_requirements"`
	MinimumEnforcement    domain.EnforcementLevel `json:"minimum_enforcement"`
	AccessContext         string                  `json:"access_context"`
	RequiresApproval      bool                    `json:"requires_approval"`
	ApprovalID            domain.ID               `json:"approval_id,omitempty"`
	State                 ProposalState           `json:"state"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

type ProposeOptions struct {
	RequiresApproval *bool
	CustomRationale  string
	ApprovalValidity time.Duration
}

type DiscoveryEngine struct {
	store     *state.Store
	clock     clock.Clock
	registry  *Registry
	approvals *approvals.Service
	ownerID   domain.ID

	mu      sync.RWMutex
	sources []DiscoverySource
}

func NewDiscoveryEngine(
	store *state.Store,
	clk clock.Clock,
	registry *Registry,
	approvalsSvc *approvals.Service,
	ownerID domain.ID,
	sources ...DiscoverySource,
) *DiscoveryEngine {
	activeSources := make([]DiscoverySource, 0, len(sources))
	for _, s := range sources {
		if s != nil {
			activeSources = append(activeSources, s)
		}
	}
	return &DiscoveryEngine{
		store:     store,
		clock:     clk,
		registry:  registry,
		approvals: approvalsSvc,
		ownerID:   ownerID,
		sources:   activeSources,
	}
}

func (d *DiscoveryEngine) RegisterSource(source DiscoverySource) error {
	if source == nil {
		return errors.New("discovery source cannot be nil")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sources = append(d.sources, source)
	return nil
}

func (d *DiscoveryEngine) DetectGap(ctx context.Context, semanticName string, req GapRequirements) (*CapabilityGap, error) {
	if err := d.configured(); err != nil {
		return nil, err
	}
	semanticName = strings.TrimSpace(semanticName)
	if semanticName == "" {
		return nil, errors.New("semantic capability name is required")
	}
	if req.MinimumEnforcement == "" {
		req.MinimumEnforcement = domain.EnforcementUnenforced
	}
	if !validEnforcement(req.MinimumEnforcement) {
		return nil, fmt.Errorf("invalid requested minimum enforcement: %s", req.MinimumEnforcement)
	}

	// 1. Check if capability definition exists in registry
	var def Definition
	var authorityJSON string
	err := d.store.DB().QueryRowContext(ctx, `
		SELECT capability_id, semantic_name, semantic_version, provider, access_context,
		       authority_requirements_json, minimum_enforcement
		FROM capability_definitions
		WHERE semantic_name = ? AND active = 1`, semanticName,
	).Scan(
		&def.ID, &def.Skill.Name, &def.Skill.Version, &def.Access.Provider,
		&def.Access.Context, &authorityJSON, &def.Environment.MinimumEnforcement,
	)

	if errors.Is(err, sql.ErrNoRows) {
		return &CapabilityGap{
			SemanticName:        semanticName,
			RequiredEnforcement: req.MinimumEnforcement,
			MaxAcceptableCost:   req.MaxAcceptableCost,
			Reason:              GapReasonMissingCapability,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query capability definition: %w", err)
	}

	// 2. Definition exists, check assessment
	assessment, err := d.registry.LatestAssessment(ctx, semanticName)
	if err != nil || !assessment.Available || assessment.Health == HealthUnhealthy {
		return &CapabilityGap{
			SemanticName:         semanticName,
			RequiredEnforcement:  req.MinimumEnforcement,
			MaxAcceptableCost:    req.MaxAcceptableCost,
			CurrentIncumbent:     def.Access.Provider,
			IncumbentEnforcement: assessment.Enforcement,
			Reason:               GapReasonDegradedHealth,
		}, nil
	}

	// 3. Check enforcement level
	if !enforcementAtLeast(assessment.Enforcement, req.MinimumEnforcement) {
		return &CapabilityGap{
			SemanticName:         semanticName,
			RequiredEnforcement:  req.MinimumEnforcement,
			MaxAcceptableCost:    req.MaxAcceptableCost,
			CurrentIncumbent:     def.Access.Provider,
			IncumbentEnforcement: assessment.Enforcement,
			Reason:               GapReasonInsufficientEnforce,
		}, nil
	}

	// 4. Check cost if maximum acceptable cost ceiling is specified
	incumbentCost := extractCost(assessment.CostMetadata)
	if req.MaxAcceptableCost > 0 && incumbentCost > req.MaxAcceptableCost {
		return &CapabilityGap{
			SemanticName:         semanticName,
			RequiredEnforcement:  req.MinimumEnforcement,
			MaxAcceptableCost:    req.MaxAcceptableCost,
			CurrentIncumbent:     def.Access.Provider,
			IncumbentCost:        incumbentCost,
			IncumbentEnforcement: assessment.Enforcement,
			Reason:               GapReasonSuboptimalCost,
		}, nil
	}

	return nil, nil // No gap detected
}

func (d *DiscoveryEngine) DiscoverCandidates(ctx context.Context, gap CapabilityGap) ([]CandidateProvider, error) {
	if err := d.configured(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	sources := append([]DiscoverySource(nil), d.sources...)
	d.mu.RUnlock()

	var candidates []CandidateProvider
	for _, source := range sources {
		found, err := source.Discover(ctx, gap)
		if err != nil {
			return nil, fmt.Errorf("discovery source %q: %w", source.Name(), err)
		}
		for _, c := range found {
			c.Source = source.Name()
			candidates = append(candidates, c)
		}
	}
	return candidates, nil
}

func (d *DiscoveryEngine) EvaluateCandidate(ctx context.Context, candidate CandidateProvider, gap CapabilityGap) (CandidateEvaluation, error) {
	if err := d.configured(); err != nil {
		return CandidateEvaluation{}, err
	}
	if candidate.ProviderName == "" || candidate.SemanticName == "" {
		return CandidateEvaluation{}, errors.New("candidate provider name and semantic name are required")
	}
	if candidate.Factory == nil {
		return CandidateEvaluation{}, errors.New("candidate provider factory is required for sandboxed evaluation")
	}

	provider, err := candidate.Factory()
	if err != nil {
		return CandidateEvaluation{}, fmt.Errorf("instantiate candidate sandbox adapter: %w", err)
	}

	synthDef := Definition{
		ID:    domain.NewID("evalcap"),
		Skill: Skill{Name: candidate.SemanticName, Version: candidate.SemanticVersion},
		Access: Access{
			Provider: candidate.ProviderName,
			Context:  candidate.AccessContext,
		},
		Authority: AuthorityRequirement{
			Capabilities: candidate.AuthorityRequirements,
		},
		Environment: EnvironmentRequirement{
			MinimumEnforcement: candidate.MinimumEnforcement,
		},
	}

	start := d.clock.Now()
	probeResult, err := provider.Probe(ctx, synthDef)
	latency := d.clock.Now().Sub(start).Milliseconds()
	if err != nil {
		return CandidateEvaluation{}, fmt.Errorf("probe candidate provider: %w", err)
	}

	meetsEnforce := enforcementAtLeast(probeResult.Enforcement, candidate.MinimumEnforcement) &&
		enforcementAtLeast(probeResult.Enforcement, gap.RequiredEnforcement)

	licenseOk := isLicenseAcceptable(candidate.License)

	var costRatio float64 = 1.0
	candidateCost := candidate.EstimatedCost
	if candidateCost == 0 {
		candidateCost = extractCost(probeResult.CostMetadata)
	}
	if gap.IncumbentCost > 0 && candidateCost > 0 {
		costRatio = candidateCost / gap.IncumbentCost
	}

	recommended := meetsEnforce && licenseOk && probeResult.Available && probeResult.Health == HealthHealthy
	if gap.Reason == GapReasonSuboptimalCost && costRatio >= 1.0 && candidate.QualityScore <= 0.8 {
		recommended = false
	}

	evidence := make([]string, 0, len(probeResult.Evidence)+5)
	evidence = append(evidence, probeResult.Evidence...)
	evidence = append(evidence,
		fmt.Sprintf("observed_enforcement:%s", probeResult.Enforcement),
		fmt.Sprintf("observed_health:%s", probeResult.Health),
		fmt.Sprintf("license:%s", candidate.License),
		fmt.Sprintf("cost_ratio:%.2f", costRatio),
		fmt.Sprintf("benchmark_quality:%.2f", candidate.QualityScore),
	)

	return CandidateEvaluation{
		Candidate:         candidate,
		ObservedProbe:     probeResult,
		CostRatio:         costRatio,
		QualityScore:      candidate.QualityScore,
		LatencyMs:         latency,
		LicenseAcceptable: licenseOk,
		MeetsEnforcement:  meetsEnforce,
		Recommended:       recommended,
		Evidence:          evidence,
	}, nil
}

func (d *DiscoveryEngine) ProposeAcquisition(
	ctx context.Context,
	eval CandidateEvaluation,
	gap CapabilityGap,
	opts ProposeOptions,
) (AcquisitionProposal, error) {
	if err := d.configured(); err != nil {
		return AcquisitionProposal{}, err
	}
	cand := eval.Candidate
	if cand.ProviderName == "" || cand.SemanticName == "" {
		return AcquisitionProposal{}, errors.New("evaluated candidate details are required")
	}

	proposalID := domain.NewID("capproposal")
	now := d.clock.Now().UTC()

	rationale := strings.TrimSpace(opts.CustomRationale)
	if rationale == "" {
		if gap.CurrentIncumbent != "" {
			rationale = fmt.Sprintf("Candidate %s offers capability %s (quality: %.2f, cost ratio: %.2fx vs incumbent %s)",
				cand.ProviderName, cand.SemanticName, eval.QualityScore, eval.CostRatio, gap.CurrentIncumbent)
		} else {
			rationale = fmt.Sprintf("Candidate %s satisfies missing capability %s with %s enforcement",
				cand.ProviderName, cand.SemanticName, cand.MinimumEnforcement)
		}
	}

	// Determine if Owner/Policy approval is required
	requiresApproval := true
	if opts.RequiresApproval != nil {
		requiresApproval = *opts.RequiresApproval
	} else if len(cand.AuthorityRequirements) == 0 && cand.EstimatedCost == 0 && cand.MinimumEnforcement == domain.EnforcementUnenforced {
		// Zero-cost, zero-authority expansion local candidates can be pre-approved
		requiresApproval = false
	}

	proposal := AcquisitionProposal{
		ID:                    proposalID,
		SemanticName:          cand.SemanticName,
		CandidateProvider:     cand.ProviderName,
		CandidateVersion:      cand.SemanticVersion,
		IncumbentProvider:     gap.CurrentIncumbent,
		Rationale:             rationale,
		EvaluationEvidence:    eval.Evidence,
		AuthorityRequirements: cand.AuthorityRequirements,
		MinimumEnforcement:    cand.MinimumEnforcement,
		AccessContext:         cand.AccessContext,
		RequiresApproval:      requiresApproval,
		State:                 ProposalProposed,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	digest := proposalDigest(proposal)

	if requiresApproval {
		if d.approvals == nil {
			return AcquisitionProposal{}, errors.New("approval service is required for gated capability acquisition")
		}
		validity := opts.ApprovalValidity
		if validity <= 0 {
			validity = 48 * time.Hour
		}

		approvalRec, err := d.approvals.Create(ctx, approvals.CreateRequest{
			SubjectKind:       "CAPABILITY_ACQUISITION",
			SubjectID:         proposal.ID,
			RequestDigest:     digest,
			PolicyDecisionID:  domain.ID("policy-capability-acquisition"),
			RequiredApprovers: []domain.ID{d.ownerID},
			RequestedBy:       domain.ID("summa42-capability-engine"),
			ExpiresAt:         now.Add(validity),
		})
		if err != nil {
			return AcquisitionProposal{}, fmt.Errorf("create approval request for capability acquisition: %w", err)
		}
		proposal.ApprovalID = approvalRec.ID
		proposal.State = ProposalApprovalPending
	} else {
		proposal.State = ProposalApproved
	}

	evidenceJSON, err := json.Marshal(proposal.EvaluationEvidence)
	if err != nil {
		return AcquisitionProposal{}, err
	}
	authorityJSON, err := json.Marshal(proposal.AuthorityRequirements)
	if err != nil {
		return AcquisitionProposal{}, err
	}

	_, err = d.store.DB().ExecContext(ctx, `
		INSERT INTO capability_acquisition_proposals(
			proposal_id, semantic_name, candidate_provider, candidate_version, incumbent_provider,
			rationale, evaluation_evidence_json, authority_requirements_json, minimum_enforcement,
			access_context, requires_approval, approval_id, state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		proposal.ID, proposal.SemanticName, proposal.CandidateProvider, proposal.CandidateVersion,
		proposal.IncumbentProvider, proposal.Rationale, string(evidenceJSON), string(authorityJSON),
		proposal.MinimumEnforcement, proposal.AccessContext, boolInt(proposal.RequiresApproval),
		string(proposal.ApprovalID), proposal.State, formatTime(now), formatTime(now),
	)
	if err != nil {
		return AcquisitionProposal{}, fmt.Errorf("persist acquisition proposal: %w", err)
	}

	return proposal, nil
}

func (d *DiscoveryEngine) GetProposal(ctx context.Context, proposalID domain.ID) (AcquisitionProposal, error) {
	if err := d.configured(); err != nil {
		return AcquisitionProposal{}, err
	}
	return d.loadProposal(ctx, proposalID)
}

func (d *DiscoveryEngine) ListProposals(ctx context.Context, semanticName string) ([]AcquisitionProposal, error) {
	if err := d.configured(); err != nil {
		return nil, err
	}
	semanticName = strings.TrimSpace(semanticName)

	var query string
	var args []any
	if semanticName != "" {
		query = `SELECT proposal_id, semantic_name, candidate_provider, candidate_version, incumbent_provider,
		                rationale, evaluation_evidence_json, authority_requirements_json, minimum_enforcement,
		                access_context, requires_approval, approval_id, state, created_at, updated_at
		         FROM capability_acquisition_proposals
		         WHERE semantic_name = ?
		         ORDER BY created_at DESC`
		args = append(args, semanticName)
	} else {
		query = `SELECT proposal_id, semantic_name, candidate_provider, candidate_version, incumbent_provider,
		                rationale, evaluation_evidence_json, authority_requirements_json, minimum_enforcement,
		                access_context, requires_approval, approval_id, state, created_at, updated_at
		         FROM capability_acquisition_proposals
		         ORDER BY created_at DESC`
	}

	rows, err := d.store.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proposals []AcquisitionProposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, p)
	}
	return proposals, rows.Err()
}

func (d *DiscoveryEngine) ActivateAcquisition(
	ctx context.Context,
	proposalID domain.ID,
	factory func() (AssessableProvider, error),
) error {
	if err := d.configured(); err != nil {
		return err
	}
	if factory == nil {
		return errors.New("provider factory is required for activation")
	}

	proposal, err := d.loadProposal(ctx, proposalID)
	if err != nil {
		return err
	}

	if proposal.State == ProposalActivated {
		return nil // Already activated
	}

	digest := proposalDigest(proposal)

	if proposal.RequiresApproval {
		if proposal.ApprovalID == "" {
			return errors.New("proposal requires approval but no approval id is present")
		}
		isApproved, err := d.approvals.IsApproved(ctx, proposal.ApprovalID, digest)
		if err != nil {
			return fmt.Errorf("check approval: %w", err)
		}
		if !isApproved {
			return fmt.Errorf("acquisition proposal %s is not approved by Owner/Policy", proposalID)
		}
		// Consume the approval so it cannot be reused
		if err := d.approvals.Consume(ctx, proposal.ApprovalID, digest); err != nil {
			return fmt.Errorf("consume acquisition approval: %w", err)
		}
	} else if proposal.State != ProposalApproved {
		return fmt.Errorf("proposal %s is in state %s, cannot activate", proposalID, proposal.State)
	}

	provider, err := factory()
	if err != nil {
		return fmt.Errorf("construct provider adapter: %w", err)
	}
	if provider.Name() != proposal.CandidateProvider {
		return fmt.Errorf("constructed provider has name %q, want %q", provider.Name(), proposal.CandidateProvider)
	}

	// 1. Register provider in Registry
	if err := d.registry.RegisterProvider(provider); err != nil {
		return fmt.Errorf("register provider in capability registry: %w", err)
	}

	// 2. Assess and onboard provider capabilities
	assessments, err := d.registry.AssessProvider(ctx, proposal.CandidateProvider)
	if err != nil {
		return fmt.Errorf("assess newly registered provider: %w", err)
	}
	if len(assessments) == 0 {
		return fmt.Errorf("newly registered provider %s produced no valid assessments", proposal.CandidateProvider)
	}

	// 3. Mark proposal as ACTIVATED
	now := d.clock.Now().UTC()
	_, err = d.store.DB().ExecContext(ctx, `
		UPDATE capability_acquisition_proposals
		SET state = ?, updated_at = ?
		WHERE proposal_id = ?`,
		ProposalActivated, formatTime(now), proposal.ID,
	)
	if err != nil {
		return fmt.Errorf("update proposal state to ACTIVATED: %w", err)
	}

	return nil
}

func (d *DiscoveryEngine) RejectProposal(ctx context.Context, proposalID domain.ID, approver domain.ID, reason string) error {
	if err := d.configured(); err != nil {
		return err
	}
	proposal, err := d.loadProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if proposal.State == ProposalActivated {
		return errors.New("cannot reject an already activated proposal")
	}

	if proposal.ApprovalID != "" && d.approvals != nil {
		digest := proposalDigest(proposal)
		_ = d.approvals.Reject(ctx, proposal.ApprovalID, approver, digest)
	}

	now := d.clock.Now().UTC()
	_, err = d.store.DB().ExecContext(ctx, `
		UPDATE capability_acquisition_proposals
		SET state = ?, updated_at = ?
		WHERE proposal_id = ?`,
		ProposalRejected, formatTime(now), proposal.ID,
	)
	return err
}

func (d *DiscoveryEngine) loadProposal(ctx context.Context, proposalID domain.ID) (AcquisitionProposal, error) {
	row := d.store.DB().QueryRowContext(ctx, `
		SELECT proposal_id, semantic_name, candidate_provider, candidate_version, incumbent_provider,
		       rationale, evaluation_evidence_json, authority_requirements_json, minimum_enforcement,
		       access_context, requires_approval, approval_id, state, created_at, updated_at
		FROM capability_acquisition_proposals
		WHERE proposal_id = ?`, proposalID,
	)
	return scanProposal(row)
}

func scanProposal(scanner interface {
	Scan(dest ...any) error
}) (AcquisitionProposal, error) {
	var p AcquisitionProposal
	var evidenceJSON, authorityJSON string
	var reqApproval int
	var approvalID sql.NullString
	var createdAtStr, updatedAtStr string

	err := scanner.Scan(
		&p.ID, &p.SemanticName, &p.CandidateProvider, &p.CandidateVersion, &p.IncumbentProvider,
		&p.Rationale, &evidenceJSON, &authorityJSON, &p.MinimumEnforcement,
		&p.AccessContext, &reqApproval, &approvalID, &p.State, &createdAtStr, &updatedAtStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AcquisitionProposal{}, errors.New("capability acquisition proposal not found")
	}
	if err != nil {
		return AcquisitionProposal{}, err
	}

	p.RequiresApproval = reqApproval == 1
	if approvalID.Valid {
		p.ApprovalID = domain.ID(approvalID.String)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &p.EvaluationEvidence); err != nil {
		return AcquisitionProposal{}, fmt.Errorf("decode evaluation evidence: %w", err)
	}
	if err := json.Unmarshal([]byte(authorityJSON), &p.AuthorityRequirements); err != nil {
		return AcquisitionProposal{}, fmt.Errorf("decode authority requirements: %w", err)
	}
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return AcquisitionProposal{}, fmt.Errorf("parse created_at: %w", err)
	}
	p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		return AcquisitionProposal{}, fmt.Errorf("parse updated_at: %w", err)
	}

	return p, nil
}

func (d *DiscoveryEngine) configured() error {
	if d == nil || d.store == nil || d.clock == nil || d.registry == nil {
		return errors.New("discovery engine is not configured")
	}
	return nil
}

func proposalDigest(p AcquisitionProposal) string {
	h := sha256.New()
	fmt.Fprintf(h, "semantic:%s|provider:%s|version:%s|context:%s|enforcement:%s|authority:%s|evidence:%s",
		p.SemanticName, p.CandidateProvider, p.CandidateVersion, p.AccessContext,
		p.MinimumEnforcement, strings.Join(p.AuthorityRequirements, ","), strings.Join(p.EvaluationEvidence, ","),
	)
	return hex.EncodeToString(h.Sum(nil))
}

func isLicenseAcceptable(license string) bool {
	clean := strings.ToUpper(strings.TrimSpace(license))
	switch clean {
	case "MIT", "APACHE-2.0", "APACHE 2.0", "BSD-2-CLAUSE", "BSD-3-CLAUSE", "ISC", "CC0-1.0", "UNLICENSE", "PROPRIETARY-APPROVED":
		return true
	default:
		return false
	}
}

func extractCost(metadata map[string]any) float64 {
	if metadata == nil {
		return 0
	}
	for _, k := range []string{"unit_cost", "cost", "price", "estimated_cost"} {
		if v, ok := metadata[k]; ok {
			switch num := v.(type) {
			case float64:
				return num
			case int:
				return float64(num)
			case int64:
				return float64(num)
			}
		}
	}
	return 0
}
