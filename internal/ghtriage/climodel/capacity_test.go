package climodel

import "testing"

func TestAgyPoolsSplitGeminiFromAdditional(t *testing.T) {
	var agy Profile
	for _, p := range BuiltinProfiles() {
		if p.Name == "agy" {
			agy = p
		}
	}
	for model, want := range map[string]string{
		"gemini-3.8-flash-high": "agy:gemini",
		"claude-opus-5-5":       "agy:additional",
		"gpt-oss-120b":          "agy:additional",
		"some-new-model":        "agy:default",
	} {
		if got := agy.PoolFor(model); got != want {
			t.Errorf("PoolFor(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestCapacityRefusesOverlappingPools(t *testing.T) {
	overlapping := []Capacity{
		{Pools: []Pool{{Name: "a", Models: []string{"gemini-*"}}, {Name: "b", Models: []string{"gemini-3*"}}}},
		{Pools: []Pool{{Name: "a", Models: []string{"x-*"}}, {Name: "b", Models: []string{"x-*"}}}},
	}
	for _, c := range overlapping {
		if err := c.validate(); err == nil {
			t.Errorf("validate(%+v) accepted overlapping pools", c)
		}
	}
	ok := Capacity{Pools: []Pool{{Name: "a", Models: []string{"gemini-*"}}, {Name: "b", Models: []string{"claude-*"}}}}
	if err := ok.validate(); err != nil {
		t.Errorf("validate(disjoint pools): %v", err)
	}
}

func TestCapacityRefusesBadPatternsAndEmptyNames(t *testing.T) {
	for _, c := range []Capacity{
		{Pools: []Pool{{Name: "a", Models: []string{"gemini-["}}}},
		{Pools: []Pool{{Name: "", Models: []string{"gemini-*"}}}},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("validate(%+v) accepted a bad pool", c)
		}
	}
}

func TestCommandCodePoolIsPlanUnlessFree(t *testing.T) {
	if got := commandCodePool("reflection/beam:free"); got != "commandcode:free:reflection/beam:free" {
		t.Errorf("free model pool = %q", got)
	}
	if got := commandCodePool("deepseek/deepseek-v4-pro"); got != "commandcode:plan" {
		t.Errorf("paid model pool = %q", got)
	}
}

// HTTP 429 is a limit: without it a refusal is not recorded and the pool is
// retried on every pass.
func TestTooManyRequestsIsALimit(t *testing.T) {
	for _, msg := range []string{"HTTP 429 Too Many Requests", "error: too many requests, slow down"} {
		if !providerRefused(msg) {
			t.Errorf("%q is not recognised as a limit", msg)
		}
	}
	if providerRefused("connection reset by peer") {
		t.Error("a network error is recognised as a limit")
	}
}
