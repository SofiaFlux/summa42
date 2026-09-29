package ghissue

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The keys of the triage payload are a contract with the triage executor, which
// decodes four of them and has no way to see this struct. Pinning the whole set
// here is what makes a rename a two-file change on this side of the boundary
// rather than a single one: with only the keys this package reads pinned, a
// rename done here and in a test of its own leaves the executor decoding a
// payload no real task can carry while every test in this package stays green -
// and the executor rejects every triage task, silently, at the decoder.
func TestTriageTaskPayloadKeys(t *testing.T) {
	issue, err := ParseIssue(wireIssue(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := TriageTaskPayload(issue, "ev_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sortStrings(keys)
	want := []string{
		"author", "issue", "issueSnapshot", "labels", "repo", "revision", "title", "triage", "url",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("payload keys = %q, want exactly %q", keys, want)
	}
	// The values the executor decodes, and the ones the objective is built from,
	// so a payload that carries the right keys and the wrong contents still fails
	// here rather than in the executor.
	if decoded["repo"] != "o/r" || decoded["issue"] != float64(7) ||
		decoded["revision"] != issue.RevisionID() || decoded["issueSnapshot"] != "ev_snapshot" {
		t.Fatalf("payload = %v, want the issue's own repo, number, revision and snapshot id", decoded)
	}
	// An issue with no labels carries an empty array, never null: the payload is
	// the only place the issue's own collections are marshalled.
	unlabelled, err := ParseIssue(wireIssue(func(m map[string]any) { m["labels"] = []any{} }), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	payload, err = TriageTaskPayload(unlabelled, "ev_snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(payload, []byte(`"labels":[]`)) {
		t.Fatalf("payload = %s, want an empty labels array rather than null", payload)
	}
}

// The object id is the other format this package owns and another one reads: the
// triage reviewer recovers a case's issue from it to decide whether a decision
// is about the issue the case was registered for. The writer and the reader are
// here, so a format change is a change of one thing - and the pair agreeing is
// what makes the reviewer's read correct rather than coincidental.
func TestParseObjectIDReadsWhatObjectIDWrites(t *testing.T) {
	issue := Issue{Repository: "o/r", Number: 42}
	repository, number, err := ParseObjectID(issue.ObjectID())
	if err != nil {
		t.Fatal(err)
	}
	if repository != "o/r" || number != 42 {
		t.Fatalf("ParseObjectID(%q) = %s#%d, want o/r#42", issue.ObjectID(), repository, number)
	}
	// The source qualifier is optional, because the case row carries the source in
	// its own column: a hand-registered case without one names the same issue.
	if repository, number, err = ParseObjectID("o/r#42"); err != nil {
		t.Fatal(err)
	} else if repository != "o/r" || number != 42 {
		t.Fatalf("an unqualified object id = %s#%d, want o/r#42", repository, number)
	}
	for _, object := range []string{"", "o/r", "o/r#", "o/r#0", "o/r#-1", "o/r#x", "#42", "github:"} {
		if _, _, err := ParseObjectID(object); err == nil {
			t.Fatalf("object id %q was read as an issue", object)
		}
	}
}
