package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/SofiaFlux/summa42/internal/runmanifest"
)

const (
	FixtureVersion   = 1
	SanitizerVersion = "replay-scrubber-v1"
)

// TaskSemantics captures the functional intent of the task stripped of secrets.
type TaskSemantics struct {
	Objective            string   `json:"objective"`
	TaskClass            string   `json:"task_class"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	AuthorityCeiling     []string `json:"authority_ceiling,omitempty"`
	AcceptanceCriteria   []string `json:"acceptance_criteria,omitempty"`
	TaskPayloadHash      string   `json:"task_payload_hash,omitempty"`
}

// ExecutionMetadata captures reproducible runtime environment coordinates.
type ExecutionMetadata struct {
	ExecutorKind          string `json:"executor_kind"`
	ExecutorModel         string `json:"executor_model,omitempty"`
	TEBProfile            string `json:"teb_profile,omitempty"`
	ContextProjectionHash string `json:"context_projection_hash,omitempty"`
	PolicyHash            string `json:"policy_hash,omitempty"`
}

// ExpectedInvariants defines structural properties that must be satisfied during replay.
type ExpectedInvariants struct {
	RequireAuthorityCeilingHeld bool `json:"require_authority_ceiling_held"`
	RequireZeroAmbientNetwork   bool `json:"require_zero_ambient_network"`
	RequireAuditTamperEvident   bool `json:"require_audit_tamper_evident"`
	RequireNoStaleAttemptAction bool `json:"require_no_stale_attempt_action"`
}

// CostEnvelope declares deterministic consumption and risk limits.
type CostEnvelope struct {
	MaxTokens   int64 `json:"max_tokens"`
	MaxExposure int64 `json:"max_exposure"`
}

// SanitizationProvenance records deterministic details about how the fixture was scrubbed.
type SanitizationProvenance struct {
	SanitizerVersion string   `json:"sanitizer_version"`
	RulesetHash      string   `json:"ruleset_hash"`
	ScrubbedAt       string   `json:"scrubbed_at,omitempty"`
	PassedChecks     []string `json:"passed_checks"`
}

// Fixture represents a versioned, sanitized replay test case.
type Fixture struct {
	Version      int                    `json:"version"`
	FixtureID    string                 `json:"fixture_id"`
	Task         TaskSemantics          `json:"task"`
	Execution    ExecutionMetadata      `json:"execution"`
	Invariants   ExpectedInvariants     `json:"invariants"`
	CostEnvelope CostEnvelope           `json:"cost_envelope"`
	Sanitization SanitizationProvenance `json:"sanitization"`
}

func (f *Fixture) Validate() error {
	if f == nil {
		return errors.New("fixture cannot be nil")
	}
	if f.Version != FixtureVersion {
		return fmt.Errorf("unsupported fixture version %d, expected %d", f.Version, FixtureVersion)
	}
	if strings.TrimSpace(f.FixtureID) == "" {
		return errors.New("fixture_id is required")
	}
	if strings.TrimSpace(f.Task.Objective) == "" {
		return errors.New("task objective is required")
	}
	if strings.TrimSpace(f.Task.TaskClass) == "" {
		return errors.New("task_class is required")
	}
	if strings.TrimSpace(f.Execution.ExecutorKind) == "" {
		return errors.New("executor_kind is required")
	}
	if f.CostEnvelope.MaxTokens <= 0 || f.CostEnvelope.MaxExposure < 0 {
		return errors.New("invalid cost envelope limits: max_tokens must be positive and max_exposure non-negative")
	}
	if !f.Invariants.RequireAuthorityCeilingHeld && !f.Invariants.RequireZeroAmbientNetwork && !f.Invariants.RequireAuditTamperEvident && !f.Invariants.RequireNoStaleAttemptAction {
		return errors.New("fixture must declare at least one expected invariant")
	}
	return nil
}

type scanRule struct {
	Code    string
	Pattern *regexp.Regexp
}

func defaultScanRules() []scanRule {
	return []scanRule{
		{Code: "PRIVATE_KEY", Pattern: regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 ]+PRIVATE KEY-----`)},
		{Code: "API_SECRET_TOKEN", Pattern: regexp.MustCompile(`(?i)(?:sk_live_|sk_test_|ghp_|gho_|xoxb-|xoxp-|AKIA)[a-zA-Z0-9_\-]{16,}`)},
		{Code: "AUTH_HEADER_OR_PARAM", Pattern: regexp.MustCompile(`(?i)(?:bearer_token|auth_sec|api_key|secret_key|access_token)\s*[:=]\s*[^\s,;]+`)},
		{Code: "UNIX_PATH", Pattern: regexp.MustCompile(`/(?:home|Users|private|var|etc|opt|srv|mnt|workspace|workspaces|repo|repos)/[^\s]+`)},
		{Code: "EMAIL", Pattern: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
		{Code: "URL", Pattern: regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)},
	}
}

type Scrubber struct {
	rules       []scanRule
	rulesetHash string
}

func NewScrubber() *Scrubber {
	rules := defaultScanRules()
	h := sha256.New()
	for _, r := range rules {
		h.Write([]byte(r.Code + "=" + r.Pattern.String() + "\n"))
	}
	return &Scrubber{
		rules:       rules,
		rulesetHash: hex.EncodeToString(h.Sum(nil))[:16],
	}
}

// sanitizeString applies redaction regexes to replace sensitive substrings.
func (s *Scrubber) sanitizeString(input string) string {
	res := input
	// Deny replacement rules for well-known secret patterns
	redactRules := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:sk_live_|sk_test_|ghp_|gho_|xoxb-|xoxp-)[a-zA-Z0-9_\-]{16,}`),
		regexp.MustCompile(`(?i)(?:bearer_token|auth_sec|api_key|secret_key|access_token)=([a-zA-Z0-9_\-]+)`),
		regexp.MustCompile(`(?i)(?:auth_sec_|key_sec_)[a-zA-Z0-9_\-]+`),
	}
	for _, re := range redactRules {
		res = re.ReplaceAllString(res, "[REDACTED]")
	}
	return res
}

// verifyClean ensures no sensitive patterns remain after redaction.
func (s *Scrubber) verifyClean(text, fieldName string) error {
	for _, r := range s.rules {
		if r.Pattern.MatchString(text) {
			return fmt.Errorf("field %s contains unredacted sensitive pattern [%s]", fieldName, r.Code)
		}
	}
	return nil
}

func normalizeStrings(vals []string) []string {
	set := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			set[v] = struct{}{}
		}
	}
	res := make([]string, 0, len(set))
	for v := range set {
		res = append(res, v)
	}
	sort.Strings(res)
	return res
}

// ScrubManifest scrubs an Attempt Run Manifest, task class, and criteria into a durable, verified Fixture.
func (s *Scrubber) ScrubManifest(manifest runmanifest.Manifest, taskClass string, authorityCeiling, criteria []string) (Fixture, error) {
	taskClass = strings.TrimSpace(taskClass)
	if taskClass == "" {
		taskClass = "default"
	}

	// 1. Sanitize objective
	cleanObjective := s.sanitizeString(manifest.TaskObjective)
	if err := s.verifyClean(cleanObjective, "Task.Objective"); err != nil {
		return Fixture{}, err
	}

	// 2. Normalize and sanitize criteria
	normCriteria := normalizeStrings(criteria)
	cleanCriteria := make([]string, 0, len(normCriteria))
	for i, c := range normCriteria {
		cleanC := s.sanitizeString(c)
		if err := s.verifyClean(cleanC, fmt.Sprintf("Task.AcceptanceCriteria[%d]", i)); err != nil {
			return Fixture{}, err
		}
		cleanCriteria = append(cleanCriteria, cleanC)
	}

	// 3. Normalize visible capabilities (drop AccessContext / provider credentials)
	rawCaps := make([]string, 0, len(manifest.VisibleCapabilities))
	for _, cap := range manifest.VisibleCapabilities {
		if cap.SemanticName != "" {
			rawCaps = append(rawCaps, cap.SemanticName)
		}
	}
	cleanCaps := normalizeStrings(rawCaps)
	cleanCeiling := normalizeStrings(authorityCeiling)
	if len(cleanCeiling) == 0 {
		cleanCeiling = append([]string(nil), cleanCaps...)
	}

	// 4. Derive deterministic FixtureID from stable task semantics & context
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%s:%s", cleanObjective, taskClass, manifest.TaskPayloadHash)))
	fixtureID := fmt.Sprintf("fix-%s", hex.EncodeToString(h.Sum(nil))[:16])

	passedChecks := make([]string, 0, len(s.rules))
	for _, r := range s.rules {
		passedChecks = append(passedChecks, r.Code)
	}

	fixture := Fixture{
		Version:   FixtureVersion,
		FixtureID: fixtureID,
		Task: TaskSemantics{
			Objective:            cleanObjective,
			TaskClass:            taskClass,
			RequiredCapabilities: cleanCaps,
			AuthorityCeiling:     cleanCeiling,
			AcceptanceCriteria:   cleanCriteria,
			TaskPayloadHash:      manifest.TaskPayloadHash,
		},
		Execution: ExecutionMetadata{
			ExecutorKind:          manifest.ExecutorKind,
			ExecutorModel:         manifest.ExecutorModel,
			TEBProfile:            manifest.TEB.Name,
			ContextProjectionHash: manifest.ContextProjectionHash,
			PolicyHash:            manifest.Policy.PolicySetHash,
		},
		Invariants: ExpectedInvariants{
			RequireAuthorityCeilingHeld: true,
			RequireZeroAmbientNetwork:   true,
			RequireAuditTamperEvident:   true,
			RequireNoStaleAttemptAction: true,
		},
		CostEnvelope: CostEnvelope{
			MaxTokens:   8192,
			MaxExposure: 1000,
		},
		Sanitization: SanitizationProvenance{
			SanitizerVersion: SanitizerVersion,
			RulesetHash:      s.rulesetHash,
			PassedChecks:     passedChecks,
		},
	}

	if err := fixture.Validate(); err != nil {
		return Fixture{}, fmt.Errorf("scrubbed fixture invalid: %w", err)
	}

	return fixture, nil
}

// SaveFixture serializes and writes a validated Fixture to disk, enforcing clean scan at persistence boundary.
func SaveFixture(path string, f Fixture) error {
	if err := f.Validate(); err != nil {
		return fmt.Errorf("invalid fixture: %w", err)
	}
	scrubber := NewScrubber()
	if err := scrubber.verifyClean(f.Task.Objective, "Task.Objective"); err != nil {
		return fmt.Errorf("persistence boundary scan failed: %w", err)
	}
	for i, c := range f.Task.AcceptanceCriteria {
		if err := scrubber.verifyClean(c, fmt.Sprintf("Task.AcceptanceCriteria[%d]", i)); err != nil {
			return fmt.Errorf("persistence boundary scan failed: %w", err)
		}
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fixture: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create fixture dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write fixture file: %w", err)
	}
	return nil
}

// LoadFixture reads and validates a Fixture from disk with strict schema decoding (disallowing unknown fields).
func LoadFixture(path string) (Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("read fixture file: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	var f Fixture
	if err := decoder.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("strict decode fixture: %w", err)
	}
	if err := f.Validate(); err != nil {
		return Fixture{}, fmt.Errorf("validate loaded fixture: %w", err)
	}
	return f, nil
}
