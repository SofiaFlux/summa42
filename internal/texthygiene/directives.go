package texthygiene

import (
	"regexp"
	"strings"
)

// DirectiveRule is one deterministic entry in RoleDirectiveRules.
// This is a tripwire, not a proof: keeping the rules as data lets reviewers
// audit and extend the flagged patterns without mistaking pattern matching for
// a security boundary.
type DirectiveRule struct {
	ID          string
	Description string
	Pattern     *regexp.Regexp
}

// DirectiveFinding identifies text in producer output that appears to address
// the reviewer or the Owner with instructions.
type DirectiveFinding struct {
	Field   string `json:"field"`
	RuleID  string `json:"rule_id"`
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

// RoleDirectiveRules is the deterministic pattern list (kept as data) used to
// flag producer output that addresses the reviewer or Owner with instructions.
// This is a tripwire, not a proof.
var RoleDirectiveRules = []DirectiveRule{
	{
		ID:          "address-role-imperative",
		Description: "directs the reviewer, owner, maintainer, human, or operator to skip checks or force acceptance",
		Pattern:     regexp.MustCompile(`(?i)(?:^|[\s"'(/#*])(?:dear\s+)?(?:(?:code|plan|triage)\s+)?(?:reviewer|owner|maintainer|human|operator)\b\s*[:,]\s*(?:ignore|disregard|forget|skip|approve|accept|merge|override|bypass|treat|do\s+not\s+(?:check|review|flag|reject|block)|please\s+(?:accept|approve|merge))`),
	},
	{
		ID:          "note-to-role",
		Description: "carries an explicit note or instruction addressed to the reviewer or owner",
		Pattern:     regexp.MustCompile(`(?i)\b(?:note|instruction|message|notice)\s+(?:to|for)\s+(?:the\s+)?(?:(?:code|plan|triage)\s+)?(?:reviewer|owner|maintainer|operator)\b\s*[:,-]`),
	},
	{
		ID:          "ignore-instructions",
		Description: "attempts to override prior or system review instructions",
		Pattern:     regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\s+(?:all\s+|any\s+)?(?:previous|above|prior|system|reviewer|the)\s+(?:instructions?|rules?|constraints?|prompts?|diff)\b`),
	},
	{
		ID:          "force-verdict",
		Description: "instructs the reader to return an ACCEPT/APPROVE/MERGE verdict",
		Pattern:     regexp.MustCompile(`(?i)\b(?:respond|reply|answer|return|output|set\s+verdict)\s+(?:with\s+|to\s+|only\s*[: ]+)\s*["']?(?:ACCEPT|APPROVE|MERGE)["']?\b`),
	},
	{
		ID:          "must-accept",
		Description: "states that the reviewer, owner, or reader must accept or ignore findings",
		Pattern:     regexp.MustCompile(`(?i)\b(?:reviewer|owner|you)\s+(?:must|should\s+just|simply)\s+(?:accept|approve|merge|ignore|skip)\b`),
	},
}

// ScanRoleDirectives scans text line by line against RoleDirectiveRules.
// This is a tripwire, not a proof: matches are flagged as findings for the
// reviewer and as rogue containment signals, not treated as complete safety.
func ScanRoleDirectives(field, text string) []DirectiveFinding {
	if text == "" {
		return nil
	}
	var findings []DirectiveFinding
	lines := strings.Split(text, "\n")
	for idx, rawLine := range lines {
		for _, rule := range RoleDirectiveRules {
			if rule.Pattern.MatchString(rawLine) {
				findings = append(findings, DirectiveFinding{
					Field:   field,
					RuleID:  rule.ID,
					Line:    idx + 1,
					Excerpt: boundedExcerpt(rawLine, 120),
				})
			}
		}
	}
	return findings
}

func boundedExcerpt(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
