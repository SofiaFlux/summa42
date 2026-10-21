package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/teb"
)

type manifestPolicy struct{ compositionPolicy }

func (manifestPolicy) Metadata() (policy.OPAMetadata, error) {
	return policy.OPAMetadata{
		PolicySetID:            "policy-manifest",
		PolicySetHash:          "policy-hash",
		PolicyCapabilitiesHash: "caps-hash",
	}, nil
}

func TestBoxComposesAttemptRunManifestRecorder(t *testing.T) {
	root := t.TempDir()
	box, err := Open(context.Background(), Config{
		StatePath:        filepath.Join(root, "state.db"),
		EvidencePath:     filepath.Join(root, "evidence"),
		CollectiveID:     "collective-1",
		OwnerPrincipalID: "owner-1",
		PolicyEngine:     manifestPolicy{},
		TEBProfile:       teb.PartialProfile("sandbox-test", teb.GuaranteeNonRoot),
		RuntimeVersion:   "v-test",
		RuntimeCommit:    "commit-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if box.RunManifests == nil {
		t.Fatal("RunManifests service is nil")
	}

	missionID, err := box.Purpose.CreateMission(context.Background(), "capture Attempt provenance")
	if err != nil {
		t.Fatal(err)
	}
	task, err := box.Execution.CreateTask(context.Background(), execution.TaskRequest{
		Purpose:             domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria:  []string{"done"},
		RequiredEnforcement: domain.EnforcementPartial,
		ResourceEnvelopeID:  "env-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := box.Execution.StartAttempt(context.Background(), task.ID, "codex", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	record, err := box.RunManifests.Manifest(context.Background(), attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Manifest.Runtime.RuntimeVersion != "v-test" || record.Manifest.Runtime.RuntimeCommit != "commit-test" {
		t.Fatalf("runtime metadata = %#v", record.Manifest.Runtime)
	}
	if record.Manifest.Policy.PolicySetID != "policy-manifest" || record.Manifest.Policy.PolicySetHash != "policy-hash" {
		t.Fatalf("policy metadata = %#v", record.Manifest.Policy)
	}
	if record.Manifest.TEB.Name != "sandbox-test" || record.Manifest.TEB.Level != domain.EnforcementPartial {
		t.Fatalf("TEB metadata = %#v", record.Manifest.TEB)
	}
}
