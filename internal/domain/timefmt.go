package domain

import "time"

// TimeLayout is the fixed-width canonical timestamp layout. RFC3339Nano trims
// trailing fractional zeros, so its output does not sort lexically.
const TimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTime renders t in UTC with the fixed-width canonical layout. Parsing
// with time.RFC3339Nano still accepts it, and legacy mixed-width rows.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }
