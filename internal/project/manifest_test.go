package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/policy"
)

func sampleManifestJSON(mutator func(string) string) []byte {
	raw := `{
  "schema_version": 1,
  "project": "daas",
  "mission_statement": "Operate DaaS",
  "data_class": "public",
  "workspace_root": "/var/lib/summa42/ws/daas",
  "repositories": [
    {
      "id": "summa-gh",
      "host": "github",
      "repository": "SofiaFlux/summa42",
      "local_path": "/var/lib/summa42/repos/summa42",
      "base_branch": "main",
      "credential_file": "/etc/summa42/creds/github_token",
      "data_class": "public",
      "allowed_prefixes": ["internal/"],
      "context_files": ["AGENTS.md"],
      "validation": {
        "commands": [["go", "test", "./..."]],
        "user": "summa42-validate",
        "timeout": "10m"
      },
      "sandbox": {
        "toolchains": ["go1.27"],
        "fetch_allowlist": ["pkg.go.dev"],
        "registry_allowlist": { "mode": "lockfile" },
        "greenfield": false
      },
      "intake": {
        "maintainers": ["owner-login"],
        "bot_login": "summa42-bot",
        "auto_grant_label": "summa42-pilot",
        "poll_interval": "30s"
      },
      "case_grants": {
        "capabilities": ["github.issue.read", "workspace.repo.write", "workspace.test"],
        "actions": ["github.issue.read", "workspace.repo.write", "workspace.test"],
        "work_capabilities": ["workspace.repo.write", "workspace.test"]
      }
    },
    {
      "id": "daas-ado",
      "host": "ado",
      "organization": "https://dev.azure.com/contoso",
      "ado_project": "daas",
      "repository": "daas-api",
      "reviewer_id": "rev-sp-1",
      "local_path": "/var/lib/summa42/repos/daas-api",
      "base_branch": "main",
      "credential_file": "/etc/summa42/creds/ado_token",
      "data_class": "private",
      "allowed_prefixes": ["src/"],
      "validation": {
        "commands": [["pytest"]],
        "user": "summa42-validate",
        "timeout": "10m"
      }
    }
  ],
  "models": {
    "producer": {
      "binary": "cmd",
      "commandcode_model": "zai-org/glm-5.3",
      "user": "summa42-model",
      "timeout": "30m",
      "allow_paid": true,
      "zdr": true
    },
    "reviewer": {
      "binary": "cmd",
      "commandcode_model": "zai-org/glm-5.3-review",
      "user": "summa42-review",
      "timeout": "15m",
      "allow_paid": true,
      "zdr": true
    }
  },
  "limits": {
    "max_steps": 3,
    "remaining_budget": 10,
    "max_repairs": 2,
    "lease_duration": "5m",
    "heartbeat_interval": "1m",
    "max_wall_time": "2h"
  }
}`
	if mutator != nil {
		raw = mutator(raw)
	}
	return []byte(raw)
}

func defaultOPAEngine(t *testing.T, module string) policy.PolicyEngine {
	t.Helper()
	sum := sha256.Sum256([]byte(module))
	return policy.NewOPAEngine(policy.OPAConfig{
		ModuleName:    "summa42-default.rego",
		Module:        module,
		PolicySetID:   policy.DefaultPolicySetID,
		PolicySetHash: hex.EncodeToString(sum[:]),
	})
}

func TestManifestRejectsUnknownField(t *testing.T) {
	raw := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"schema_version": 1,`, `"schema_version": 1, "unexpected_extra": true,`, 1)
	})
	if _, err := ParseManifest(raw); err == nil {
		t.Fatal("expected unknown field in project manifest to be rejected")
	}

	// Inline secrets/tokens also fail cleanly.
	rawSecret := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"credential_file": "/etc/summa42/creds/github_token",`, `"token": "ghp_secret",`, 1)
	})
	if _, err := ParseManifest(rawSecret); err == nil {
		t.Fatal("expected inline secret token field to be rejected")
	}
}

func TestManifestGrantsCannotExceedPolicy(t *testing.T) {
	ctx := context.Background()
	eng := defaultOPAEngine(t, policy.DefaultModule)
	now := time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)

	// Known valid grants pass under policy_mvc_default_v2.
	valid, err := ParseManifest(sampleManifestJSON(nil))
	if err != nil {
		t.Fatalf("ParseManifest valid failed: %v", err)
	}
	if err := ValidateManifestWithPolicy(ctx, valid, eng, "owner-1", now); err != nil {
		t.Fatalf("valid manifest rejected by default v2 policy: %v", err)
	}

	// Undeclared/HIGH capability (e.g. policy.upgrade) must be refused by active policy.
	highRaw := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"workspace.test"]`, `"workspace.test", "policy.upgrade"]`, 1)
	})
	highM, err := ParseManifest(highRaw)
	if err != nil {
		t.Fatalf("ParseManifest highM: %v", err)
	}
	if err := ValidateManifestWithPolicy(ctx, highM, eng, "owner-1", now); err == nil {
		t.Fatal("expected high-risk capability in manifest grant to be refused by policy")
	}

	// MEDIUM-risk publish capability under v1 policy (LOW only) must be refused.
	v1Module := `package summa42
default decision := {"outcome": "DENY", "reason_codes": ["deny"]}
decision := {"outcome": "ALLOW", "reason_codes": ["low"]} if {
  input.risk == "LOW"
  input.authority_valid == true
}`
	v1Eng := defaultOPAEngine(t, v1Module)
	medRaw := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"workspace.test"]`, `"workspace.test", "github.pr.create"]`, 1)
	})
	medM, err := ParseManifest(medRaw)
	if err != nil {
		t.Fatalf("ParseManifest medM: %v", err)
	}
	if err := ValidateManifestWithPolicy(ctx, medM, v1Eng, "owner-1", now); err == nil {
		t.Fatal("expected MEDIUM-risk grant to fail against LOW-only v1 policy")
	}
}

func TestPrivateDataClassRefusesNonZDRRemoteModel(t *testing.T) {
	// sampleManifestJSON has one private repo (daas-ado), so non-ZDR remote producer must be refused.
	nonZDR := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"zdr": true`, `"zdr": false`, 1)
	})
	if _, err := ParseManifest(nonZDR); err == nil {
		t.Fatal("expected non-ZDR remote producer to be refused for manifest with private repo")
	}

	// Local-only profile is allowed without remote ZDR flag.
	localAllowed := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"commandcode_model": "zai-org/glm-5.3",
      "user": "summa42-model",
      "timeout": "30m",
      "allow_paid": true,
      "zdr": true`, `"profile": "ollama", "local": true,
      "user": "summa42-model",
      "timeout": "30m"`, 1)
	})
	if _, err := ParseManifest(localAllowed); err != nil {
		t.Fatalf("expected local model profile to be permitted for private repo: %v", err)
	}
}

func TestUnsignedManifestRefusedAndEditedRefusedUntilReapplied(t *testing.T) {
	svc, _, collectiveID, owner, clk := setupCollective(t)
	ctx := context.Background()
	eng := defaultOPAEngine(t, policy.DefaultModule)
	home := t.TempDir()

	cmd, err := NewCreateCommand(owner, collectiveID, "daas", "DaaS", "Create daas", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	daas, err := svc.CreateSigned(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}

	projDir := filepath.Join(home, "projects", "daas")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(projDir, "project.json")
	if err := os.WriteFile(manifestPath, sampleManifestJSON(nil), 0o600); err != nil {
		t.Fatal(err)
	}

	// Unsigned project must be refused even though file exists on disk.
	if _, _, _, err := svc.VerifyActiveManifest(ctx, home, "daas", eng); !errors.Is(err, ErrManifestUnsigned) {
		t.Fatalf("unsigned manifest err = %v, want ErrManifestUnsigned", err)
	}

	// Sign and apply manifest.
	_, digest, err := LoadManifestFile(home, "daas")
	if err != nil {
		t.Fatal(err)
	}
	applyCmd, err := NewApplyCommand(owner, collectiveID, daas.ID, "daas", digest, "Apply initial manifest", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyManifestSigned(ctx, applyCmd); err != nil {
		t.Fatalf("ApplyManifestSigned failed: %v", err)
	}

	// Verified manifest succeeds.
	if _, _, gotDigest, err := svc.VerifyActiveManifest(ctx, home, "daas", eng); err != nil || gotDigest != digest {
		t.Fatalf("VerifyActiveManifest = (%q, %v), want %q", gotDigest, err, digest)
	}

	// Editing project.json on disk must refuse until re-applied.
	edited := sampleManifestJSON(func(s string) string {
		return strings.Replace(s, `"max_steps": 3,`, `"max_steps": 5,`, 1)
	})
	if err := os.WriteFile(manifestPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.VerifyActiveManifest(ctx, home, "daas", eng); !errors.Is(err, ErrManifestMismatch) {
		t.Fatalf("edited manifest err = %v, want ErrManifestMismatch", err)
	}

	// Re-applying with new SHA-256 restores validity.
	_, newDigest, err := LoadManifestFile(home, "daas")
	if err != nil {
		t.Fatal(err)
	}
	reapplyCmd, err := NewApplyCommand(owner, collectiveID, daas.ID, "daas", newDigest, "Reapply edited manifest", clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApplyManifestSigned(ctx, reapplyCmd); err != nil {
		t.Fatal(err)
	}
	if _, m, gotDigest, err := svc.VerifyActiveManifest(ctx, home, "daas", eng); err != nil || gotDigest != newDigest || m.Limits.MaxSteps != 5 {
		t.Fatalf("after reapply: digest=%q steps=%d err=%v", gotDigest, m.Limits.MaxSteps, err)
	}
}
