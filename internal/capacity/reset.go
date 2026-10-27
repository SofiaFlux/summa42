// Package capacity records what a model harness can do right now: which limits
// exist, when they reset, and what calls have used up.
package capacity

import (
	"regexp"
	"strconv"
	"time"
)

// maxReset caps a parsed reset so a garbled message cannot block a pool for months.
const maxReset = 8 * 24 * time.Hour

// resetPattern matches only the relative English form the CLIs print,
// e.g. "resets in 2d 1h" or "resets in 23h56m". Localized or absolute times do not match.
var resetPattern = regexp.MustCompile(`(?i)resets? in (?:(\d+)d\s*)?(?:(\d+)h\s*)?(?:(\d+)m)?`)

// ParseReset returns the time a limit resets, counted from observed. It returns
// false when the message has no relative reset time it can read.
func ParseReset(message string, observed time.Time) (time.Time, bool) {
	m := resetPattern.FindStringSubmatch(message)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "") {
		return time.Time{}, false
	}
	d := component(m[1], 24*time.Hour) + component(m[2], time.Hour) + component(m[3], time.Minute)
	if d > maxReset {
		d = maxReset
	}
	return observed.Add(d), true
}

func component(digits string, unit time.Duration) time.Duration {
	if digits == "" {
		return 0
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return time.Duration(n) * unit
}
