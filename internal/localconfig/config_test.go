package localconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestConfigRoundTripKeepsControlSecretPrivate(t *testing.T) {
	home := t.TempDir()
	token, err := NewControlToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 32 {
		t.Fatalf("control token length=%d", len(token))
	}
	cfg := Config{
		Version:                       CurrentVersion,
		CollectiveID:                  domain.ID("collective_test"),
		OwnerPrincipalID:              domain.ID("owner_test"),
		CubePrincipalID:               domain.ID("cube_test"),
		ConstitutionalRootPrincipalID: domain.ID("root_test"),
		ConstitutionHash:              "constitution-hash",
		ActivePolicySetID:             domain.ID("policy_test"),
		DatabasePath:                  filepath.Join(home, "state", "summa42.db"),
		EvidencePath:                  filepath.Join(home, "evidence"),
		ControlToken:                  token,
	}
	if err := Write(home, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if got.ControlToken != token || got.CollectiveID != cfg.CollectiveID || got.ActivePolicySetID != cfg.ActivePolicySetID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(home, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("config mode=%#o, want 0600", info.Mode().Perm())
		}
	}
}

func TestResolveHomeUsesEnvironmentOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-home")
	t.Setenv("SUMMA42_HOME", want)
	got, err := ResolveHome("")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("resolved home=%q, want %q", got, abs)
	}
}

func TestFieldFeedbackDefaultsToDisabledLocalOnly(t *testing.T) {
	cfg := FieldFeedbackConfig{}.withDefaults()
	if cfg.Enabled {
		t.Fatal("field feedback defaulted to enabled")
	}
	if cfg.Mode != FeedbackModeLocalOnly {
		t.Fatalf("default feedback mode = %q, want LOCAL_ONLY", cfg.Mode)
	}
	if cfg.Provider != "" || cfg.Destination != "" || cfg.MaintenanceEnvelopeID != "" {
		t.Fatalf("default feedback config carries export settings: %+v", cfg)
	}
	if cfg.RequiredEnforcement != domain.EnforcementEnforced {
		t.Fatalf("default feedback enforcement = %q, want ENFORCED", cfg.RequiredEnforcement)
	}
}

func TestFieldFeedbackExportModesRequireProviderDestinationAndBudget(t *testing.T) {
	for _, mode := range []FeedbackMode{FeedbackModeAutoIfAllowed, FeedbackModeRequireApproval} {
		cfg := FieldFeedbackConfig{Enabled: true, Mode: mode}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("mode %s accepted without provider/destination/envelope", mode)
		}
		cfg.Provider = "github"
		cfg.Destination = "owner/repo"
		cfg.MaintenanceEnvelopeID = "maintenance"
		cfg.RequiredEnforcement = domain.EnforcementEnforced
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid mode %s rejected: %v", mode, err)
		}
	}
}

func TestFieldFeedbackRejectsCredentialLikeConfigFieldsViaStrictDecode(t *testing.T) {
	home := t.TempDir()
	token, err := NewControlToken()
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{
	  "version":1,
	  "collective_id":"collective",
	  "owner_principal_id":"owner",
	  "cube_principal_id":"cube",
	  "constitutional_root_principal_id":"root",
	  "constitution_hash":"hash",
	  "active_policy_set_id":"policy",
	  "database_path":%q,
	  "evidence_path":%q,
	  "control_token":%q,
	  "field_feedback":{"enabled":true,"mode":"AUTO_IF_ALLOWED","provider":"github","destination":"owner/repo","maintenance_envelope_id":"env","token":"secret"}
	}`, filepath.Join(home, "state.db"), filepath.Join(home, "evidence"), token)
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(home); err == nil {
		t.Fatal("credential-like field in field_feedback was accepted")
	}
}
