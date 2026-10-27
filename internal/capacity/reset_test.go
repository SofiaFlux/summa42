package capacity

import (
	"testing"
	"time"
)

func TestParseResetTable(t *testing.T) {
	observed := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		message string
		want    time.Time
		ok      bool
	}{
		{"days and hours", "You've reached your weekly usage limit. Resets in 2d 1h (Thu 10:23 AM).", observed.Add(49 * time.Hour), true},
		{"hours and minutes", "RESOURCE_EXHAUSTED ... resets in 23h56m", observed.Add(23*time.Hour + 56*time.Minute), true},
		{"no reset time", "rate limit exceeded", time.Time{}, false},
		{"localized text is unknown, never wrong", "zresetuje się za 2 dni", time.Time{}, false},
		{"absurd value is capped at eight days", "resets in 999d", observed.Add(8 * 24 * time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseReset(tt.message, observed)
			if ok != tt.ok {
				t.Fatalf("ParseReset ok = %v, want %v", ok, tt.ok)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("ParseReset = %v, want %v", got, tt.want)
			}
		})
	}
}
