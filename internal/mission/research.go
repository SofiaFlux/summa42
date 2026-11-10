package mission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrEmptyStatement is returned when the raw owner statement is blank.
	ErrEmptyStatement = errors.New("raw owner statement cannot be empty")
)

// FindingSeverity categorizes the risk level of an identified issue.
type FindingSeverity string

const (
	FindingSeverityInfo     FindingSeverity = "INFO"
	FindingSeverityWarning  FindingSeverity = "WARNING"
	FindingSeverityHigh     FindingSeverity = "HIGH"
	FindingSeverityCritical FindingSeverity = "CRITICAL"
)

// AmbiguityFinding details vague or subjective phrasing that requires clarification.
type AmbiguityFinding struct {
	Term           string          `json:"term"`
	Location       string          `json:"location"`
	Reason         string          `json:"reason"`
	Recommendation string          `json:"recommendation"`
	Severity       FindingSeverity `json:"severity"`
}

// ConflictFinding describes competing objectives or dangerous proxy gaming risks.
type ConflictFinding struct {
	Severity             FindingSeverity `json:"severity"`
	Description          string          `json:"description"`
	CompetingValues      [2]string       `json:"competing_values"`
	ProxyRisk            string          `json:"proxy_risk,omitempty"`
	MitigatingConstraint string          `json:"mitigating_constraint,omitempty"`
	SuggestedTradeOff    TradeOffRule    `json:"suggested_trade_off,omitempty"`
}

// AdversarialScenario describes a potential exploit vector where an agent games a proxy metric.
type AdversarialScenario struct {
	Name                      string          `json:"name"`
	RiskLevel                 FindingSeverity `json:"risk_level"`
	GamingVector              string          `json:"gaming_vector"`
	Impact                    string          `json:"impact"`
	RecommendedCountermeasure string          `json:"recommended_countermeasure"`
}

// EvidenceSummary captures provenance inputs and cryptographic evidence of the research run.
type EvidenceSummary struct {
	InputHash    string    `json:"input_hash"`
	Timestamp    time.Time `json:"timestamp"`
	SourceInputs []string  `json:"source_inputs"`
	EvidenceRefs []string  `json:"evidence_refs"`
}

// ResearchInput encapsulates inputs for the research pipeline.
type ResearchInput struct {
	OwnerStatement  string    `json:"owner_statement"`
	OrgContext      string    `json:"org_context,omitempty"`
	Policies        []string  `json:"policies,omitempty"`
	EvidenceRefs    []string  `json:"evidence_refs,omitempty"`
	AuthorPrincipal Principal `json:"author_principal"`
	BaseMission     *Mission  `json:"base_mission,omitempty"`
}

// MissionProposal represents the non-authoritative output of the research engine.
type MissionProposal struct {
	ProposalID                domain.ID             `json:"proposal_id"`
	ProposedMission           Mission               `json:"proposed_mission"`
	Ambiguities               []AmbiguityFinding    `json:"ambiguities,omitempty"`
	Conflicts                 []ConflictFinding     `json:"conflicts,omitempty"`
	AdversarialScenarios      []AdversarialScenario `json:"adversarial_scenarios,omitempty"`
	ExtractedAssumptions      []string              `json:"extracted_assumptions,omitempty"`
	IdentifiedUnknowns        []string              `json:"identified_unknowns,omitempty"`
	SuggestedRevisionTriggers []string              `json:"suggested_revision_triggers,omitempty"`
	Evidence                  EvidenceSummary       `json:"evidence"`
	IsDangerousProxyDetected  bool                  `json:"is_dangerous_proxy_detected"`
	RequiresClarification     bool                  `json:"requires_clarification"`
}

// MissionResearcher defines the interface for intent analysis and mission proposal synthesis.
type MissionResearcher interface {
	Research(ctx context.Context, input ResearchInput) (*MissionProposal, error)
}

// Researcher implements deterministic intent analysis, ambiguity detection, and adversarial proxy checking.
type Researcher struct{}

// NewResearcher creates a new instance of Researcher.
func NewResearcher() *Researcher {
	return &Researcher{}
}

// Research analyzes the raw statement, context, and policies to produce a structured, non-authoritative proposal.
func (r *Researcher) Research(ctx context.Context, input ResearchInput) (*MissionProposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	statement := strings.TrimSpace(input.OwnerStatement)
	if statement == "" {
		return nil, ErrEmptyStatement
	}

	// 1. Evidence hashing for deterministic provenance
	h := sha256.New()
	fmt.Fprintf(h, "statement:%d:%s\n", len(statement), statement)
	fmt.Fprintf(h, "org_context:%d:%s\n", len(input.OrgContext), input.OrgContext)
	for i, p := range input.Policies {
		fmt.Fprintf(h, "policy[%d]:%d:%s\n", i, len(p), p)
	}
	inputHash := hex.EncodeToString(h.Sum(nil))

	// 2. Ambiguity Detection
	ambiguities := detectAmbiguities(statement)

	// 3. Conflict Analysis & Adversarial Interpretation
	conflicts, scenarios, dangerousProxyDetected := analyzeConflictsAndAdversarialVectors(statement, input.Policies)

	// 4. Extraction of Assumptions, Unknowns, and Revision Triggers
	assumptions, unknowns, triggers := extractOperationalParameters(statement, input.OrgContext, input.Policies)

	// 5. Synthesize Non-Authoritative Mission Draft
	version := 1
	var baseVersion int
	if input.BaseMission != nil {
		version = input.BaseMission.Version + 1
		baseVersion = input.BaseMission.Version
	}

	// Compile complete 10-dimension mission
	proposedMission := assembleDraftMission(
		statement,
		input,
		version,
		baseVersion,
		conflicts,
		scenarios,
		assumptions,
		unknowns,
		triggers,
	)

	requiresClarification := len(ambiguities) > 0 || dangerousProxyDetected
	for _, c := range conflicts {
		if c.Severity == FindingSeverityCritical || c.Severity == FindingSeverityHigh {
			requiresClarification = true
			break
		}
	}

	var sourceInputs []string
	sourceInputs = append(sourceInputs, fmt.Sprintf("raw_statement: %s", statement))
	if input.OrgContext != "" {
		sourceInputs = append(sourceInputs, fmt.Sprintf("org_context: %s", input.OrgContext))
	}
	for _, p := range input.Policies {
		sourceInputs = append(sourceInputs, fmt.Sprintf("policy: %s", p))
	}

	proposal := &MissionProposal{
		ProposalID:                domain.NewID("proposal"),
		ProposedMission:           proposedMission,
		Ambiguities:               ambiguities,
		Conflicts:                 conflicts,
		AdversarialScenarios:      scenarios,
		ExtractedAssumptions:      assumptions,
		IdentifiedUnknowns:        unknowns,
		SuggestedRevisionTriggers: triggers,
		Evidence: EvidenceSummary{
			InputHash:    inputHash,
			Timestamp:    time.Now().UTC(),
			SourceInputs: sourceInputs,
			EvidenceRefs: cloneStringSlice(input.EvidenceRefs),
		},
		IsDangerousProxyDetected: dangerousProxyDetected,
		RequiresClarification:    requiresClarification,
	}

	return proposal, nil
}

func detectAmbiguities(statement string) []AmbiguityFinding {
	type patternRule struct {
		pattern        *regexp.Regexp
		term           string
		reason         string
		recommendation string
		severity       FindingSeverity
	}

	rules := []patternRule{
		{
			pattern:        regexp.MustCompile(`(?i)\b(fastest)\b`),
			term:           "fastest",
			reason:         "Subjective superlative without explicit latency SLA or throughput target.",
			recommendation: "Define bounded latency SLO (e.g., p99 < 50ms) and target requests per second.",
			severity:       FindingSeverityWarning,
		},
		{
			pattern:        regexp.MustCompile(`(?i)\b(best)\b`),
			term:           "best",
			reason:         "Unqualified qualitative descriptor with no objective benchmark.",
			recommendation: "Replace with measurable evaluation criteria or explicit benchmark metrics.",
			severity:       FindingSeverityWarning,
		},
		{
			pattern:        regexp.MustCompile(`(?i)\b(maximum|maximize)\b`),
			term:           "maximum/maximize",
			reason:         "Unconstrained optimization directive vulnerable to Goodhart's Law and proxy gaming.",
			recommendation: "Specify an acceptable target threshold and pair with bounded constraint guardrails.",
			severity:       FindingSeverityHigh,
		},
		{
			pattern:        regexp.MustCompile(`(?i)\b(lowest possible|cheap)\b`),
			term:           "lowest possible",
			reason:         "Cost minimization without quality floor can trigger catastrophic degradation.",
			recommendation: "Specify a concrete monthly cloud cost budget with minimum service reliability floors.",
			severity:       FindingSeverityWarning,
		},
		{
			pattern:        regexp.MustCompile(`(?i)\b(as soon as possible|asap)\b`),
			term:           "as soon as possible",
			reason:         "Undefined timeline creates ambiguity between velocity and verification rigor.",
			recommendation: "Establish explicit delivery milestones with mandatory verification gates.",
			severity:       FindingSeverityInfo,
		},
		{
			pattern:        regexp.MustCompile(`(?i)\b(acceptable quality)\b`),
			term:           "acceptable quality",
			reason:         "Subjective quality statement with no verification oracle.",
			recommendation: "Formulate concrete test coverage, lint, and security regression gates.",
			severity:       FindingSeverityWarning,
		},
	}

	var findings []AmbiguityFinding
	for _, r := range rules {
		matches := r.pattern.FindAllStringIndex(statement, -1)
		for _, loc := range matches {
			match := statement[loc[0]:loc[1]]
			findings = append(findings, AmbiguityFinding{
				Term:           match,
				Location:       fmt.Sprintf("char %d-%d", loc[0], loc[1]),
				Reason:         r.reason,
				Recommendation: r.recommendation,
				Severity:       r.severity,
			})
		}
	}

	return findings
}

func analyzeConflictsAndAdversarialVectors(statement string, policies []string) ([]ConflictFinding, []AdversarialScenario, bool) {
	var conflicts []ConflictFinding
	var scenarios []AdversarialScenario
	var dangerousProxy bool

	stmtLower := strings.ToLower(statement)

	// Pattern 1: Revenue / Growth at all costs vs Trust / Ethics
	if strings.Contains(stmtLower, "at all costs") ||
		(strings.Contains(stmtLower, "revenue") && (strings.Contains(stmtLower, "friction") || strings.Contains(stmtLower, "cancellation"))) ||
		(strings.Contains(stmtLower, "maximize") && strings.Contains(stmtLower, "revenue")) {

		dangerousProxy = true
		conflicts = append(conflicts, ConflictFinding{
			Severity:    FindingSeverityHigh,
			Description: "High-risk proxy gaming: unconstrained revenue or growth pursuit conflicts directly with customer trust, ethics, and long-term brand equity.",
			CompetingValues: [2]string{
				"revenue",
				"trust",
			},
			ProxyRisk:            "Goodhart's Law trap: Autonomous workers may deploy dark patterns, conceal cancellation flows, or introduce deliberate user friction to artificially inflate retention and revenue.",
			MitigatingConstraint: "Prohibit coercive retention mechanisms, deceptive UX flows, and opaque subscription cancellation barriers.",
			SuggestedTradeOff: TradeOffRule{
				Priority:    "User Trust and Consent",
				Subordinate: "Short-term Revenue",
				Rationale:   "Long-term user trust and regulatory compliance take precedence over short-term revenue extraction.",
			},
		})

		scenarios = append(scenarios, AdversarialScenario{
			Name:                      "Dark Pattern & Cancellation Obstruction",
			RiskLevel:                 FindingSeverityHigh,
			GamingVector:              "Autonomous workers obstruct subscription cancellation flows and introduce high user friction to artificially suppress churn and maximize short-term revenue.",
			Impact:                    "Severe customer backlash, reputation destruction, and regulatory fines for deceptive business practices.",
			RecommendedCountermeasure: "Mandate single-click self-service cancellation and periodic third-party UX audits.",
		})
	}

	// Pattern 2: Zero errors vs Availability / Throughput
	if strings.Contains(stmtLower, "zero error") || strings.Contains(stmtLower, "100% success rate") {
		dangerousProxy = true
		conflicts = append(conflicts, ConflictFinding{
			Severity:    FindingSeverityHigh,
			Description: "Zero-error target conflicts with system availability and throughput: an agent can achieve zero errors by refusing to process risky traffic.",
			CompetingValues: [2]string{
				"error_rate",
				"availability",
			},
			ProxyRisk:            "Shedding / refusal gaming: Autonomous workers reject or drop incoming requests that carry non-trivial execution risk to prevent error metric degradation.",
			MitigatingConstraint: "Enforce minimum availability floor (e.g. >= 99.9%) and request acceptance obligations alongside error rate budgets.",
			SuggestedTradeOff: TradeOffRule{
				Priority:    "Service Availability & User Access",
				Subordinate: "Zero-Error Perfection",
				Rationale:   "Maintaining service availability for users is prioritized over artificial zero-error statistics achieved via request refusal.",
			},
		})

		scenarios = append(scenarios, AdversarialScenario{
			Name:                      "Request Refusal & Traffic Dropping",
			RiskLevel:                 FindingSeverityHigh,
			GamingVector:              "Worker drops or rejects uncertain requests with 503 or early refusal to prevent any potential runtime errors from counting against its success rate metric.",
			Impact:                    "Denial of service for end-users while internal dashboards report misleading 100% success rates.",
			RecommendedCountermeasure: "Bind error metrics to processed volume with strict minimum processing quotas.",
		})
	}

	// Pattern 3: Velocity vs Verification / Correctness
	if (strings.Contains(stmtLower, "bypass") && strings.Contains(stmtLower, "test")) ||
		strings.Contains(stmtLower, "without test") ||
		strings.Contains(stmtLower, "immediately to production") {

		dangerousProxy = true
		conflicts = append(conflicts, ConflictFinding{
			Severity:    FindingSeverityCritical,
			Description: "Bypassing verification and test suites directly violates system integrity and safety invariants.",
			CompetingValues: [2]string{
				"velocity",
				"correctness",
			},
			ProxyRisk:            "Premature deployment of corrupted or insecure components leading to catastrophic state corruption.",
			MitigatingConstraint: "Mandatory verification, automated regression checks, and canary gates cannot be bypassed by any principal.",
			SuggestedTradeOff: TradeOffRule{
				Priority:    "System Safety and Correctness",
				Subordinate: "Deployment Velocity",
				Rationale:   "Correctness, regression testing, and data integrity strictly override speed of release.",
			},
		})

		scenarios = append(scenarios, AdversarialScenario{
			Name:                      "Unverified Production Deployment",
			RiskLevel:                 FindingSeverityCritical,
			GamingVector:              "Worker skips test execution and compliance gates to report early feature completion.",
			Impact:                    "Unchecked bugs, data loss, and security vulnerabilities pushed into production.",
			RecommendedCountermeasure: "Hardware-enforced or immutable CI branch protection blocking unverified commits.",
		})
	}

	// Pattern 4: Organizational Policy Conflicts
	for _, policy := range policies {
		polLower := strings.ToLower(policy)
		// Check financial transfer limits vs unconstrained automation
		if strings.Contains(polLower, "human approval") && strings.Contains(polLower, "transfer") {
			if strings.Contains(stmtLower, "without human intervention") ||
				strings.Contains(stmtLower, "fully automate") && (strings.Contains(stmtLower, "transfer") || strings.Contains(stmtLower, "$")) {
				dangerousProxy = true
				conflicts = append(conflicts, ConflictFinding{
					Severity:    FindingSeverityCritical,
					Description: fmt.Sprintf("Critical policy conflict: Statement attempts automated transfer bypassing mandatory human approval policy (%s).", policy),
					CompetingValues: [2]string{
						"automation",
						"human_governance",
					},
					ProxyRisk:            "Unsupervised financial execution risk: autonomous agents executing irreversible wire transfers without human sovereign veto.",
					MitigatingConstraint: "All financial transfers exceeding defined policy thresholds require multi-party human sovereign approval.",
					SuggestedTradeOff: TradeOffRule{
						Priority:    "Human Governance and Policy Compliance",
						Subordinate: "End-to-End Automation",
						Rationale:   "Organizational financial safeguards and human approval gates strictly override autonomous execution.",
					},
				})
			}
		}
	}

	return conflicts, scenarios, dangerousProxy
}

func extractOperationalParameters(statement, orgContext string, policies []string) ([]string, []string, []string) {
	var assumptions []string
	var unknowns []string
	var triggers []string

	combined := strings.ToLower(statement + " " + orgContext)

	// Assumptions extraction via regex (case-insensitive, matching variations like "assuming", "assumes")
	reAssuming := regexp.MustCompile(`(?i)\bassum(?:ing|e|es)\s+([^.;]+)`)
	matches := reAssuming.FindAllStringSubmatch(statement+" "+orgContext, -1)
	for _, m := range matches {
		if len(m) > 1 {
			clause := strings.TrimSpace(m[1])
			if clause != "" {
				assumptions = append(assumptions, fmt.Sprintf("Assumes %s.", clause))
			}
		}
	}
	if len(assumptions) == 0 {
		assumptions = append(assumptions, "Assumes underlying infrastructure and cloud dependencies operate within standard SLA parameters.")
		assumptions = append(assumptions, "Assumes organizational security and compliance policies remain consistent during execution.")
	}

	// Unknowns
	if strings.Contains(combined, "crm") || strings.Contains(combined, "migrate") || strings.Contains(combined, "migration") {
		unknowns = append(unknowns, "Unknown total data volume and cutover downtime tolerance of existing legacy systems.")
		unknowns = append(unknowns, "Unknown third-party API rate limits and integration latency under peak workloads.")
	}
	if strings.Contains(combined, "transaction") || strings.Contains(combined, "revenue") {
		unknowns = append(unknowns, "Unknown peak transaction volume and concurrent contention patterns.")
	}
	if len(unknowns) == 0 {
		unknowns = append(unknowns, "Unknown edge-case input distributions and anomalous user traffic behavior.")
		unknowns = append(unknowns, "Unknown long-term infrastructure scaling cost under unconstrained workloads.")
	}

	// Revision Triggers
	triggers = append(triggers, "Monthly infrastructure or operational expenditures exceed budgeted financial bounds.")
	triggers = append(triggers, "Service error rate, latency SLO breach, or availability drops below defined acceptance floors.")
	triggers = append(triggers, "Discovery of previously unmodeled security vulnerability, data loss event, or regulatory non-compliance.")
	if strings.Contains(combined, "revenue") || strings.Contains(combined, "user") {
		triggers = append(triggers, "Customer trust index declines or subscription churn rate increases abnormally.")
	}

	return assumptions, unknowns, triggers
}

func assembleDraftMission(
	statement string,
	input ResearchInput,
	version int,
	baseVersion int,
	conflicts []ConflictFinding,
	scenarios []AdversarialScenario,
	assumptions []string,
	unknowns []string,
	triggers []string,
) Mission {
	now := time.Now().UTC()

	// Desired outcomes
	outcomes := []string{
		"Successfully fulfill the owner statement objectives within defined safety and compliance bounds.",
	}
	if strings.Contains(strings.ToLower(statement), "gdpr") {
		outcomes = append(outcomes, "Achieve verified compliance with GDPR and relevant privacy regulations.")
	}
	if strings.Contains(strings.ToLower(statement), "throughput") {
		outcomes = append(outcomes, "Sustain high transaction throughput without double-spending or data corruption.")
	}

	// Obligations
	var obligations []string
	obligations = append(obligations, "Maintain immutable, cryptographically verifiable audit trails of all critical state transitions.")
	for _, p := range input.Policies {
		obligations = append(obligations, p)
	}

	// Constraints
	var constraints []string
	constraints = append(constraints, "Strict adherence to organizational governance policies and human trust boundaries.")
	for _, c := range conflicts {
		if c.MitigatingConstraint != "" {
			constraints = append(constraints, c.MitigatingConstraint)
		}
	}

	// Non-goals
	var nonGoals []string
	nonGoals = append(nonGoals, "Unconstrained metric optimization at the expense of safety, compliance, or human trust.")
	for _, s := range scenarios {
		nonGoals = append(nonGoals, fmt.Sprintf("Explotative behavior: %s", s.Name))
	}

	// Trade-off rules
	var tradeOffs []TradeOffRule
	for _, c := range conflicts {
		if c.SuggestedTradeOff.Priority != "" {
			tradeOffs = append(tradeOffs, c.SuggestedTradeOff)
		}
	}
	// Fallback trade-off rule if none generated
	if len(tradeOffs) == 0 {
		tradeOffs = append(tradeOffs, TradeOffRule{
			Priority:    "Safety and Data Integrity",
			Subordinate: "Execution Velocity",
			Rationale:   "Preserving system correctness and user safety is strictly prioritized over execution speed.",
		})
	}

	// Human trust boundaries
	var boundaries []string
	boundaries = append(boundaries, "Autonomous workers cannot self-approve mission modifications or override safety constraints.")
	for _, p := range input.Policies {
		if strings.Contains(strings.ToLower(p), "human approval") {
			boundaries = append(boundaries, fmt.Sprintf("Human sovereign gate: %s", p))
		}
	}

	var sourceInputs []string
	sourceInputs = append(sourceInputs, statement)
	sourceInputs = append(sourceInputs, input.Policies...)

	missionID := domain.NewID("mission")
	if input.BaseMission != nil && input.BaseMission.ID != "" {
		missionID = input.BaseMission.ID
	}

	return Mission{
		ID:                   missionID,
		Version:              version,
		State:                MissionStateDraft,
		Purpose:              statement,
		DesiredOutcomes:      outcomes,
		Obligations:          obligations,
		Constraints:          constraints,
		NonGoals:             nonGoals,
		TradeOffRules:        tradeOffs,
		HumanTrustBoundaries: boundaries,
		Assumptions:          assumptions,
		Unknowns:             unknowns,
		RevisionTriggers:     triggers,
		Provenance: MissionProvenance{
			BaseVersion:     baseVersion,
			SourceInputs:    sourceInputs,
			EvidenceRefs:    cloneStringSlice(input.EvidenceRefs),
			AuthorPrincipal: input.AuthorPrincipal.ID,
			ChangeSummary:   "Synthesized non-authoritative mission proposal via Mission Researcher and Adversarial Intent Compiler",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}
