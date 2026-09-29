package ghtriage

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// The question is the whole prompt, so it is pinned here as text rather than
// by comparing the input against the constant it is built from, which can
// never differ. A shorter question, or one that loses its closing full stop,
// fails this test.
func TestStage2QuestionIsTheFixedLiteral(t *testing.T) {
	const want = `Classify this issue. State whether it is actionable without further information from the reporter, whether it needs a reproduction before it can be planned, how large the change would be, and what type of work it is. Answer with JSON only.`

	input := BuildStage2Input(snapshot(nil, "Crash", "boom", TriageBug), nil)
	if input.Question != want {
		t.Fatalf("question = %q, want %q", input.Question, want)
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

// The limits are counted in runes, not bytes. "→" is three bytes per rune, so
// a byte-based truncation both reports the wrong rune count and cuts a rune in
// half, leaving invalid UTF-8 in the only context the model ever sees.
func TestBuildStage2InputTruncatesMultiByteTextOnRuneBoundaries(t *testing.T) {
	input := BuildStage2Input(
		snapshot(nil, strings.Repeat("→", 500), strings.Repeat("→", 9000), TriageBug), nil)

	if !utf8.ValidString(input.Title) {
		t.Fatalf("truncating the title produced invalid UTF-8: %q", input.Title)
	}
	if got := len([]rune(input.Title)); got != 200 {
		t.Fatalf("title runes = %d, want 200", got)
	}
	if !strings.HasSuffix(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a truncated body lacks the marker: %q", input.BodyExcerpt)
	}
	if !utf8.ValidString(input.BodyExcerpt) {
		t.Fatalf("truncating the body produced invalid UTF-8: %q", input.BodyExcerpt)
	}
	if got := len([]rune(strings.TrimSuffix(input.BodyExcerpt, truncationMarker))); got != 4000 {
		t.Fatalf("body excerpt runes = %d, want 4000", got)
	}
}

// A body of exactly the limit is not truncated, so the bound is ">" and not
// ">=". Multi-byte so that 4000 runes is not also 4000 bytes.
func TestBuildStage2InputBodyAtExactlyTheLimitIsNotTruncated(t *testing.T) {
	body := strings.Repeat("→", 4000)
	input := BuildStage2Input(snapshot(nil, "Crash", body, TriageBug), nil)

	if strings.Contains(input.BodyExcerpt, truncationMarker) {
		t.Fatalf("a body of exactly 4000 runes was marked truncated: %q", input.BodyExcerpt)
	}
	if input.BodyExcerpt != body {
		t.Fatalf("a body of exactly 4000 runes was altered: %d runes, want %d, got %q",
			len([]rune(input.BodyExcerpt)), len([]rune(body)), input.BodyExcerpt)
	}
}

// Stage 1 and the decision document read the same snapshot, so sorting must not
// reach back into the caller's slices. The sortedness of the returned slices
// is covered by TestBuildStage2InputSortsLabelsAndSignals; what is unique here
// is that the caller's own slices come back untouched.
func TestBuildStage2InputDoesNotReorderTheCallersSlices(t *testing.T) {
	labels := []string{"p1", "bug", "enhancement"}
	signals := []string{"title:[bug]", "has-repro"}
	snap := snapshot(labels, "t", "b", TriageBug)

	BuildStage2Input(snap, signals)

	if want := []string{"p1", "bug", "enhancement"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("the caller's labels were reordered in place: %v, want %v", labels, want)
	}
	if want := []string{"title:[bug]", "has-repro"}; !reflect.DeepEqual(signals, want) {
		t.Fatalf("the caller's signals were reordered in place: %v, want %v", signals, want)
	}
}

// The document the model reads says labels and signals are arrays, and a real
// unlabelled issue with a plain title has neither. append(nil, empty...) yields a
// nil slice, which json.Marshal writes as null, so the model was handed
// "labels":null on the very issues with the least to say - and this is the only
// document it ever sees. The accepting direction matters as much as the empty
// one: a preparation that emptied the fields for every issue would pass a test
// that only checked they were not null.
func TestBuildStage2InputRendersEmptyCollectionsAsArrays(t *testing.T) {
	snap := snapshot(nil, "Crash on save", "it crashes", TriageUnclassified)
	input := BuildStage2Input(snap, nil)
	// The signals stage 2 is given are the ones stage 1 produced for this very
	// snapshot, so the empty case is a real one rather than a hand-built nil.
	if got := Stage1(snap).Signals; len(got) != 0 {
		t.Fatalf("fixture signals = %q, want an issue that signals nothing", got)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"labels", "signals"} {
		if decoded[key] == nil {
			t.Fatalf("%s = null in %s, want an empty array", key, raw)
		}
		if values, ok := decoded[key].([]any); !ok || len(values) != 0 {
			t.Fatalf("%s = %v in %s, want an empty array", key, decoded[key], raw)
		}
	}
	// And a populated issue is still populated: the fix is the empty case.
	populated := BuildStage2Input(
		snapshot([]string{"bug"}, "Crash on save", "it crashes", TriageBug), []string{"label:bug"})
	raw, err = json.Marshal(populated)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"labels":["bug"]`) ||
		!strings.Contains(string(raw), `"signals":["label:bug"]`) {
		t.Fatalf("prompt = %s, want the issue's own labels and signals", raw)
	}
}

// What is stored is not what is sent, and the difference is deliberate: the
// decision records stage 1's own signal list, which is built non-nil whether or
// not there was anything to record. So the change above cannot have moved
// stage1.signals from [] to null, and this says so on the stored document rather
// than leaving it to be inferred.
func TestStage1SignalsAreStoredAsAnArrayNotNull(t *testing.T) {
	raw, err := json.Marshal(Stage1(snapshot(nil, "Crash on save", "it crashes", TriageUnclassified)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"signals":[]`) {
		t.Fatalf("stage 1 = %s, want an empty signal array rather than null", raw)
	}
}

func TestBuildStage2InputSortsLabelsAndSignals(t *testing.T) {
	input := BuildStage2Input(
		snapshot([]string{"p1", "bug", "enhancement"}, "t", "b", TriageBug),
		[]string{"title:[bug]", "has-repro"})

	if want := []string{"bug", "enhancement", "p1"}; !reflect.DeepEqual(input.Labels, want) {
		t.Fatalf("labels = %v, want %v", input.Labels, want)
	}
	if want := []string{"has-repro", "title:[bug]"}; !reflect.DeepEqual(input.Signals, want) {
		t.Fatalf("signals = %v, want %v", input.Signals, want)
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
