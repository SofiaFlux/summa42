package ghtriage

import (
	"strings"
	"testing"
)

func TestStage2QuestionIsTheFixedLiteral(t *testing.T) {
	input := BuildStage2Input(snapshot(nil, "Crash", "boom", TriageBug), nil)
	if input.Question != Stage2Question {
		t.Fatalf("question = %q, want the fixed literal", input.Question)
	}
	if !strings.Contains(input.Question, "Answer with JSON only") {
		t.Fatalf("the question does not constrain the response format: %q", input.Question)
	}
}

func TestBuildStage2InputNeverCarriesTheReproductionBlock(t *testing.T) {
	body := "```\npanic: nil\n```\nand then it dies"
	input := BuildStage2Input(snapshot([]string{"bug"}, "Crash on save", body, TriageBug), []string{"has-repro", "label:bug"})

	if strings.Contains(input.BodyExcerpt, "panic: nil") {
		t.Fatalf("the reproduction block reached the model: %q", input.BodyExcerpt)
	}
	if !strings.Contains(input.BodyExcerpt, "and then it dies") {
		t.Fatalf("prose was lost with the reproduction block: %q", input.BodyExcerpt)
	}
	if input.Schema != Stage2InputSchema {
		t.Fatalf("schema = %q", input.Schema)
	}
}

func TestBuildStage2InputTruncatesTitleAndBody(t *testing.T) {
	input := BuildStage2Input(
		snapshot(nil, strings.Repeat("a", 500), strings.Repeat("b", 9000), TriageBug), nil)

	if got := len([]rune(input.Title)); got != 200 {
		t.Fatalf("title runes = %d, want 200", got)
	}
	if !strings.HasSuffix(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a truncated body lacks the marker: %q", input.BodyExcerpt)
	}
	if got := len([]rune(strings.TrimSuffix(input.BodyExcerpt, truncationMarker))); got != 4000 {
		t.Fatalf("body excerpt runes = %d, want 4000", got)
	}
}

func TestBuildStage2InputUntruncatedBodyHasNoMarker(t *testing.T) {
	input := BuildStage2Input(snapshot(nil, "Crash", "short body", TriageBug), nil)
	if strings.Contains(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a short body was marked truncated: %q", input.BodyExcerpt)
	}
}

func TestBuildStage2InputSortsLabelsAndSignals(t *testing.T) {
	input := BuildStage2Input(
		snapshot([]string{"p1", "bug", "enhancement"}, "t", "b", TriageBug),
		[]string{"title:[bug]", "has-repro"})

	if input.Labels[0] != "bug" {
		t.Fatalf("labels are not sorted: %v", input.Labels)
	}
	if input.Signals[0] != "has-repro" {
		t.Fatalf("signals are not sorted: %v", input.Signals)
	}
}

// A fenced block is only stripped when it opens the body. One embedded in
// prose is the reporter's own words, and dropping it would silently destroy
// the description stage 2 is supposed to read.
func TestBuildStage2InputKeepsAFencedBlockThatDoesNotOpenTheBody(t *testing.T) {
	body := "it happens right after this paragraph\n```\npanic: nil\n```\nand then it dies"
	input := BuildStage2Input(snapshot([]string{"bug"}, "Crash on save", body, TriageBug), nil)

	if !strings.Contains(input.BodyExcerpt, "panic: nil") {
		t.Fatalf("a fenced block inside prose was removed: %q", input.BodyExcerpt)
	}
	if !strings.Contains(input.BodyExcerpt, "it happens right after this paragraph") {
		t.Fatalf("prose was lost: %q", input.BodyExcerpt)
	}
}

func TestBuildStage2InputStripLeadingFenceBoundaries(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"an empty body stays empty", "", ""},
		{"leading blank lines still make the fence leading", "\n\n```\npanic: nil\n```\ntail", "tail"},
		{"an unclosed fence keeps the body", "```\npanic: nil", "```\npanic: nil"},
		{"a closed fence that is the whole body leaves nothing", "```\n```", ""},
		{"a body with no fence is untouched", "just prose", "just prose"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildStage2Input(snapshot(nil, "t", tc.body, TriageBug), nil).BodyExcerpt
			if got != tc.want {
				t.Fatalf("body excerpt = %q, want %q", got, tc.want)
			}
		})
	}
}
