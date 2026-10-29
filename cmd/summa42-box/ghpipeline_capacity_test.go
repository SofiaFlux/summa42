package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
)

// fakeLedger holds pools that are exhausted until a fixed time and records
// every call it is given.
type fakeLedger struct {
	exhausted map[string]time.Time
	degraded  map[string]time.Time
	calls     []capacity.Call
}

func (l *fakeLedger) RecordCall(_ context.Context, c capacity.Call) error {
	l.calls = append(l.calls, c)
	return nil
}

func (l *fakeLedger) PoolState(_ context.Context, pool string, now time.Time) (capacity.PoolState, error) {
	until, ok := l.exhausted[pool]
	if ok && until.After(now) {
		return capacity.PoolState{Pool: pool, Exhausted: true, ResetAt: until}, nil
	}
	until, ok = l.degraded[pool]
	return capacity.PoolState{Pool: pool, Degraded: ok && until.After(now), ResetAt: until}, nil
}

func TestPipelineHoldsAStageWhoseReviewerPoolIsExhausted(t *testing.T) {
	p, stderr := parsedPipeline(t, validPipelineArgs(t))
	f := &fakePipeline{}
	f.install(p)
	until := time.Now().Add(49 * time.Hour)
	ledger := &fakeLedger{exhausted: map[string]time.Time{"commandcode:plan": until}}
	p.ledger = ledger
	p.gates = map[string][]gate{
		"plan":        {{[]string{"agy:gemini"}, "plan"}, {[]string{"commandcode:plan"}, "plan-review"}},
		"plan-review": {{[]string{"commandcode:plan"}, "plan-review"}},
	}
	if err := p.pass(context.Background()); err != nil {
		t.Fatalf("a held stage failed the pass: %v", err)
	}
	if slices.Contains(f.calls, "plan") || slices.Contains(f.calls, "plan-review") {
		t.Fatalf("held stages ran: %v", f.calls)
	}
	out := stderr.String()
	if !strings.Contains(out, "stage=plan case=- skipped=capacity: pool commandcode:plan exhausted until "+until.UTC().Format(time.RFC3339)+" (needed by plan-review)") {
		t.Fatalf("stderr = %q, want the plan hold with its reason", out)
	}
	if !strings.Contains(out, "capacity: pool=commandcode:plan exhausted until=") {
		t.Fatalf("stderr = %q, want the pool transition", out)
	}
	if len(ledger.calls) == 0 || ledger.calls[0].Outcome != capacity.OutcomeGated || ledger.calls[0].Pool != "commandcode:plan" {
		t.Fatalf("ledger = %+v, want a gated row for the held stage", ledger.calls)
	}
}

func TestPipelineHoldsPlanningButNotReviewWhenOnlyTheProducerIsExhausted(t *testing.T) {
	p, _ := parsedPipeline(t, validPipelineArgs(t))
	f := &fakePipeline{}
	f.install(p)
	p.ledger = &fakeLedger{exhausted: map[string]time.Time{"agy:gemini": time.Now().Add(time.Hour)}}
	p.gates = map[string][]gate{
		"plan":        {{[]string{"agy:gemini"}, "plan"}, {[]string{"commandcode:plan"}, "plan-review"}},
		"plan-review": {{[]string{"commandcode:plan"}, "plan-review"}},
	}
	if err := p.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.calls, "plan") {
		t.Fatalf("planning ran with its producer pool exhausted: %v", f.calls)
	}
	if !slices.Contains(f.calls, "plan-review") {
		t.Fatalf("review was held although its pool is free: %v", f.calls)
	}
}

func TestReviewStageRoutesToAnApprovedReplacementWhenThePrimaryIsExhausted(t *testing.T) {
	p, _ := parsedPipeline(t, validPipelineArgs(t))
	f := &fakePipeline{}
	f.install(p)
	p.ledger = &fakeLedger{exhausted: map[string]time.Time{"commandcode:plan": time.Now().Add(time.Hour)}}
	p.reviewerPool = "commandcode:plan"
	p.fallbacks = []climodel.Config{{Binary: "/bin/fallback", RunAsUser: "summa42-model", Timeout: time.Minute,
		CommandCodeModel: "reflection/beam:free", FallbackOf: "commandcode:plan"}}
	p.gates = map[string][]gate{
		"plan-review": {{[]string{"commandcode:plan", "commandcode:free:reflection/beam:free"}, "plan-review"}},
	}
	if err := p.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	reviews := f.argv["plan-review"]
	if len(reviews) != 1 {
		t.Fatalf("plan review calls = %d, want 1", len(reviews))
	}
	argv := reviews[0]
	if !slices.Contains(argv, "--model-binary=/bin/fallback") || !slices.Contains(argv, "--model-fallback-of=commandcode:plan") {
		t.Fatalf("argv %v does not name the replacement reviewer", argv)
	}
	if slices.Contains(argv, "--model-binary=/bin/reviewer") {
		t.Fatalf("argv %v still names the exhausted primary reviewer", argv)
	}
}

func TestWorkerGateIsTheProducerPoolOnly(t *testing.T) {
	p, _ := parsedPipeline(t, validPipelineArgs(t))
	if got := p.gates["worker"]; len(got) != 1 {
		t.Fatalf("worker gate = %+v, want the producer pool only", got)
	}
}

func TestReviewStageRoutesAwayFromADegradedPrimary(t *testing.T) {
	p, _ := parsedPipeline(t, validPipelineArgs(t))
	f := &fakePipeline{}
	f.install(p)
	p.ledger = &fakeLedger{degraded: map[string]time.Time{"opencode:default": time.Now().Add(time.Hour)}}
	p.reviewerPool = "opencode:default"
	p.fallbacks = []climodel.Config{{Binary: "/bin/fallback", RunAsUser: "summa42-model", Timeout: time.Minute,
		CommandCodeModel: "reflection/beam:free", FallbackOf: "opencode:default"}}
	p.gates = map[string][]gate{
		"plan-review": {{[]string{"opencode:default", "commandcode:free:reflection/beam:free"}, "plan-review"}},
	}
	if err := p.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	reviews := f.argv["plan-review"]
	if len(reviews) != 1 || !slices.Contains(reviews[0], "--model-binary=/bin/fallback") {
		t.Fatalf("plan review did not route to the healthy replacement: %v", reviews)
	}
}
