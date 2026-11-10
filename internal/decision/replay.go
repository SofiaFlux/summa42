package decision

import (
	"context"
	"time"
)

// ReplayFixture represents a historic decision scenario with observed downstream ground truth.
type ReplayFixture struct {
	FixtureID      string          `json:"fixture_id"`
	Description    string          `json:"description,omitempty"`
	Request        DecisionRequest `json:"request"`
	RecordedResult DecisionResult  `json:"recorded_result"`
	ActualOutcome  string          `json:"actual_outcome,omitempty"`
	Success        bool            `json:"success,omitempty"`
}

// ReplayComparison tracks the differential between original and candidate provider for one fixture.
type ReplayComparison struct {
	FixtureID         string        `json:"fixture_id"`
	OriginalOutcome   string        `json:"original_outcome"`
	ChallengerOutcome string        `json:"challenger_outcome"`
	Agreed            bool          `json:"agreed"`
	OriginalCorrect   bool          `json:"original_correct"`
	ChallengerCorrect bool          `json:"challenger_correct"`
	LatencyDelta      time.Duration `json:"latency_delta"`
	CostDelta         int64         `json:"cost_delta"`
	ChallengerError   string        `json:"challenger_error,omitempty"`
}

// ReplayReport summarizes historical evaluation across all fixtures.
type ReplayReport struct {
	TotalFixtures         int                `json:"total_fixtures"`
	AgreementCount        int                `json:"agreement_count"`
	AgreementRate         float64            `json:"agreement_rate"`
	ChallengerAccuracy    float64            `json:"challenger_accuracy"`
	OriginalAccuracy      float64            `json:"original_accuracy"`
	CostSavingsMicroCents int64              `json:"cost_savings_micro_cents"`
	Comparisons           []ReplayComparison `json:"comparisons"`
}

// ReplayHarness evaluates candidate providers against historical decision fixtures.
type ReplayHarness struct{}

// NewReplayHarness creates a new ReplayHarness.
func NewReplayHarness() *ReplayHarness {
	return &ReplayHarness{}
}

// Run executes all fixtures using the challenger provider and compiles comparative metrics.
func (h *ReplayHarness) Run(ctx context.Context, fixtures []ReplayFixture, challenger DecisionProvider) (ReplayReport, error) {
	report := ReplayReport{
		TotalFixtures: len(fixtures),
		Comparisons:   make([]ReplayComparison, 0, len(fixtures)),
	}

	if len(fixtures) == 0 {
		return report, nil
	}

	var (
		agreedCount        int
		challengerCorrects int
		originalCorrects   int
		totalCostSavings   int64
	)

	for _, fix := range fixtures {
		start := time.Now()
		cRes, err := challenger.Resolve(ctx, fix.Request)
		cLatency := time.Since(start)

		comp := ReplayComparison{
			FixtureID:       fix.FixtureID,
			OriginalOutcome: fix.RecordedResult.SelectedOutcome,
		}

		if err != nil {
			comp.ChallengerError = err.Error()
			comp.ChallengerOutcome = OutcomeAbstain
		} else {
			comp.ChallengerOutcome = cRes.SelectedOutcome
			if cRes.Latency > 0 {
				cLatency = cRes.Latency
			}
		}

		comp.Agreed = comp.OriginalOutcome == comp.ChallengerOutcome
		if comp.Agreed {
			agreedCount++
		}

		if fix.ActualOutcome != "" {
			comp.OriginalCorrect = comp.OriginalOutcome == fix.ActualOutcome
			comp.ChallengerCorrect = comp.ChallengerOutcome == fix.ActualOutcome

			if comp.OriginalCorrect {
				originalCorrects++
			}
			if comp.ChallengerCorrect {
				challengerCorrects++
			}
		}

		comp.LatencyDelta = cLatency - fix.RecordedResult.Latency
		comp.CostDelta = cRes.Usage.CostMicroCents - fix.RecordedResult.Usage.CostMicroCents
		totalCostSavings += (fix.RecordedResult.Usage.CostMicroCents - cRes.Usage.CostMicroCents)

		report.Comparisons = append(report.Comparisons, comp)
	}

	report.AgreementCount = agreedCount
	report.AgreementRate = float64(agreedCount) / float64(len(fixtures))
	report.ChallengerAccuracy = float64(challengerCorrects) / float64(len(fixtures))
	report.OriginalAccuracy = float64(originalCorrects) / float64(len(fixtures))
	report.CostSavingsMicroCents = totalCostSavings

	return report, nil
}
