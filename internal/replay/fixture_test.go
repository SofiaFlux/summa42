package replay_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/replay"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
)

func sampleRawManifest() runmanifest.Manifest {
	return runmanifest.Manifest{
		Version:         1,
		TaskID:          domain.ID("task_raw_1"),
		TaskObjective:   "Process payment with secret key sk_live_secret1234567890",
		TaskPayloadHash: "hash123",
		AttemptID:       domain.ID("att_raw_1"),
		FenceGeneration: 1,
		ExecutorKind:    "model-runner",
		ExecutorVersion: "1.0.0",
		ExecutorModel:   "claude-3-7-sonnet",
		Runtime: runmanifest.BuildMetadata{
			RuntimeVersion: "v1.2.3",
			RuntimeCommit:  "abc1234",
		},
		Policy: runmanifest.PolicySnapshot{
			PolicySetID:            "pol_1",
			PolicySetHash:          "hash_pol",
			PolicyCapabilitiesHash: "hash_caps",
		},
		TEB: runmanifest.TEBSnapshot{
			Name:       "strict_oci",
			Level:      domain.EnforcementEnforced,
			Guarantees: []string{"no_host_network", "read_only_root"},
		},
		VisibleCapabilities: []runmanifest.CapabilitySnapshot{
			{SemanticName: "payment.process", Provider: "stripe", AccessContext: "bearer_token=sk_live_verysecret123"},
			{SemanticName: "db.query", Provider: "sqlite", AccessContext: "secret_db_pass"},
		},
		ResourceEnvelopeID:    "env_1",
		ContextProjectionHash: "ctx_proj_hash_abc",
		StartedAt:             time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestReplayFixture_SchemaValidation(t *testing.T) {
	fixture := replay.Fixture{
		Version:   replay.FixtureVersion,
		FixtureID: "fix_sample_01",
		Task: replay.TaskSemantics{
			Objective:            "Sanitized objective without secrets",
			TaskClass:            "payment",
			RequiredCapabilities: []string{"payment.process"},
			AcceptanceCriteria:   []string{"payment processed"},
		},
		Execution: replay.ExecutionMetadata{
			ExecutorKind:          "model-runner",
			ExecutorModel:         "claude-3-7-sonnet",
			TEBProfile:            "strict_oci",
			ContextProjectionHash: "ctx_proj_hash_abc",
		},
		Invariants: replay.ExpectedInvariants{
			RequireAuthorityCeilingHeld: true,
			RequireZeroAmbientNetwork:   true,
			RequireAuditTamperEvident:   true,
		},
		CostEnvelope: replay.CostEnvelope{
			MaxTokens:   4000,
			MaxExposure: 100,
		},
		Sanitization: replay.SanitizationProvenance{
			SanitizerVersion: replay.SanitizerVersion,
			RulesetHash:      "hash_rules_1",
			PassedChecks:     []string{"PRIVATE_KEY", "API_SECRET_TOKEN"},
		},
	}

	if err := fixture.Validate(); err != nil {
		t.Fatalf("fixture validation failed: %v", err)
	}

	// Marshaling round-trip
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	var parsed replay.Fixture
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	if parsed.FixtureID != fixture.FixtureID {
		t.Fatalf("expected fixture ID %s, got %s", fixture.FixtureID, parsed.FixtureID)
	}
}

func TestReplayFixture_RejectInvalidSchema(t *testing.T) {
	// Missing TaskClass
	invalid := replay.Fixture{
		Version:   replay.FixtureVersion,
		FixtureID: "fix_1",
		Task: replay.TaskSemantics{
			Objective: "Objective",
		},
		Execution: replay.ExecutionMetadata{
			ExecutorKind: "model",
		},
		Invariants: replay.ExpectedInvariants{RequireAuthorityCeilingHeld: true},
		CostEnvelope: replay.CostEnvelope{
			MaxTokens: 100,
		},
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected validation error for missing TaskClass")
	}

	// Negative cost envelope
	invalidCost := invalid
	invalidCost.Task.TaskClass = "default"
	invalidCost.CostEnvelope.MaxTokens = -10
	if err := invalidCost.Validate(); err == nil {
		t.Fatal("expected validation error for negative tokens")
	}

	// No invariants
	invalidInvariants := invalidCost
	invalidInvariants.CostEnvelope.MaxTokens = 100
	invalidInvariants.Invariants = replay.ExpectedInvariants{}
	if err := invalidInvariants.Validate(); err == nil {
		t.Fatal("expected validation error for empty invariants")
	}
}

func TestScrubber_StripsSecretsAndEnforcesProvenance(t *testing.T) {
	manifest := sampleRawManifest()
	scrubber := replay.NewScrubber()

	fixture, err := scrubber.ScrubManifest(manifest, "payment", []string{"Criteria: execute with auth_sec=auth_sec_123"})
	if err != nil {
		t.Fatalf("scrub manifest: %v", err)
	}

	if err := fixture.Validate(); err != nil {
		t.Fatalf("validated scrubbed fixture: %v", err)
	}

	fixtureJSON, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal scrubbed fixture: %v", err)
	}
	rawStr := string(fixtureJSON)

	// Ensure no raw secrets leaked into serialized fixture
	forbiddenStrings := []string{
		"sk_live_secret1234567890",
		"sk_live_verysecret123",
		"auth_sec_123",
		"secret_db_pass",
	}

	for _, secret := range forbiddenStrings {
		if strings.Contains(rawStr, secret) {
			t.Fatalf("critical leak: scrubbed fixture contains secret %q", secret)
		}
	}

	if !strings.Contains(fixture.Task.Objective, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in sanitized objective, got: %s", fixture.Task.Objective)
	}
	if fixture.Sanitization.RulesetHash == "" {
		t.Fatal("expected RulesetHash in sanitization provenance")
	}
	if len(fixture.Sanitization.PassedChecks) == 0 {
		t.Fatal("expected PassedChecks in sanitization provenance")
	}
}

func TestScrubber_DeterminismAcrossRuns(t *testing.T) {
	manifest := sampleRawManifest()
	scrubber := replay.NewScrubber()

	criteria := []string{"Criteria B", "Criteria A"}
	fixture1, err := scrubber.ScrubManifest(manifest, "task_class_1", criteria)
	if err != nil {
		t.Fatal(err)
	}

	// Pass unsorted criteria and ensure deterministic output
	fixture2, err := scrubber.ScrubManifest(manifest, "task_class_1", []string{"Criteria A", "Criteria B"})
	if err != nil {
		t.Fatal(err)
	}

	data1, _ := json.Marshal(fixture1)
	data2, _ := json.Marshal(fixture2)

	if string(data1) != string(data2) {
		t.Fatalf("fixtures not identical:\n1: %s\n2: %s", string(data1), string(data2))
	}
}

func TestScrubber_RejectsUnredactableSecretsAndKeyMaterial(t *testing.T) {
	manifest := sampleRawManifest()
	manifest.TaskObjective = "-----BEGIN EC PRIVATE KEY-----\nMHQCAQEEIB..."
	scrubber := replay.NewScrubber()

	_, err := scrubber.ScrubManifest(manifest, "default", nil)
	if err == nil {
		t.Fatal("expected rejection of private key material")
	}

	// Also test private key in criteria
	manifest2 := sampleRawManifest()
	manifest2.TaskObjective = "Clean objective"
	_, err = scrubber.ScrubManifest(manifest2, "default", []string{"-----BEGIN RSA PRIVATE KEY-----..."})
	if err == nil {
		t.Fatal("expected rejection of private key in criteria")
	}
}

func TestReplayFixture_SaveLoadAndStrictDecoding(t *testing.T) {
	manifest := sampleRawManifest()
	scrubber := replay.NewScrubber()

	fixture, err := scrubber.ScrubManifest(manifest, "default", []string{"Criteria 1"})
	if err != nil {
		t.Fatalf("scrub manifest: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")

	if err := replay.SaveFixture(path, fixture); err != nil {
		t.Fatalf("save fixture: %v", err)
	}

	loaded, err := replay.LoadFixture(path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	if loaded.FixtureID != fixture.FixtureID {
		t.Fatalf("expected FixtureID %s, got %s", fixture.FixtureID, loaded.FixtureID)
	}

	// Test strict decoding rejection on unknown fields
	tamperedPath := filepath.Join(dir, "tampered.json")
	tamperedJSON := `{"version": 1, "fixture_id": "fix-1", "unknown_rogue_field": "injected", "task": {"objective": "o", "task_class": "c"}, "execution": {"executor_kind": "k"}, "invariants": {"require_authority_ceiling_held": true}, "cost_envelope": {"max_tokens": 100}}`
	if err := os.WriteFile(tamperedPath, []byte(tamperedJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := replay.LoadFixture(tamperedPath); err == nil {
		t.Fatal("expected LoadFixture to reject unknown fields with strict decoding")
	}

	// Test SaveFixture enforces boundary scan
	leakyFixture := fixture
	leakyFixture.Task.Objective = "Contains unredacted /home/topol/secret.pem path"
	leakyPath := filepath.Join(dir, "leaky.json")
	if err := replay.SaveFixture(leakyPath, leakyFixture); err == nil {
		t.Fatal("expected SaveFixture to reject unredacted path at persistence boundary")
	}
}
