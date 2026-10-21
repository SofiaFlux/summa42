package domain

import (
	"sort"
	"testing"
	"time"
)

func TestFormatTimeSortsLexically(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Nanosecond), base.Add(100 * time.Millisecond), base.Add(time.Second), base.Add(1500 * time.Millisecond)}
	var got []string
	for _, tm := range times {
		got = append(got, FormatTime(tm))
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not lexically sorted: %v", got)
	}
	if len(got[0]) != len(got[1]) {
		t.Fatalf("not fixed width: %v", got)
	}
	parsed, err := time.Parse(time.RFC3339Nano, got[2])
	if err != nil || !parsed.Equal(times[2]) {
		t.Fatalf("round trip failed: %v %v", parsed, err)
	}
}
