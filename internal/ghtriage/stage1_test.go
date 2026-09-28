package ghtriage

import "testing"

func snapshot(labels []string, title, body string, triage Triage) Snapshot {
	return Snapshot{
		Repo: "o/r", Issue: 42, Title: title, Body: body,
		Author: "maintainer", Labels: labels, Triage: string(triage),
		UpdatedAt: "2026-09-28T10:00:00Z",
	}
}

func TestStage1ResolvesOnlyAValidDuplicateLabel(t *testing.T) {
	cases := []struct {
		name       string
		snap       Snapshot
		wantTriage Triage
		wantDisp   Disposition
		wantRule   string
	}{
		{"single valid duplicate label resolves",
			snapshot([]string{"duplicate-of:41", "bug"}, "Crash on save", "boom", TriageBug),
			TriageDuplicate, DispositionDuplicate, "explicit-duplicate-label"},
		{"bare hash reference is not proof",
			snapshot([]string{"bug"}, "Crash on save", "same as #41 happened", TriageBug),
			TriageBug, "", ""},
		{"self reference is not a duplicate",
			snapshot([]string{"duplicate-of:42"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
		{"conflicting duplicate labels are unresolved",
			snapshot([]string{"duplicate-of:41", "duplicate-of:43"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
		{"malformed duplicate label is unresolved",
			snapshot([]string{"duplicate-of:abc"}, "Crash on save", "boom", TriageBug),
			TriageBug, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Stage1(tc.snap)
			if got.Triage != tc.wantTriage {
				t.Fatalf("triage = %q, want %q", got.Triage, tc.wantTriage)
			}
			if got.Disposition != tc.wantDisp {
				t.Fatalf("disposition = %q, want %q", got.Disposition, tc.wantDisp)
			}
			if got.Rule != tc.wantRule {
				t.Fatalf("rule = %q, want %q", got.Rule, tc.wantRule)
			}
		})
	}
}

func TestStage1WidensOnlyUnclassifiedToQuestion(t *testing.T) {
	labelled := Stage1(snapshot([]string{"bug", "question"}, "Crash", "boom", TriageBug))
	if labelled.Triage != TriageBug {
		t.Fatalf("a labelled bug widened to %q", labelled.Triage)
	}
	prefix := Stage1(snapshot(nil, "[question] Why is this slow", "boom", TriageUnclassified))
	if prefix.Triage != TriageQuestion {
		t.Fatalf("unclassified with a [question] prefix = %q, want question", prefix.Triage)
	}
}

func TestStage1ExtractsReproSignal(t *testing.T) {
	fenced := Stage1(snapshot([]string{"bug"}, "Crash", "steps\n```\npanic\n```\n", TriageBug))
	if !containsString(fenced.Signals, "has-repro") {
		t.Fatalf("signals %v lack has-repro", fenced.Signals)
	}
	plain := Stage1(snapshot([]string{"bug"}, "Crash", "it just crashes", TriageBug))
	if containsString(plain.Signals, "has-repro") {
		t.Fatalf("signals %v claim has-repro", plain.Signals)
	}
}

func TestStage1SignalsAreSorted(t *testing.T) {
	got := Stage1(snapshot([]string{"bug", "enhancement", "p1"}, "[bug] Crash", "x", TriageBug))
	for i := 1; i < len(got.Signals); i++ {
		if got.Signals[i-1] > got.Signals[i] {
			t.Fatalf("signals are not sorted: %v", got.Signals)
		}
	}
}

func containsString(haystack []string, needle string) bool {
	for _, value := range haystack {
		if value == needle {
			return true
		}
	}
	return false
}
