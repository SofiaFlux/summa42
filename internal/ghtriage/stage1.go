package ghtriage

import (
	"sort"
	"strconv"
	"strings"
)

const (
	duplicateLabelPrefix = "duplicate-of:"
	questionLabel        = "question"
)

// Stage1 is a pure function from the canonical snapshot to signals, a
// classification, and — for exactly one decisive case — a disposition. It is
// deliberately conservative: the only decisive rule requires an explicit,
// well-formed duplicate label, because a bare #N in the body is context and
// the snapshot carries no other issue's state to verify one against.
func Stage1(snap Snapshot) Stage1Result {
	signals := make([]string, 0, len(snap.Labels)+3)
	for _, label := range snap.Labels {
		signals = append(signals, "label:"+label)
	}
	if prefix := titlePrefix(snap.Title); prefix != "" {
		signals = append(signals, "title:["+prefix+"]")
	}
	if hasFencedBlock(snap.Body) {
		signals = append(signals, "has-repro")
	}
	target, duplicate := validDuplicateTarget(snap)
	if duplicate {
		signals = append(signals, duplicateLabelPrefix+strconv.FormatInt(target, 10))
	}
	sort.Strings(signals)

	triage := Triage(snap.Triage)
	switch {
	case duplicate:
		triage = TriageDuplicate
	case triage == TriageUnclassified && questionSignalled(snap):
		triage = TriageQuestion
	}

	result := Stage1Result{Triage: triage, Signals: signals}
	if duplicate {
		result.Disposition = DispositionDuplicate
		result.Rule = "explicit-duplicate-label"
	}
	return result
}

func titlePrefix(title string) string {
	trimmed := strings.TrimSpace(title)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	end := strings.Index(trimmed, "]")
	if end < 2 {
		return ""
	}
	return strings.ToLower(trimmed[1:end])
}

func questionSignalled(snap Snapshot) bool {
	if titlePrefix(snap.Title) == "question" {
		return true
	}
	for _, label := range snap.Labels {
		if strings.EqualFold(strings.TrimSpace(label), questionLabel) {
			return true
		}
	}
	return false
}

// validDuplicateTarget accepts exactly one well-formed duplicate-of label
// naming a positive issue other than this one. Zero, several, or a malformed
// label all leave the issue unresolved.
func validDuplicateTarget(snap Snapshot) (int64, bool) {
	target := int64(0)
	found := 0
	for _, label := range snap.Labels {
		raw, ok := strings.CutPrefix(strings.TrimSpace(label), duplicateLabelPrefix)
		if !ok {
			continue
		}
		number, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || number <= 0 || number == snap.Issue {
			return 0, false
		}
		target = number
		found++
	}
	if found != 1 {
		return 0, false
	}
	return target, true
}

func hasFencedBlock(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return true
		}
	}
	return false
}
