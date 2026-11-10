package mission

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrIncompleteGoalProposal is returned when required fields in a goal proposal are missing.
	ErrIncompleteGoalProposal = errors.New("goal proposal is incomplete: missing required fields")

	// ErrHumanTrustBoundaryBypassed is returned when a proposal touches a human trust boundary without requiring human approval.
	ErrHumanTrustBoundaryBypassed = errors.New("goal proposal touches human trust boundary without mandatory human approval gate")

	// ErrAuthorityExpansion is returned when a proposal attempts to expand authority or resource bounds beyond mission constraints.
	ErrAuthorityExpansion = errors.New("goal proposal attempts to expand authority ceiling beyond mission constraints")

	// ErrInactiveMission is returned when attempting to propose or validate goals against a non-active mission.
	ErrInactiveMission = errors.New("cannot propose or validate goals against a non-active mission")
)

// EvidenceKind categorizes the provenance source of supporting evidence.
type EvidenceKind string

const (
	EvidenceKindLive      EvidenceKind = "LIVE"
	EvidenceKindSimulated EvidenceKind = "SIMULATED"
	EvidenceKindAssumed   EvidenceKind = "ASSUMED"
)

// SupportingEvidenceItem captures a verifiable evidence artifact and its provenance tier.
type SupportingEvidenceItem struct {
	URI         string       `json:"uri"`
	Kind        EvidenceKind `json:"kind"`
	Description string       `json:"description"`
	CapturedAt  time.Time    `json:"captured_at"`
}

// TelemetryMetric captures an observed operational metric from telemetry systems.
type TelemetryMetric struct {
	Name        string    `json:"name"`
	Value       float64   `json:"value"`
	Unit        string    `json:"unit"`
	RecordedAt  time.Time `json:"recorded_at"`
	Description string    `json:"description"`
}

// OperationalReality provides the ground truth observed telemetry and limitations for goal discovery.
type OperationalReality struct {
	ObservedTelemetry      []TelemetryMetric        `json:"observed_telemetry,omitempty"`
	ObservedEvidence       []SupportingEvidenceItem `json:"observed_evidence,omitempty"`
	CurrentLimitations     []string                 `json:"current_limitations,omitempty"`
	CandidateOpportunities []string                 `json:"candidate_opportunities,omitempty"`
}

// ReversibilityLevel classifies how easily the goal's side-effects can be undone.
type ReversibilityLevel string

const (
	ReversibilityFull         ReversibilityLevel = "FULL"
	ReversibilityPartial      ReversibilityLevel = "PARTIAL"
	ReversibilityIrreversible ReversibilityLevel = "IRREVERSIBLE"
)

// RiskLevel defines the potential operational or alignment risk of executing the goal.
type RiskLevel string

const (
	RiskLow      RiskLevel = "LOW"
	RiskMedium   RiskLevel = "MEDIUM"
	RiskHigh     RiskLevel = "HIGH"
	RiskCritical RiskLevel = "CRITICAL"
)

// ResourceEnvelope bounds the compute, cost, and duration resources allocatable to the goal.
type ResourceEnvelope struct {
	MaxCostUSD          float64       `json:"max_cost_usd"`
	MaxTokens           int           `json:"max_tokens"`
	MaxDuration         time.Duration `json:"max_duration"`
	AllowedCapabilities []string      `json:"allowed_capabilities,omitempty"`
}

// AuthorityCeiling captures the boundary conditions and required approval gates for the goal.
type AuthorityCeiling struct {
	RequiresHumanApproval    bool     `json:"requires_human_approval"`
	TouchedTrustBoundaries   []string `json:"touched_trust_boundaries,omitempty"`
	MaxAuthorizedSpendingUSD float64  `json:"max_authorized_spending_usd,omitempty"`
}

// GoalProposalStatus represents the governance state of a candidate goal proposal.
type GoalProposalStatus string

const (
	GoalProposalStatusProposed GoalProposalStatus = "PROPOSED"
	GoalProposalStatusAccepted GoalProposalStatus = "ACCEPTED"
	GoalProposalStatusRejected GoalProposalStatus = "REJECTED"
)

// GoalProposal captures a bounded, evidence-backed candidate goal derived from operational reality and active mission.
type GoalProposal struct {
	ID                        domain.ID                `json:"id"`
	MissionID                 domain.ID                `json:"mission_id"`
	MissionVersion            int                      `json:"mission_version"`
	Title                     string                   `json:"title"`
	ProblemStatement          string                   `json:"problem_statement"`
	SupportingEvidence        []SupportingEvidenceItem `json:"supporting_evidence"`
	Assumptions               []string                 `json:"assumptions"`
	ExpectedOutcome           string                   `json:"expected_outcome"`
	SuccessCriteria           []string                 `json:"success_criteria"`
	ResourceEnvelope          ResourceEnvelope         `json:"resource_envelope"`
	Reversibility             ReversibilityLevel       `json:"reversibility"`
	RiskClassification        RiskLevel                `json:"risk_classification"`
	AffectedObligations       []string                 `json:"affected_obligations,omitempty"`
	ReferencedMissionElements []string                 `json:"referenced_mission_elements"`
	AuthorityCeiling          AuthorityCeiling         `json:"authority_ceiling"`
	Status                    GoalProposalStatus       `json:"status"`
	ProposedBy                Principal                `json:"proposed_by"`
	CreatedAt                 time.Time                `json:"created_at"`
}

// ValidateGoalProposal enforces deterministic safety, completeness, and authority ceiling invariants.
func ValidateGoalProposal(p GoalProposal, m Mission) error {
	if m.State != MissionStateActive {
		return ErrInactiveMission
	}

	// Completeness checks
	if strings.TrimSpace(p.Title) == "" ||
		strings.TrimSpace(p.ProblemStatement) == "" ||
		strings.TrimSpace(p.ExpectedOutcome) == "" ||
		len(p.SuccessCriteria) == 0 ||
		len(p.SupportingEvidence) == 0 ||
		p.ResourceEnvelope.MaxCostUSD <= 0 ||
		p.Reversibility == "" ||
		p.RiskClassification == "" {
		return ErrIncompleteGoalProposal
	}

	// Cost constraint check against Mission Constraints
	maxAllowedCost := extractMaxCostConstraint(m.Constraints)
	if maxAllowedCost > 0 && p.ResourceEnvelope.MaxCostUSD > maxAllowedCost {
		return ErrAuthorityExpansion
	}

	// Human Trust Boundary check
	// 1. If TouchedTrustBoundaries is non-empty, RequiresHumanApproval MUST be true
	if len(p.AuthorityCeiling.TouchedTrustBoundaries) > 0 && !p.AuthorityCeiling.RequiresHumanApproval {
		return ErrHumanTrustBoundaryBypassed
	}

	// 2. Scan proposal fields for actions matching explicit Mission Human Trust Boundaries
	matchedBoundaries := scanTouchedTrustBoundaries(p, m.HumanTrustBoundaries)
	if len(matchedBoundaries) > 0 && !p.AuthorityCeiling.RequiresHumanApproval {
		return ErrHumanTrustBoundaryBypassed
	}

	return nil
}

// extractMaxCostConstraint extracts the dollar ceiling if defined in Mission constraints.
func extractMaxCostConstraint(constraints []string) float64 {
	reCost := regexp.MustCompile(`(?i)(?:cost|budget|spend)[^\d$]*\$?([0-9]+(?:\.[0-9]+)?)`)
	for _, c := range constraints {
		match := reCost.FindStringSubmatch(c)
		if len(match) > 1 {
			if val, err := strconv.ParseFloat(match[1], 64); err == nil && val > 0 {
				return val
			}
		}
	}
	return 0
}

// scanTouchedTrustBoundaries evaluates whether proposal text intersects with human trust boundary keywords.
func scanTouchedTrustBoundaries(p GoalProposal, boundaries []string) []string {
	var touched []string
	blob := strings.ToLower(fmt.Sprintf("%s %s %s %s",
		p.Title,
		p.ProblemStatement,
		p.ExpectedOutcome,
		strings.Join(p.SuccessCriteria, " "),
	))

	for _, b := range boundaries {
		bLower := strings.ToLower(b)
		// Check schema migration / drop
		if (strings.Contains(bLower, "schema") || strings.Contains(bLower, "migration") || strings.Contains(bLower, "drop")) &&
			(strings.Contains(blob, "schema") || strings.Contains(blob, "migration") || strings.Contains(blob, "database migration")) {
			touched = append(touched, b)
			continue
		}
		// Check financial transfer limits
		if strings.Contains(bLower, "transfer") || strings.Contains(bLower, "financial") || strings.Contains(bLower, "$") {
			if strings.Contains(blob, "transfer") || strings.Contains(blob, "wire") || strings.Contains(blob, "refund") {
				touched = append(touched, b)
				continue
			}
		}
	}
	return touched
}

// GoalResearchEngine derives evidence-backed candidate goal proposals from operational reality against active mission.
type GoalResearchEngine struct{}

// NewGoalResearchEngine creates a new instance of GoalResearchEngine.
func NewGoalResearchEngine() *GoalResearchEngine {
	return &GoalResearchEngine{}
}

// ResearchGoals analyzes operational telemetry, evidence, and limitations against active mission objectives.
func (e *GoalResearchEngine) ResearchGoals(
	ctx context.Context,
	m Mission,
	reality OperationalReality,
	principal Principal,
) ([]GoalProposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if m.State != MissionStateActive {
		return nil, ErrInactiveMission
	}

	var proposals []GoalProposal
	now := time.Now().UTC()

	// 1. Process candidate opportunities or limitations
	items := reality.CandidateOpportunities
	if len(items) == 0 {
		items = reality.CurrentLimitations
	}

	if len(items) == 0 && len(reality.ObservedEvidence) > 0 {
		items = []string{"Resolve operational limitation identified in observed telemetry evidence"}
	}

	for i, opp := range items {
		problem := opp
		if i < len(reality.CurrentLimitations) {
			problem = reality.CurrentLimitations[i]
		}

		// Find relevant mission outcomes
		var referencedElements []string
		for _, outcome := range m.DesiredOutcomes {
			// Link if keyword overlaps (latency, throughput, compliance, security, etc.)
			if hasKeywordOverlap(problem+" "+opp, outcome) {
				referencedElements = append(referencedElements, outcome)
			}
		}
		if len(referencedElements) == 0 && len(m.DesiredOutcomes) > 0 {
			referencedElements = append(referencedElements, m.DesiredOutcomes[0])
		}

		// Supporting evidence: preserve live vs simulated
		var evidence []SupportingEvidenceItem
		if len(reality.ObservedEvidence) > 0 {
			evidence = cloneEvidenceItems(reality.ObservedEvidence)
		} else {
			evidence = append(evidence, SupportingEvidenceItem{
				URI:         "telemetry://observed/reality_baseline",
				Kind:        EvidenceKindLive,
				Description: fmt.Sprintf("Observed operational limitation: %s", problem),
				CapturedAt:  now,
			})
		}

		// Authority boundaries check
		var touchedBoundaries []string
		for _, b := range m.HumanTrustBoundaries {
			if hasKeywordOverlap(problem+" "+opp, b) {
				touchedBoundaries = append(touchedBoundaries, b)
			}
		}

		// Default safe resource envelope under mission ceiling
		maxAllowedCost := extractMaxCostConstraint(m.Constraints)
		costUSD := 5.0
		if maxAllowedCost > 0 && costUSD > maxAllowedCost {
			costUSD = maxAllowedCost
		}

		p := GoalProposal{
			ID:                 domain.NewID("goalprop"),
			MissionID:          m.ID,
			MissionVersion:     m.Version,
			Title:              fmt.Sprintf("Goal: %s", opp),
			ProblemStatement:   problem,
			SupportingEvidence: evidence,
			Assumptions: []string{
				"Underlying service infrastructure remains accessible throughout goal execution.",
				"Proposed modifications maintain system invariants and zero unintended data loss.",
			},
			ExpectedOutcome: fmt.Sprintf("Operational bottleneck resolved: %s", opp),
			SuccessCriteria: []string{
				"Verification suite confirms target SLO or functional correctness without regressions.",
			},
			ResourceEnvelope: ResourceEnvelope{
				MaxCostUSD:  costUSD,
				MaxTokens:   50000,
				MaxDuration: 2 * time.Hour,
			},
			Reversibility:             ReversibilityFull,
			RiskClassification:        RiskMedium,
			AffectedObligations:       cloneStringSlice(m.Obligations),
			ReferencedMissionElements: referencedElements,
			AuthorityCeiling: AuthorityCeiling{
				RequiresHumanApproval:  len(touchedBoundaries) > 0,
				TouchedTrustBoundaries: touchedBoundaries,
			},
			Status:     GoalProposalStatusProposed,
			ProposedBy: principal,
			CreatedAt:  now,
		}

		if err := ValidateGoalProposal(p, m); err == nil {
			proposals = append(proposals, p)
		}
	}

	return proposals, nil
}

func hasKeywordOverlap(text, target string) bool {
	t1 := strings.ToLower(text)
	t2 := strings.ToLower(target)
	keywords := []string{"latency", "slo", "transaction", "reconcil", "compliance", "gdpr", "audit", "security", "schema", "cost", "index"}
	for _, kw := range keywords {
		if strings.Contains(t1, kw) && strings.Contains(t2, kw) {
			return true
		}
	}
	return false
}

func cloneEvidenceItems(items []SupportingEvidenceItem) []SupportingEvidenceItem {
	out := make([]SupportingEvidenceItem, len(items))
	copy(out, items)
	return out
}
