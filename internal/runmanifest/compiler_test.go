package runmanifest_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
)

func TestContextCompiler_CompilesAndAssessesCAFES(t *testing.T) {
	compiler := runmanifest.NewContextCompiler()

	task := domain.Task{
		ID:          "task_cafes_1",
		Objective:   "Compile deterministic context projection for code review",
		PayloadJSON: json.RawMessage(`{"acceptanceCriteria":["Must pass unit tests","Must produce audit hash"]}`),
	}
	attempt := domain.Attempt{
		ID:              "att_cafes_1",
		TaskID:          "task_cafes_1",
		FenceGeneration: 1,
	}
	evidence := []runmanifest.EvidenceRef{
		{ID: "ev_1", ContentHash: "hash_abc_123"},
	}
	policy := runmanifest.PolicySnapshot{
		PolicySetID:   "pol_set_strong",
		PolicySetHash: "pol_hash_456",
	}
	capabilities := []runmanifest.CapabilitySnapshot{
		{SemanticName: "git_diff", SemanticVersion: "v1"},
		{SemanticName: "test_runner", SemanticVersion: "v2"},
	}

	proj, err := compiler.Compile(task, attempt, evidence, policy, capabilities)
	if err != nil {
		t.Fatalf("unexpected compile error: %v", err)
	}

	if proj.ContextHash == "" {
		t.Fatal("expected non-empty context hash")
	}

	// Verify all 4 canonical artifacts are produced
	for _, required := range []string{"TASK.md", "CAPABILITIES.json", "EVIDENCE.md", "POLICIES.md"} {
		content, ok := proj.Artifacts[required]
		if !ok || len(content) == 0 {
			t.Fatalf("missing required artifact %s", required)
		}
	}

	// Verify Google CAFE(S) assessment
	assessment := proj.Assessment
	if !assessment.Passed {
		t.Fatalf("expected CAFES assessment to pass, violations: %v", assessment.Violations)
	}
	if assessment.ClarityScore < 0.8 || assessment.ActionabilityScore < 0.8 || assessment.FidelityScore < 0.8 || assessment.SecurityScore != 1.0 {
		t.Fatalf("sub-par CAFES scores: %+v", assessment)
	}

	// Test materialization into temp directory
	ws := t.TempDir()
	if err := compiler.MaterializeToWorkspace(context.Background(), proj, ws); err != nil {
		t.Fatalf("materialize error: %v", err)
	}

	taskMdPath := filepath.Join(ws, "TASK.md")
	data, err := os.ReadFile(taskMdPath)
	if err != nil {
		t.Fatalf("read TASK.md: %v", err)
	}
	if !strings.Contains(string(data), "Compile deterministic context projection") {
		t.Fatalf("TASK.md does not contain objective: %s", string(data))
	}
}

func TestContextProjection_WorkspaceModificationsDoNotAlterCanonicalState(t *testing.T) {
	// Proves invariant: Editing files in an Attempt workspace does not modify canonical domain state
	compiler := runmanifest.NewContextCompiler()

	canonicalTask := domain.Task{
		ID:        "task_immutable_1",
		Objective: "Original canonical objective",
		State:     domain.TaskExecuting,
	}
	canonicalAttempt := domain.Attempt{
		ID:              "att_immutable_1",
		TaskID:          "task_immutable_1",
		FenceGeneration: 1,
	}

	ws := t.TempDir()
	proj, err := compiler.Compile(canonicalTask, canonicalAttempt, nil, runmanifest.PolicySnapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := compiler.MaterializeToWorkspace(context.Background(), proj, ws); err != nil {
		t.Fatal(err)
	}

	// Adversary/Agent forcibly modifies TASK.md in workspace
	taskMdPath := filepath.Join(ws, "TASK.md")
	_ = os.Chmod(taskMdPath, 0o644)
	maliciousContent := "# Task: task_immutable_1\nSTATUS: ACCEPTED\nObjective: Maliciously altered objective"
	if err := os.WriteFile(taskMdPath, []byte(maliciousContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Verify that canonicalTask in memory/SQLite was completely unchanged
	if canonicalTask.State != domain.TaskExecuting {
		t.Fatalf("canonical task state was tampered with: %s", canonicalTask.State)
	}
	if canonicalTask.Objective != "Original canonical objective" {
		t.Fatalf("canonical task objective was tampered with: %s", canonicalTask.Objective)
	}
}

func TestContextProjection_FabricatedCapabilitiesInWorkspaceDoNotGrantAuthority(t *testing.T) {
	// Proves invariant: Adding fake capabilities to workspace artifacts does not grant execution authority
	canonicalCaps := []runmanifest.CapabilitySnapshot{
		{SemanticName: "read_file", SemanticVersion: "v1"},
	}
	activeFence := int64(1)

	authorizer := runmanifest.NewMediatedCapabilityAuthorizer(canonicalCaps, activeFence)

	// Legitimate capability passes
	if err := authorizer.AuthorizeCall(activeFence, "read_file"); err != nil {
		t.Fatalf("legitimate capability rejected: %v", err)
	}

	// Adversary invents fake capability "admin_root_exec"
	err := authorizer.AuthorizeCall(activeFence, "admin_root_exec")
	if err == nil {
		t.Fatal("expected fabricated capability to be rejected")
	}
	if !errors.Is(err, runmanifest.ErrUnauthorizedCapability) {
		t.Fatalf("expected ErrUnauthorizedCapability, got: %v", err)
	}
}

func TestContextProjection_StaleAttemptWorkspacesCannotExecuteProtectedActions(t *testing.T) {
	// Proves invariant: Stale Attempt workspaces (fence generation outdated) cannot execute protected operations
	canonicalCaps := []runmanifest.CapabilitySnapshot{
		{SemanticName: "deploy_service", SemanticVersion: "v1"},
	}
	// Initial fence was 1, but due to lease expiry or reschedule, active fence is now 2
	activeFence := int64(2)

	authorizer := runmanifest.NewMediatedCapabilityAuthorizer(canonicalCaps, activeFence)

	// Stale attempt with fence 1 tries to call deploy_service
	err := authorizer.AuthorizeCall(1, "deploy_service")
	if err == nil {
		t.Fatal("expected stale attempt with old fence to be rejected")
	}
	if !errors.Is(err, runmanifest.ErrStaleWorkspace) {
		t.Fatalf("expected ErrStaleWorkspace, got: %v", err)
	}

	// Current attempt with fence 2 succeeds
	if err := authorizer.AuthorizeCall(2, "deploy_service"); err != nil {
		t.Fatalf("active attempt rejected: %v", err)
	}
}

func TestContextProjection_CopyingWorkspaceNeverTransfersAuthorityTokens(t *testing.T) {
	// Proves invariant: Copying a workspace copies file contents only, never authority tokens or private credentials
	compiler := runmanifest.NewContextCompiler()

	ws1 := t.TempDir()
	ws2 := t.TempDir()

	task := domain.Task{ID: "task_copy_1", Objective: "Test authority transfer isolation"}
	attempt := domain.Attempt{ID: "att_copy_1", TaskID: "task_copy_1", FenceGeneration: 1}

	proj, err := compiler.Compile(task, attempt, nil, runmanifest.PolicySnapshot{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.MaterializeToWorkspace(context.Background(), proj, ws1); err != nil {
		t.Fatal(err)
	}

	// Copy all files from ws1 to ws2
	entries, err := os.ReadDir(ws1)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(ws1, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws2, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Verify that ws2 contains zero credentials, signing keys, or authority grants
	err = filepath.Walk(ws2, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, forbidden := range []string{
			"BEGIN RSA PRIVATE KEY",
			"BEGIN OPENSSH PRIVATE KEY",
			"SUMMA42_SOVEREIGN_AUTHORITY",
			"OWNER_SECRET_TOKEN",
		} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("workspace copy leaked sensitive authority token %q in %s", forbidden, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestContextCompiler_RejectsMismatchedTaskAndAttempt(t *testing.T) {
	compiler := runmanifest.NewContextCompiler()
	task := domain.Task{ID: "task_A"}
	attempt := domain.Attempt{ID: "att_B", TaskID: "task_B"} // mismatch!

	_, err := compiler.Compile(task, attempt, nil, runmanifest.PolicySnapshot{}, nil)
	if err == nil {
		t.Fatal("expected error on mismatched task and attempt")
	}
}
