package fieldfeedback

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

const sanitizedFeedbackSchemaVersion = 1

type Sanitizer interface {
	Sanitize(context.Context, domain.ID) (domain.SanitizedFeedback, domain.SanitizationResult, error)
}

type Abstracter interface {
	Abstract(context.Context, domain.FeedbackCandidate) (domain.FeedbackCandidate, error)
}

type DeterministicSanitizerConfig struct {
	Version                    string
	DenyPatterns               []*regexp.Regexp
	AllowExecutorMetadata      bool
	AllowSyntheticReproduction bool
}

var safeExportMetadataToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

var safeExportCategories = map[string]struct{}{
	"APPROVAL_FRICTION":                       {},
	"ATTEMPT_RECOVERY":                        {},
	"COST_LATENCY_OUTLIER":                    {},
	"EXECUTOR_SELECTION_FRICTION":             {},
	"HUMAN_INTERVENTION":                      {},
	"INDEPENDENT_PROBLEM":                     {},
	"MALFORMED_CANONICAL_ROW":                 {},
	"POLICY_AUTHORITY_FRICTION":               {},
	"RECOVERY_FRICTION":                       {},
	"REPEATED_ATTEMPT_FAILURE":                {},
	"TEST":                                    {},
	"UNRESOLVED_OPERATION":                    {},
	"VERIFICATION_CHALLENGE_AFTER_ACCEPTANCE": {},
}

type scanRule struct {
	Code    string
	Pattern *regexp.Regexp
}

type DeterministicSanitizer struct {
	store       *state.Store
	clock       clock.Clock
	feedback    *Feedback
	config      DeterministicSanitizerConfig
	rulesetHash string
	abstracter  Abstracter
	builtin     []scanRule
}

type exportProjection struct {
	SchemaVersion     int                     `json:"schema_version"`
	GenericTaskClass  domain.GenericTaskClass `json:"generic_task_class"`
	Category          string                  `json:"category"`
	StateTransitions  []string                `json:"state_transitions"`
	Metrics           NormalizedMetrics       `json:"metrics"`
	HumanIntervention bool                    `json:"human_intervention"`
	Enforcement       domain.EnforcementLevel `json:"enforcement"`
	RuntimeVersion    string                  `json:"runtime_version,omitempty"`
	ExecutorKind      string                  `json:"executor_kind,omitempty"`
	ExecutorVersion   string                  `json:"executor_version,omitempty"`
}

func NewDeterministicSanitizer(
	store *state.Store,
	clk clock.Clock,
	feedback *Feedback,
	cfg DeterministicSanitizerConfig,
) (*DeterministicSanitizer, error) {
	if store == nil || store.DB() == nil || clk == nil || feedback == nil {
		return nil, errors.New("sanitizer store, clock, and feedback service are required")
	}
	cfg.Version = strings.TrimSpace(cfg.Version)
	if cfg.Version == "" {
		return nil, errors.New("sanitizer version is required")
	}
	for _, pattern := range cfg.DenyPatterns {
		if pattern == nil {
			return nil, errors.New("sanitizer deny patterns must not contain nil")
		}
	}
	builtin := builtinScanRules()
	rulesetHash, err := computeRulesetHash(cfg, builtin)
	if err != nil {
		return nil, err
	}
	return &DeterministicSanitizer{
		store: store, clock: clk, feedback: feedback, config: cfg,
		rulesetHash: rulesetHash, builtin: builtin,
	}, nil
}

func (s *DeterministicSanitizer) Sanitize(ctx context.Context, candidateID domain.ID) (domain.SanitizedFeedback, domain.SanitizationResult, error) {
	if s == nil || s.store == nil || s.feedback == nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, errors.New("sanitizer is not configured")
	}
	candidate, err := s.feedback.Candidate(ctx, candidateID)
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	switch candidate.State {
	case domain.FeedbackStateCandidate, domain.FeedbackStateSanitized:
		if err := s.feedback.transitionCandidate(ctx, candidate.ID, domain.FeedbackStateSanitizing); err != nil {
			return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
		}
	case domain.FeedbackStateSanitizing:
		// Resume an interrupted sanitization attempt.
	default:
		return domain.SanitizedFeedback{}, domain.SanitizationResult{},
			fmt.Errorf("candidate %s cannot be sanitized from state %s", candidate.ID, candidate.State)
	}
	candidate, err = s.feedback.Candidate(ctx, candidate.ID)
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}

	if s.abstracter != nil {
		candidate, err = s.abstracter.Abstract(ctx, candidate)
		if err != nil {
			return s.persistUnsafe(ctx, candidate, domain.SanitizationUncertain, []string{"ABSTRACTION_FAILED"})
		}
	}

	inputDigest, err := candidateDigest(candidate)
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	projection, err := s.project(candidate)
	if err != nil {
		return s.persistUnsafe(ctx, candidate, domain.SanitizationUncertain, []string{"PROJECTION_INVALID"})
	}

	preReasons := s.scanProjection(projection)
	if len(preReasons) > 0 {
		return s.persistUnsafeWithDigest(ctx, candidate, inputDigest, domain.SanitizationReject, preReasons)
	}

	content, err := json.Marshal(projection)
	if err != nil {
		return s.persistUnsafeWithDigest(ctx, candidate, inputDigest, domain.SanitizationUncertain, []string{"CANONICAL_JSON_FAILED"})
	}
	postReasons := s.scanText(string(content))
	if len(postReasons) > 0 {
		return s.persistUnsafeWithDigest(ctx, candidate, inputDigest, domain.SanitizationReject, postReasons)
	}

	now := s.clock.Now().UTC()
	contentDigest := sha256.Sum256(content)
	contentHash := hex.EncodeToString(contentDigest[:])
	fingerprintDigest := sha256.Sum256([]byte(fmt.Sprintf(
		"summa42-sanitized-feedback-v1\nschema:%d\ncontent-hash:%s\n",
		sanitizedFeedbackSchemaVersion, contentHash,
	)))
	fingerprint := hex.EncodeToString(fingerprintDigest[:])
	result := domain.SanitizationResult{
		ID: domain.NewID("sanitization"), CandidateID: candidate.ID,
		SanitizerVersion: s.config.Version, RulesetHash: s.rulesetHash,
		InputDigest: inputDigest, Outcome: domain.SanitizationPass,
		ReasonCodesJSON: "[]", CreatedAt: now,
	}
	artifact := domain.SanitizedFeedback{
		ID: domain.NewID("sanitized"), CandidateID: candidate.ID,
		SchemaVersion: sanitizedFeedbackSchemaVersion, ContentJSON: string(content),
		ContentHash: contentHash, SanitizationResultID: result.ID,
		CorrelationKey: candidate.CorrelationKey, Fingerprint: fingerprint, CreatedAt: now,
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := insertSanitizationResult(ctx, tx, result); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO sanitized_feedback("+
				"feedback_id, candidate_id, schema_version, content_json, content_hash, sanitization_result_id, "+
				"correlation_key, fingerprint, created_at"+
				") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			artifact.ID, artifact.CandidateID, artifact.SchemaVersion, artifact.ContentJSON,
			artifact.ContentHash, artifact.SanitizationResultID, artifact.CorrelationKey,
			artifact.Fingerprint, formatTime(artifact.CreatedAt),
		); err != nil {
			return fmt.Errorf("insert sanitized feedback: %w", err)
		}
		return s.feedback.transitionCandidateInTx(ctx, tx, candidate.ID, domain.FeedbackStateSanitized)
	})
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	return artifact, result, nil
}

func (s *DeterministicSanitizer) project(candidate domain.FeedbackCandidate) (exportProjection, error) {
	var transitions []string
	if err := json.Unmarshal([]byte(candidate.StateTransitionJSON), &transitions); err != nil {
		return exportProjection{}, fmt.Errorf("decode candidate transitions: %w", err)
	}
	for _, transition := range transitions {
		if !safeTransitionToken(transition) {
			return exportProjection{}, fmt.Errorf("unsafe candidate transition token %q", transition)
		}
	}
	var metrics NormalizedMetrics
	if err := json.Unmarshal([]byte(candidate.MetricsJSON), &metrics); err != nil {
		return exportProjection{}, fmt.Errorf("decode candidate metrics: %w", err)
	}
	if _, ok := safeExportCategories[candidate.Category]; !ok {
		return exportProjection{}, fmt.Errorf("candidate category %q is not in the outbound allowlist", candidate.Category)
	}
	projection := exportProjection{
		SchemaVersion:    sanitizedFeedbackSchemaVersion,
		GenericTaskClass: candidate.GenericTaskClass, Category: candidate.Category,
		StateTransitions: transitions, Metrics: metrics, HumanIntervention: candidate.HumanIntervention,
		Enforcement: candidate.Enforcement,
	}
	if s.config.AllowExecutorMetadata {
		for label, value := range map[string]string{
			"runtime version":  candidate.RuntimeVersion,
			"executor kind":    candidate.ExecutorKind,
			"executor version": candidate.ExecutorVersion,
		} {
			if value != "" && !safeExportMetadataToken.MatchString(value) {
				return exportProjection{}, fmt.Errorf("%s is not a privacy-safe metadata token", label)
			}
		}
		projection.RuntimeVersion = candidate.RuntimeVersion
		projection.ExecutorKind = candidate.ExecutorKind
		projection.ExecutorVersion = candidate.ExecutorVersion
	}
	return projection, nil
}

func (s *DeterministicSanitizer) scanProjection(p exportProjection) []string {
	parts := []string{
		p.Category, p.RuntimeVersion, p.ExecutorKind, p.ExecutorVersion,
	}
	return s.scanText(strings.Join(parts, "\n"))
}

func (s *DeterministicSanitizer) scanText(value string) []string {
	reasons := map[string]struct{}{}
	for _, rule := range s.builtin {
		if rule.Pattern.MatchString(value) {
			reasons[rule.Code] = struct{}{}
		}
	}
	for i, pattern := range s.config.DenyPatterns {
		if pattern.MatchString(value) {
			reasons[fmt.Sprintf("CONFIGURED_DENY_%d", i+1)] = struct{}{}
		}
	}
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}

func (s *DeterministicSanitizer) persistUnsafe(
	ctx context.Context,
	candidate domain.FeedbackCandidate,
	outcome domain.SanitizationOutcome,
	reasons []string,
) (domain.SanitizedFeedback, domain.SanitizationResult, error) {
	digest, err := candidateDigest(candidate)
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	return s.persistUnsafeWithDigest(ctx, candidate, digest, outcome, reasons)
}

func (s *DeterministicSanitizer) persistUnsafeWithDigest(
	ctx context.Context,
	candidate domain.FeedbackCandidate,
	inputDigest string,
	outcome domain.SanitizationOutcome,
	reasons []string,
) (domain.SanitizedFeedback, domain.SanitizationResult, error) {
	if outcome != domain.SanitizationReject && outcome != domain.SanitizationUncertain {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, errors.New("unsafe persistence requires REJECT or UNCERTAIN")
	}
	reasons = uniqueSorted(reasons)
	reasonsJSON, err := json.Marshal(reasons)
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	result := domain.SanitizationResult{
		ID: domain.NewID("sanitization"), CandidateID: candidate.ID,
		SanitizerVersion: s.config.Version, RulesetHash: s.rulesetHash,
		InputDigest: inputDigest, Outcome: outcome, ReasonCodesJSON: string(reasonsJSON),
		CreatedAt: s.clock.Now().UTC(),
	}
	target := domain.FeedbackStateRejectedUnsafe
	if outcome == domain.SanitizationUncertain {
		target = domain.FeedbackStateLocalOnly
	}
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := insertSanitizationResult(ctx, tx, result); err != nil {
			return err
		}
		return s.feedback.transitionCandidateInTx(ctx, tx, candidate.ID, target)
	})
	if err != nil {
		return domain.SanitizedFeedback{}, domain.SanitizationResult{}, err
	}
	return domain.SanitizedFeedback{}, result, nil
}

func insertSanitizationResult(ctx context.Context, tx *sql.Tx, result domain.SanitizationResult) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO sanitization_results("+
			"sanitization_id, candidate_id, sanitizer_version, ruleset_hash, input_digest, outcome, reason_codes_json, created_at"+
			") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		result.ID, result.CandidateID, result.SanitizerVersion, result.RulesetHash,
		result.InputDigest, result.Outcome, result.ReasonCodesJSON, formatTime(result.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert sanitization result: %w", err)
	}
	return nil
}

func candidateDigest(candidate domain.FeedbackCandidate) (string, error) {
	material := struct {
		CandidateID         domain.ID               `json:"candidate_id"`
		GenericTaskClass    domain.GenericTaskClass `json:"generic_task_class"`
		Category            string                  `json:"category"`
		ExpectedBehavior    string                  `json:"expected_behavior"`
		ObservedBehavior    string                  `json:"observed_behavior"`
		StateTransitionJSON string                  `json:"state_transition_json"`
		MetricsJSON         string                  `json:"metrics_json"`
		HumanIntervention   bool                    `json:"human_intervention"`
		RecoveryResult      string                  `json:"recovery_result"`
		RuntimeVersion      string                  `json:"runtime_version"`
		ExecutorKind        string                  `json:"executor_kind"`
		ExecutorVersion     string                  `json:"executor_version"`
		Enforcement         domain.EnforcementLevel `json:"enforcement"`
		CorrelationKey      string                  `json:"correlation_key"`
	}{
		CandidateID: candidate.ID, GenericTaskClass: candidate.GenericTaskClass, Category: candidate.Category,
		ExpectedBehavior: candidate.ExpectedBehavior, ObservedBehavior: candidate.ObservedBehavior,
		StateTransitionJSON: candidate.StateTransitionJSON, MetricsJSON: candidate.MetricsJSON,
		HumanIntervention: candidate.HumanIntervention, RecoveryResult: candidate.RecoveryResult,
		RuntimeVersion: candidate.RuntimeVersion, ExecutorKind: candidate.ExecutorKind,
		ExecutorVersion: candidate.ExecutorVersion, Enforcement: candidate.Enforcement,
		CorrelationKey: candidate.CorrelationKey,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func computeRulesetHash(cfg DeterministicSanitizerConfig, builtin []scanRule) (string, error) {
	deny := make([]string, 0, len(cfg.DenyPatterns))
	for _, pattern := range cfg.DenyPatterns {
		deny = append(deny, pattern.String())
	}
	sort.Strings(deny)
	builtinValues := make([]string, 0, len(builtin))
	for _, rule := range builtin {
		builtinValues = append(builtinValues, rule.Code+"="+rule.Pattern.String())
	}
	material := struct {
		Version                    string   `json:"version"`
		Builtin                    []string `json:"builtin"`
		Deny                       []string `json:"deny"`
		AllowExecutorMetadata      bool     `json:"allow_executor_metadata"`
		AllowSyntheticReproduction bool     `json:"allow_synthetic_reproduction"`
	}{
		Version: cfg.Version, Builtin: builtinValues, Deny: deny,
		AllowExecutorMetadata:      cfg.AllowExecutorMetadata,
		AllowSyntheticReproduction: cfg.AllowSyntheticReproduction,
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func builtinScanRules() []scanRule {
	return []scanRule{
		{Code: "WINDOWS_PATH", Pattern: regexp.MustCompile(`(?i)\b[A-Z]:\\(?:[^\\\s]+\\)*[^\\\s]+`)},
		{Code: "UNIX_PATH", Pattern: regexp.MustCompile(`/(?:home|Users|private|var|etc|opt|srv|mnt|workspace|workspaces|repo|repos)/[^\s]+`)},
		{Code: "URL", Pattern: regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)},
		{Code: "IPV4", Pattern: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
		{Code: "INTERNAL_HOSTNAME", Pattern: regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:internal|local|corp|lan)\b`)},
		{Code: "BEARER_TOKEN", Pattern: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)},
		{Code: "SECRET_OR_ACCOUNT", Pattern: regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|token|secret|password|account[_-]?id)\s*[:=]\s*[^\s,;]+`)},
		{Code: "EMAIL", Pattern: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
		{Code: "TICKET_ID", Pattern: regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d{2,}\b`)},
		{Code: "DIFF_OR_CODE", Pattern: regexp.MustCompile(`(?im)(?:^diff --git |^@@ |^--- a/|^\+\+\+ b/|\bpackage\s+[A-Za-z_][A-Za-z0-9_]*|\bfunc\s+[A-Za-z_][A-Za-z0-9_]*\s*\()`)},
	}
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (s *Feedback) SanitizedFeedback(ctx context.Context, id domain.ID) (domain.SanitizedFeedback, error) {
	if err := s.configured(); err != nil {
		return domain.SanitizedFeedback{}, err
	}
	id = domain.ID(strings.TrimSpace(string(id)))
	if id == "" {
		return domain.SanitizedFeedback{}, errors.New("sanitized feedback id is required")
	}
	var out domain.SanitizedFeedback
	var createdAt string
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT feedback_id, candidate_id, schema_version, content_json, content_hash, sanitization_result_id, "+
			"correlation_key, fingerprint, created_at FROM sanitized_feedback WHERE feedback_id = ?", id,
	).Scan(&out.ID, &out.CandidateID, &out.SchemaVersion, &out.ContentJSON, &out.ContentHash,
		&out.SanitizationResultID, &out.CorrelationKey, &out.Fingerprint, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SanitizedFeedback{}, fmt.Errorf("sanitized feedback %s not found", id)
	}
	if err != nil {
		return domain.SanitizedFeedback{}, err
	}
	out.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.SanitizedFeedback{}, fmt.Errorf("parse sanitized feedback created_at: %w", err)
	}
	return out, nil
}

func (s *Feedback) LatestSanitizedFeedbackForCandidate(
	ctx context.Context,
	candidateID domain.ID,
) (domain.SanitizedFeedback, bool, error) {
	if err := s.configured(); err != nil {
		return domain.SanitizedFeedback{}, false, err
	}
	candidateID = domain.ID(strings.TrimSpace(string(candidateID)))
	if candidateID == "" {
		return domain.SanitizedFeedback{}, false, errors.New("candidate id is required")
	}
	var feedbackID domain.ID
	err := s.store.DB().QueryRowContext(ctx,
		"SELECT feedback_id FROM sanitized_feedback WHERE candidate_id = ? ORDER BY created_at DESC, feedback_id DESC LIMIT 1",
		candidateID,
	).Scan(&feedbackID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SanitizedFeedback{}, false, nil
	}
	if err != nil {
		return domain.SanitizedFeedback{}, false, err
	}
	artifact, err := s.SanitizedFeedback(ctx, feedbackID)
	return artifact, err == nil, err
}
