package texthygiene

import (
	"regexp"
	"strconv"
	"strings"
)

// MaxDirectiveFindings bounds how many role-directive findings one scan returns
// so noisy or adversarial outputs cannot bloat reviewer prompts or containment events.
const MaxDirectiveFindings = 8

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
		Pattern:     regexp.MustCompile(`(?i)(?:^|[\s"'(/#*@])(?:dear\s+)?(?:(?:code|plan|triage)\s+)?(?:reviewer|owner|maintainer|human|operator)\b\s*[:,;-]\s*(?:please\s+)?(?:ignore|disregard|forget|skip|approve|accept|merge|override|bypass|treat|do\s+not|don't)`),
	},
	{
		ID:          "note-to-role",
		Description: "carries an explicit note or instruction addressed to the reviewer, human, or owner",
		Pattern:     regexp.MustCompile(`(?i)\b(?:note|instruction|message|notice)\s+(?:to|for)\s+(?:the\s+)?(?:(?:code|plan|triage)\s+)?(?:reviewer|owner|maintainer|human|operator)\b\s*[:,;-]?`),
	},
	{
		ID:          "ignore-instructions",
		Description: "attempts to override prior or system review instructions",
		Pattern:     regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\s+(?:all\s+|any\s+)?(?:previous|above|prior|system|reviewer|your|these|earlier|the)\s+(?:instructions?|rules?|constraints?|prompts?|diff|tests?)\b`),
	},
	{
		ID:          "force-verdict",
		Description: "instructs the reader to return an ACCEPT/APPROVE/MERGE verdict",
		Pattern:     regexp.MustCompile(`(?i)(?:\b(?:respond|reply|answer|return|output|give|choose)\b[^.\n]{0,30}(?:\b(?:with|to|only)\b|:)\s*["'{:]?\s*(?:["']?verdict["']?\s*[:=]\s*["']?)?(?:ACCEPT|APPROVE|MERGE)\b|\bset\s+(?:the\s+)?verdict\s*(?:to|=|:)\s*["']?(?:ACCEPT|APPROVE|MERGE)\b)`),
	},
	{
		ID:          "direct-command-to-accept",
		Description: "commands the reviewer or reader to accept, approve, merge, ignore, or skip",
		Pattern:     regexp.MustCompile(`(?i)\b(?:you\s+(?:must|should|have\s+to)|(?:plan\s+|code\s+|triage\s+)?reviewer\s+(?:must|should(?:\s+just)?|simply)|owner\s+(?:must|should)\s+(?:ignore|skip))\s+(?:(?:just|simply|immediately)\s+)?(?:accept|approve|merge|ignore|skip)\b`),
	},
}

// ScanRoleDirectives scans text (including multi-line spans) against RoleDirectiveRules.
// This is a tripwire, not a proof: matches are capped at MaxDirectiveFindings and
// surfaced as findings for the reviewer and as rogue containment signals.
func ScanRoleDirectives(field, text string) []DirectiveFinding {
	if text == "" {
		return nil
	}
	var findings []DirectiveFinding
	seen := make(map[string]bool)
	for _, rule := range RoleDirectiveRules {
		for _, loc := range rule.Pattern.FindAllStringIndex(text, MaxDirectiveFindings) {
			line := 1 + strings.Count(text[:loc[0]], "\n")
			key := fmtKey(line, rule.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			findings = append(findings, DirectiveFinding{
				Field:   field,
				RuleID:  rule.ID,
				Line:    line,
				Excerpt: boundedExcerpt(text[loc[0]:loc[1]], 120),
			})
			if len(findings) >= MaxDirectiveFindings {
				return findings
			}
		}
	}
	return findings
}

func fmtKey(line int, ruleID string) string {
	return ruleID + ":" + strconv.Itoa(line)
}

func boundedExcerpt(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}
