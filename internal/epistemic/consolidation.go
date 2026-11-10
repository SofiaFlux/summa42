package epistemic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// ConsolidationMode differentiates Sleep (deduplication & synthesis) from Dream (counterfactual hypothesis).
type ConsolidationMode string

const (
	ConsolidationModeSleep ConsolidationMode = "SLEEP"
	ConsolidationModeDream ConsolidationMode = "DREAM"
)

// TriggerKind indicates what initiated the consolidation or dreaming run.
type TriggerKind string

const (
	TriggerIdleCapacity         TriggerKind = "IDLE_CAPACITY"
	TriggerSurpriseAccumulation TriggerKind = "SURPRISE_ACCUMULATION"
	TriggerExplicitBudget       TriggerKind = "EXPLICIT_BUDGET"
)

// CognitiveBudget bounds the compute, delta volume, and speculative expansion during consolidation runs.
type CognitiveBudget struct {
	MaxDeltas     int           `json:"max_deltas"`
	MaxDuration   time.Duration `json:"max_duration"`
	MaxHypotheses int           `json:"max_hypotheses"`
}

// CognitiveBudgetConsumption reports the actual resource usage of a consolidation run.
type CognitiveBudgetConsumption struct {
	DeltasProcessed     int           `json:"deltas_processed"`
	HypothesesGenerated int           `json:"hypotheses_generated"`
	DurationConsumed    time.Duration `json:"duration_consumed"`
}

// SynthesizedExperience represents a cross-attempt procedural learning extracted from recurring friction.
type SynthesizedExperience struct {
	ID                   domain.ID   `json:"id"`
	Pattern              string      `json:"pattern"`
	RootCause            string      `json:"root_cause"`
	RecommendedProcedure string      `json:"recommended_procedure"`
	SourceAttemptIDs     []domain.ID `json:"source_attempt_ids"`
	EvidenceIDs          []string    `json:"evidence_ids"`
	CorroborationCount   int         `json:"corroboration_count"`
}

// Hypothesis represents speculative, counterfactual reasoning generated during dreaming.
// Hard Invariant: IsCanonical is ALWAYS false; hypotheses can never bypass independent verification gates.
type Hypothesis struct {
	ID                     domain.ID `json:"id"`
	Statement              string    `json:"statement"`
	Premise                string    `json:"premise"`
	CounterfactualScenario string    `json:"counterfactual_scenario"`
	SpeculativeLevel       string    `json:"speculative_level"`
	IsCanonical            bool      `json:"is_canonical"` // Invariant: ALWAYS false
}

// ExperimentProposal specifies a bounded empirical test to falsify or validate a speculative hypothesis.
type ExperimentProposal struct {
	ID                     domain.ID `json:"id"`
	HypothesisID           domain.ID `json:"hypothesis_id"`
	Objective              string    `json:"objective"`
	TestFixtureDescription string    `json:"test_fixture_description"`
	SafetyBounds           string    `json:"safety_bounds"`
}

// ConsolidationRun records the execution metrics, produced experiences, and hypotheses of an offline run.
type ConsolidationRun struct {
	RunID                  domain.ID                  `json:"run_id"`
	Mode                   ConsolidationMode          `json:"mode"`
	Trigger                TriggerKind                `json:"trigger"`
	DeltasProcessed        int                        `json:"deltas_processed"`
	ExperiencesSynthesized []SynthesizedExperience    `json:"experiences_synthesized,omitempty"`
	HypothesesGenerated    []Hypothesis               `json:"hypotheses_generated,omitempty"`
	ExperimentProposals    []ExperimentProposal       `json:"experiment_proposals,omitempty"`
	BudgetConsumed         CognitiveBudgetConsumption `json:"budget_consumed"`
	ExecutedAt             time.Time                  `json:"executed_at"`
}

// SleepDreamRunner manages offline consolidation (Sleep) and counterfactual hypothesis generation (Dream).
type SleepDreamRunner struct{}

// NewSleepDreamRunner creates a new instance of SleepDreamRunner.
func NewSleepDreamRunner() *SleepDreamRunner {
	return &SleepDreamRunner{}
}

// RunSleep performs observation deduplication and cross-attempt experience synthesis under strict budget bounds.
func (r *SleepDreamRunner) RunSleep(
	ctx context.Context,
	deltas []KnowledgeDelta,
	budget CognitiveBudget,
	trigger TriggerKind,
) (ConsolidationRun, error) {
	if err := ctx.Err(); err != nil {
		return ConsolidationRun{}, err
	}

	start := time.Now().UTC()
	limit := len(deltas)
	if budget.MaxDeltas > 0 && budget.MaxDeltas < limit {
		limit = budget.MaxDeltas
	}

	// Pattern clustering for experience synthesis
	type patternCluster struct {
		pattern    string
		attempts   []domain.ID
		evidence   []string
		candidates []string
	}
	clusters := make(map[string]*patternCluster)

	processed := 0
	for i := 0; i < limit; i++ {
		if budget.MaxDuration > 0 && time.Since(start) > budget.MaxDuration {
			break
		}
		d := deltas[i]
		processed++
		if d.IsNoMaterialLearning {
			continue
		}

		for _, obs := range d.Observations {
			key := fmt.Sprintf("%s:%s", obs.Subject, obs.Predicate)
			c, exists := clusters[key]
			if !exists {
				c = &patternCluster{
					pattern: fmt.Sprintf("Recurring friction on %s (%s)", obs.Subject, obs.Predicate),
				}
				clusters[key] = c
			}
			c.attempts = appendUnique(c.attempts, d.SourceAttemptID)
			for _, ev := range obs.EvidenceIDs {
				c.evidence = appendUniqueString(c.evidence, ev)
			}
		}

		for _, claim := range d.ProposedClaims {
			key := fmt.Sprintf("%s:%s", claim.Subject, claim.Predicate)
			if c, exists := clusters[key]; exists {
				c.candidates = appendUniqueString(c.candidates, claim.Object)
			}
		}
	}

	var experiences []SynthesizedExperience
	for _, c := range clusters {
		if len(c.attempts) >= 2 {
			rec := "Apply standardized mitigations and pre-emptive checks"
			if len(c.candidates) > 0 {
				rec = strings.Join(c.candidates, "; ")
			}
			experiences = append(experiences, SynthesizedExperience{
				ID:                   domain.NewID("synexp"),
				Pattern:              c.pattern,
				RootCause:            "Corroborated cross-attempt operational friction",
				RecommendedProcedure: rec,
				SourceAttemptIDs:     c.attempts,
				EvidenceIDs:          c.evidence,
				CorroborationCount:   len(c.attempts),
			})
		}
	}

	return ConsolidationRun{
		RunID:                  domain.NewID("crun"),
		Mode:                   ConsolidationModeSleep,
		Trigger:                trigger,
		DeltasProcessed:        processed,
		ExperiencesSynthesized: experiences,
		BudgetConsumed: CognitiveBudgetConsumption{
			DeltasProcessed:  processed,
			DurationConsumed: time.Since(start),
		},
		ExecutedAt: start,
	}, nil
}

// RunDream derives counterfactual hypotheses and test fixtures without granting direct canonical mutation rights.
func (r *SleepDreamRunner) RunDream(
	ctx context.Context,
	experiences []SynthesizedExperience,
	budget CognitiveBudget,
	trigger TriggerKind,
) (ConsolidationRun, error) {
	if err := ctx.Err(); err != nil {
		return ConsolidationRun{}, err
	}

	start := time.Now().UTC()
	var hypotheses []Hypothesis
	var proposals []ExperimentProposal

	maxHyp := 3
	if budget.MaxHypotheses > 0 {
		maxHyp = budget.MaxHypotheses
	}

	for _, exp := range experiences {
		if len(hypotheses) >= maxHyp {
			break
		}
		if budget.MaxDuration > 0 && time.Since(start) > budget.MaxDuration {
			break
		}

		// Counterfactual derivation: what if the friction is exacerbated?
		hypID := domain.NewID("hyp")
		hyp := Hypothesis{
			ID:                     hypID,
			Statement:              fmt.Sprintf("Counterfactual: Inverting resource order prevents %s under 10x concurrency", exp.Pattern),
			Premise:                fmt.Sprintf("Observed %d corroborations of root cause: %s", exp.CorroborationCount, exp.RootCause),
			CounterfactualScenario: fmt.Sprintf("Under 10x traffic spike on %s, deterministic sorting eliminates deadlock probability", exp.Pattern),
			SpeculativeLevel:       "MEDIUM",
			IsCanonical:            false, // HARD INVARIANT: strictly non-canonical
		}
		hypotheses = append(hypotheses, hyp)

		prop := ExperimentProposal{
			ID:                     domain.NewID("expprop"),
			HypothesisID:           hypID,
			Objective:              fmt.Sprintf("Empirically validate hypothesis %s in DTU sandbox", hypID),
			TestFixtureDescription: "Synthesize hostile concurrent replay fixture with randomized thread interleaving",
			SafetyBounds:           "Isolated DTU sandbox with zero production network access and 100k token ceiling",
		}
		proposals = append(proposals, prop)
	}

	return ConsolidationRun{
		RunID:               domain.NewID("crun"),
		Mode:                ConsolidationModeDream,
		Trigger:             trigger,
		HypothesesGenerated: hypotheses,
		ExperimentProposals: proposals,
		BudgetConsumed: CognitiveBudgetConsumption{
			HypothesesGenerated: len(hypotheses),
			DurationConsumed:    time.Since(start),
		},
		ExecutedAt: start,
	}, nil
}
