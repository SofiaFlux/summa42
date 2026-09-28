package ghtriage

import (
	"encoding/json"
	"strings"
	"testing"
)

func stage1Decision() Decision {
	return Decision{
		Schema:                  DecisionSchema,
		Repository:              "o/r",
		Issue:                   42,
		Revision:                "2026-09-28T10:00:00Z",
		SnapshotEvidenceID:      "evidence-1",
		TriageRulesVersion:      TriageRulesVersion,
		DispositionRulesVersion: DispositionRulesVersion,
		Stage1: Stage1Result{
			Triage: TriageDuplicate, Signals: []string{"duplicate-of:41"},
			Disposition: DispositionDuplicate, Rule: "explicit-duplicate-label",
		},
	}
}

func stage3Decision() Decision {
	d := stage1Decision()
	d.Stage1 = Stage1Result{Triage: TriageBug, Signals: []string{"has-repro"}}
	d.Stage2 = &Stage2Output{IsActionable: true, Scope: ScopeSmall, SuggestedType: SuggestedTypeBug, Rationale: "enough detail"}
	d.Stage3 = &Stage3Result{Disposition: DispositionReadyToPlan, Rule: "actionable-without-repro-small-or-medium"}
	return d
}

func TestDecisionValidate(t *testing.T) {
	resolved := stage1Decision()
	if err := resolved.Validate(); err != nil {
		t.Fatalf("a stage 1 decision was rejected: %v", err)
	}
	consulted := stage3Decision()
	if err := consulted.Validate(); err != nil {
		t.Fatalf("a stage 3 decision was rejected: %v", err)
	}
	carriesStage3 := stage1Decision()
	carriesStage3.Stage3 = &Stage3Result{Disposition: DispositionReadyToPlan, Rule: "x"}
	if err := carriesStage3.Validate(); err == nil {
		t.Fatal("a stage 1 decision carrying stage 3 was accepted")
	}
	noSnapshot := stage1Decision()
	noSnapshot.SnapshotEvidenceID = ""
	if err := noSnapshot.Validate(); err == nil {
		t.Fatal("a decision without a snapshot evidence id was accepted")
	}
	noStage2 := stage3Decision()
	noStage2.Stage2 = nil
	if err := noStage2.Validate(); err == nil {
		t.Fatal("an unresolved stage 1 without stage 2 was accepted")
	}
}

func TestDecisionFinalDisposition(t *testing.T) {
	if got := stage1Decision().FinalDisposition(); got != DispositionDuplicate {
		t.Fatalf("stage 1 final disposition = %q", got)
	}
	if got := stage3Decision().FinalDisposition(); got != DispositionReadyToPlan {
		t.Fatalf("stage 3 final disposition = %q", got)
	}
}

func TestDecisionCanonicalIsStableAndOmitsAbsentStages(t *testing.T) {
	first, err := stage1Decision().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	second, err := stage1Decision().Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("canonical bytes are not stable across calls")
	}
	if strings.Contains(string(first), "stage2") || strings.Contains(string(first), "stage3") {
		t.Fatalf("a stage 1 decision leaked later stages: %s", first)
	}
	var round Decision
	if err := json.Unmarshal(first, &round); err != nil {
		t.Fatal(err)
	}
	if round.Stage1.Rule != "explicit-duplicate-label" {
		t.Fatalf("the round trip lost the stage 1 rule: %+v", round.Stage1)
	}
}
