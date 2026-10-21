package fieldfeedback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type sensitiveCase struct {
	Name   string `json:"name"`
	Marker string `json:"marker"`
}

func TestSanitizerLeakCorpusNeverPassesSensitiveMarker(t *testing.T) {
	raw, err := os.ReadFile("testdata/sensitive_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []sensitiveCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			feedback, candidate := sanitizerCandidate(t, "Observed behavior included "+tc.Marker)
			sanitizer, err := NewDeterministicSanitizer(feedback.store, feedback.clock, feedback, DeterministicSanitizerConfig{
				Version:               "test-v1",
				DenyPatterns:          []*regexp.Regexp{regexp.MustCompile(`(?i)ACME-SECRET`)},
				AllowExecutorMetadata: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			artifact, result, err := sanitizer.Sanitize(context.Background(), candidate.ID)
			if err != nil && result.Outcome == domain.SanitizationPass {
				t.Fatalf("PASS returned error: %v", err)
			}
			if result.Outcome == domain.SanitizationPass {
				if artifact.ID == "" {
					t.Fatal("PASS produced no sanitized artifact")
				}
				if strings.Contains(artifact.ContentJSON, tc.Marker) {
					t.Fatalf("PASS leaked marker %q in %s", tc.Marker, artifact.ContentJSON)
				}
			} else if artifact.ID != "" {
				t.Fatalf("%s produced artifact %s", result.Outcome, artifact.ID)
			}
		})
	}
}

func TestSanitizedContentHashAndFingerprintAreCanonicalAndImmutable(t *testing.T) {
	feedback, candidate := sanitizerCandidate(t, "Generic retry recovery needed operator intervention")
	sanitizer, err := NewDeterministicSanitizer(feedback.store, feedback.clock, feedback, DeterministicSanitizerConfig{
		Version: "test-v1", AllowExecutorMetadata: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, result, err := sanitizer.Sanitize(context.Background(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.SanitizationPass {
		t.Fatalf("outcome = %s, want PASS", result.Outcome)
	}
	digest := sha256.Sum256([]byte(artifact.ContentJSON))
	if got := hex.EncodeToString(digest[:]); got != artifact.ContentHash {
		t.Fatalf("content hash = %s, want %s", artifact.ContentHash, got)
	}
	if artifact.Fingerprint == "" {
		t.Fatal("fingerprint is empty")
	}
	if _, err := feedback.store.DB().ExecContext(context.Background(),
		"UPDATE sanitized_feedback SET fingerprint = 'changed' WHERE feedback_id = ?", artifact.ID,
	); err == nil {
		t.Fatal("immutable sanitized feedback UPDATE succeeded")
	}
	if _, err := feedback.store.DB().ExecContext(context.Background(),
		"DELETE FROM sanitized_feedback WHERE feedback_id = ?", artifact.ID,
	); err == nil {
		t.Fatal("immutable sanitized feedback DELETE succeeded")
	}
}

func TestSameSafeContentHasStableFingerprintButDistinctArtifacts(t *testing.T) {
	feedback, firstCandidate := sanitizerCandidate(t, "Generic stable safe behavior")
	secondCandidate, err := feedback.CreateCandidate(context.Background(), CandidateInput{
		ObservationIDs:   []domain.ID{candidateObservationID(t, feedback, firstCandidate.ID)},
		GenericTaskClass: domain.GenericTaskDebugging, Category: "RECOVERY_FRICTION",
		ExpectedBehavior: "recover automatically", ObservedBehavior: "Generic stable safe behavior",
		StateTransitions: []string{"EXECUTING", "FAILED", "ELIGIBLE", "EXECUTING"},
		Metrics:          NormalizedMetrics{}, RuntimeVersion: "runtime-v1", ExecutorKind: "codex",
		ExecutorVersion: "executor-v1", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	sanitizer, err := NewDeterministicSanitizer(feedback.store, feedback.clock, feedback, DeterministicSanitizerConfig{
		Version: "test-v1", AllowExecutorMetadata: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := sanitizer.Sanitize(context.Background(), firstCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := sanitizer.Sanitize(context.Background(), secondCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("two candidates reused one artifact identity")
	}
	if first.Fingerprint != second.Fingerprint || first.ContentHash != second.ContentHash {
		t.Fatalf("same safe projection changed fingerprint/hash: first=%+v second=%+v", first, second)
	}
}

func TestResanitizeChangedLocalTextDoesNotChangeStructuredExport(t *testing.T) {
	feedback, candidate := sanitizerCandidate(t, "Generic safe behavior version one")
	sanitizer, err := NewDeterministicSanitizer(feedback.store, feedback.clock, feedback, DeterministicSanitizerConfig{
		Version: "test-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, _, err := sanitizer.Sanitize(context.Background(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := feedback.store.DB().ExecContext(context.Background(),
		"UPDATE feedback_candidates SET observed_behavior = 'Generic safe behavior version two' WHERE candidate_id = ?",
		candidate.ID,
	); err != nil {
		t.Fatal(err)
	}
	second, _, err := sanitizer.Sanitize(context.Background(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("resanitization reused immutable artifact identity")
	}
	if second.ContentHash != first.ContentHash || second.Fingerprint != first.Fingerprint {
		t.Fatalf("local-only text changed structured export: first=%+v second=%+v", first, second)
	}
	storedFirst, err := feedback.SanitizedFeedback(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedFirst.ContentJSON != first.ContentJSON || storedFirst.ContentHash != first.ContentHash {
		t.Fatal("prior sanitized artifact was mutated")
	}
}

func sanitizerCandidate(t *testing.T, observed string) (*Feedback, domain.FeedbackCandidate) {
	t.Helper()
	observer, taskID, attemptID, _, _ := newObserverHarness(t)
	ctx := context.Background()
	observation, err := observer.Record(ctx, ObservationInput{
		TaskID: taskID, AttemptID: attemptID, Category: "RECOVERY_FRICTION",
		BasisClass: "TEST", SourceKind: "TEST", SummaryLocal: "local observation only",
		Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	feedback := NewFeedback(observer.store, observer.clock)
	candidate, err := feedback.CreateCandidate(ctx, CandidateInput{
		ObservationIDs:   []domain.ID{observation.ID},
		GenericTaskClass: domain.GenericTaskDebugging, Category: "RECOVERY_FRICTION",
		ExpectedBehavior: "recover automatically", ObservedBehavior: observed,
		StateTransitions: []string{"EXECUTING", "FAILED", "ELIGIBLE", "EXECUTING"},
		Metrics:          NormalizedMetrics{}, RuntimeVersion: "runtime-v1", ExecutorKind: "codex",
		ExecutorVersion: "executor-v1", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	return feedback, candidate
}

func candidateObservationID(t *testing.T, feedback *Feedback, candidateID domain.ID) domain.ID {
	t.Helper()
	var id domain.ID
	if err := feedback.store.DB().QueryRowContext(context.Background(),
		"SELECT observation_id FROM feedback_candidate_observations WHERE candidate_id = ? ORDER BY observation_id LIMIT 1",
		candidateID,
	).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSanitizerNeverExportsUntrustedFreeTextWithoutAbstraction(t *testing.T) {
	feedback, candidate := sanitizerCandidate(t, "AcmeBank Phoenix migration failed in SofiaFlux/summa42")
	if _, err := feedback.store.DB().ExecContext(context.Background(),
		"UPDATE feedback_candidates SET expected_behavior = ?, recovery_result = ? WHERE candidate_id = ?",
		"customer AcmeBank should recover", "Phoenix project manual recovery", candidate.ID,
	); err != nil {
		t.Fatal(err)
	}
	sanitizer, err := NewDeterministicSanitizer(feedback.store, feedback.clock, feedback, DeterministicSanitizerConfig{
		Version: "test-v1", AllowExecutorMetadata: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, result, err := sanitizer.Sanitize(context.Background(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.SanitizationPass {
		t.Fatalf("outcome=%s", result.Outcome)
	}
	for _, forbidden := range []string{"AcmeBank", "Phoenix", "SofiaFlux", "summa42", "expected_behavior", "observed_behavior", "recovery_result", "correlation_key"} {
		if strings.Contains(artifact.ContentJSON, forbidden) {
			t.Fatalf("structured export leaked %q: %s", forbidden, artifact.ContentJSON)
		}
	}
}
