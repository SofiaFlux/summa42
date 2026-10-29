package climodel

import "testing"

// A model that writes a sentence before its JSON answer still gives its answer;
// anything that is not exactly one JSON object is left for the strict parser.
func TestProseBeforeTheJSONObjectIsUnwrapped(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"prose first": {"I reviewed the plan.\n{\"verdict\":\"ACCEPT\",\"reason\":\"ok\"}", `{"verdict":"ACCEPT","reason":"ok"}`},
		"pure JSON":   {`{"verdict":"REVISE","reason":"x"}`, `{"verdict":"REVISE","reason":"x"}`},
		"two objects": {`{"a":1} and {"b":2}`, `{"a":1} and {"b":2}`},
		"no object":   {"plain text", "plain text"},
		"broken JSON": {"note {\"verdict\": ", "note {\"verdict\": "},
	}
	for name, c := range cases {
		if got := string(extractJSONObject([]byte(c.in))); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}
