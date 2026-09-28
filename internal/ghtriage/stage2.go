package ghtriage

import (
	"sort"
	"strings"
)

const (
	titleRuneLimit   = 200
	bodyRuneLimit    = 4000
	truncationMarker = "[…truncated]"
)

// Stage2Question is the one and only question stage 2 is asked. It is an
// exported constant so a test can assert on the exact prompt the model
// receives, rather than on a string buried in a call site.
const Stage2Question = "Classify this issue. State whether it is actionable without further " +
	"information from the reporter, whether it needs a reproduction before it can be planned, " +
	"how large the change would be, and what type of work it is. Answer with JSON only."

type Stage2Input struct {
	Schema      string   `json:"schema"`
	Title       string   `json:"title"`
	Labels      []string `json:"labels"`
	Signals     []string `json:"signals"`
	BodyExcerpt string   `json:"body_excerpt"`
	Question    string   `json:"question"`
}

// BuildStage2Input prepares the only context the model ever sees. The raw
// issue is never passed through: the reproduction block that stage 1 already
// turned into a signal is removed, and both free-text fields are bounded.
func BuildStage2Input(snap Snapshot, signals []string) Stage2Input {
	labels := append([]string(nil), snap.Labels...)
	sort.Strings(labels)
	sorted := append([]string(nil), signals...)
	sort.Strings(sorted)

	body := stripLeadingFence(snap.Body)
	if runes := []rune(body); len(runes) > bodyRuneLimit {
		body = string(runes[:bodyRuneLimit]) + truncationMarker
	}

	return Stage2Input{
		Schema:      Stage2InputSchema,
		Title:       truncateRunes(snap.Title, titleRuneLimit),
		Labels:      labels,
		Signals:     sorted,
		BodyExcerpt: body,
		Question:    Stage2Question,
	}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// stripLeadingFence drops a reproduction block that opens the body, because
// stage 1 has already turned it into the has-repro signal.
func stripLeadingFence(body string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			start = i
		}
		break
	}
	if start < 0 {
		return body
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return body
}
