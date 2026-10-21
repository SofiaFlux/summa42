package acceptance_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/audit"
	"github.com/SofiaFlux/summa42/internal/bootstrap"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/resources"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/testutil"
	"github.com/SofiaFlux/summa42/internal/verification"
)

type diagnosticExecutor struct {
	box   *summa42runtime.Box
	calls int
	child domain.Task
}

func (e *diagnosticExecutor) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	e.calls++
	child, err := e.box.Execution.CreateChildTask(ctx, envelope.TaskID, execution.TaskRequest{
		AcceptanceCriteria:   []string{"deterministic fixture check passes"},
		RequiredCapabilities: []string{"repo.diagnose"},
		RequiredEnforcement:  domain.EnforcementEnforced,
	})
	if err != nil {
		return executors.ExecutionResult{}, err
	}
	e.child = child
	return executors.ExecutionResult{
		ExitCode: 0,
		Evidence: []executors.Evidence{{
			Kind:    executors.EvidenceAgentMessage,
			Content: "proposed diagnostic child task",
		}},
	}, nil
}

func TestUsefulWorkVerticalSlice(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("vertical slice command fixture uses /bin/sh")
	}
	ctx := context.Background()
	root := t.TempDir()
	statePath := filepath.Join(root, "state", "summa42.db")
	evidencePath := filepath.Join(root, "evidence")
	clk := testutil.NewClock(acceptanceNow)

	bootstrapStore, err := state.Open(ctx, statePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := identity.NewLocalEd25519(filepath.Join(root, "keys", "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	cube, err := identity.NewLocalEd25519(filepath.Join(root, "keys", "cube.key"), "cube")
	if err != nil {
		t.Fatal(err)
	}
	constitutionalRoot, err := identity.NewConstitutionalRootForCeremony(filepath.Join(root, "ceremony", "constitutional-root.key"))
	if err != nil {
		t.Fatal(err)
	}
	initResult, err := bootstrap.New(bootstrapStore, clk).Init(ctx, bootstrap.InitRequest{
		Constitution:       []byte(bootstrap.DefaultConstitution),
		Owner:              owner,
		Cube:               cube,
		ConstitutionalRoot: constitutionalRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := bootstrapStore.DB().Close(); err != nil {
		t.Fatal(err)
	}

	commandExecutor, err := executors.NewCommandExecutor(executors.CommandConfig{
		Path: "/bin/sh",
		Args: []string{"-c", `read value < fixture.txt; [ "$value" = repaired ]`},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pol := &mutablePolicy{outcome: domain.PolicyAllow, hash: "policy-v1"}
	agenticExecutor := &diagnosticExecutor{}
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath: statePath,
		EvidencePath: evidencePath,
		Clock: clk,
		CollectiveID: initResult.CollectiveID,
		OwnerPrincipalID: initResult.OwnerPrincipalID,
		PolicyEngine: pol,
		Executors: map[string]executors.Executor{
			"agentic-fake": agenticExecutor,
			"command":      commandExecutor,
		},
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	agenticExecutor.box = box

	now := clk.Now().UTC().Format(time.RFC3339Nano)
	envelopeID := domain.ID("envelope_vertical")
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO resource_envelopes(envelope_id, hard_limit, created_at) VALUES (?, 100, ?)`, envelopeID, now); err != nil {
		t.Fatal(err)
	}

	missionID, err := box.Purpose.CreateMission(ctx, "Repair repository failures without exceeding granted authority")
	if err != nil {
		t.Fatal(err)
	}
	goalID, err := box.Purpose.CreateGoal(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID}, "Return the fixture repository to green")
	if err != nil {
		t.Fatal(err)
	}
	if goalID == "" {
		t.Fatal("goal was not created")
	}

	mainTask, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose: domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria: []string{"fixture repository check passes"},
		RequiredCapabilities: []string{"repo.repair"},
		RequiredEnforcement: domain.EnforcementEnforced,
		AuthorityCeiling: []string{"repo.repair", "repo.diagnose"},
		ResourceEnvelopeID: envelopeID,
		Priority: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	downstream, err := box.Execution.CreateTask(ctx, execution.TaskRequest{
		Purpose: domain.PurposeRef{Kind: domain.PurposeMission, ID: missionID},
		AcceptanceCriteria: []string{"record post-repair success"},
		RequiredCapabilities: []string{"repo.repair"},
		RequiredEnforcement: domain.EnforcementEnforced,
		AuthorityCeiling: []string{"repo.repair"},
		ResourceEnvelopeID: envelopeID,
		Priority: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.Store.DB().ExecContext(ctx,
		`INSERT INTO task_dependencies(task_id, depends_on_task_id) VALUES (?, ?)`, downstream.ID, mainTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Store.DB().ExecContext(ctx,
		`UPDATE tasks SET state = ? WHERE task_id = ?`, domain.TaskCreated, downstream.ID); err != nil {
		t.Fatal(err)
	}

	capacity := scheduler.CapacitySnapshot{Capabilities: map[string]scheduler.CapabilityCapacity{
		"repo.repair": {Accessible: true, Enforcement: domain.EnforcementEnforced},
		"repo.diagnose": {Accessible: true, Enforcement: domain.EnforcementEnforced},
	}}
	candidate, err := box.Scheduler.Next(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil || candidate.Task.ID != mainTask.ID {
		t.Fatalf("scheduler selected %v, want main task %s", candidate, mainTask.ID)
	}
	mainAttempt, err := box.Scheduler.Lease(ctx, mainTask.ID, "agentic-fake")
	if err != nil {
		t.Fatal(err)
	}

	workspace := filepath.Join(root, "fixture-repo")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(workspace, "fixture.txt")
	if err := os.WriteFile(fixturePath, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	precheck, precheckErr := commandExecutor.Start(ctx, executors.AttemptEnvelope{
		TaskID: mainTask.ID, AttemptID: mainAttempt.ID, Objective: "detect fixture failure",
		AcceptanceCriteria: mainTask.AcceptanceCriteria, Workspace: workspace,
		VisibleCapabilities: mainTask.RequiredCapabilities, ResourceEnvelopeID: envelopeID,
	})
	if precheckErr == nil || precheck.ExitCode == 0 {
		t.Fatal("fixture was expected to fail before repair")
	}

	agenticResult, err := box.Executors["agentic-fake"].Start(ctx, executors.AttemptEnvelope{
		TaskID: mainTask.ID, AttemptID: mainAttempt.ID, Objective: "diagnose fixture failure and propose child work",
		AcceptanceCriteria: mainTask.AcceptanceCriteria, Workspace: workspace,
		VisibleCapabilities: mainTask.RequiredCapabilities, ResourceEnvelopeID: envelopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if agenticResult.ExitCode != 0 || len(agenticResult.Evidence) != 1 || agenticResult.Evidence[0].Kind != executors.EvidenceAgentMessage {
		t.Fatalf("agentic executor result=%+v", agenticResult)
	}
	if agenticExecutor.calls != 1 {
		t.Fatalf("agentic executor calls=%d, want 1", agenticExecutor.calls)
	}
	diagnosticTask := agenticExecutor.child
	if diagnosticTask.ID == "" || diagnosticTask.ParentTaskID != mainTask.ID {
		t.Fatalf("agentic executor did not persist child task: %+v", diagnosticTask)
	}

	if err := os.WriteFile(fixturePath, []byte("repaired\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnosticCandidate, err := box.Scheduler.Next(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosticCandidate == nil || diagnosticCandidate.Task.ID != diagnosticTask.ID {
		t.Fatalf("scheduler selected %v, want diagnostic task %s", diagnosticCandidate, diagnosticTask.ID)
	}
	diagnosticAttempt, err := box.Scheduler.Lease(ctx, diagnosticTask.ID, "command")
	if err != nil {
		t.Fatal(err)
	}
	result, err := box.Executors["command"].Start(ctx, executors.AttemptEnvelope{
		TaskID: diagnosticTask.ID, AttemptID: diagnosticAttempt.ID, Objective: "run deterministic fixture acceptance check",
		AcceptanceCriteria: diagnosticTask.AcceptanceCriteria, Workspace: workspace,
		VisibleCapabilities: diagnosticTask.RequiredCapabilities, ResourceEnvelopeID: envelopeID,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("deterministic executor exit=%d err=%v stderr=%q", result.ExitCode, err, result.Stderr)
	}
	if result.Usage.WallTime <= 0 {
		t.Fatalf("executor usage was not measured: %+v", result.Usage)
	}

	diagnosticEvidence := putTextEvidence(t, ctx, box, "fixture check passed", "ATTEMPT_OUTPUT")
	completeAndAccept(t, ctx, box, diagnosticTask.ID, diagnosticAttempt.ID, initResult.OwnerPrincipalID, diagnosticEvidence.ID)

	if next, err := box.Scheduler.Next(ctx, capacity); err != nil {
		t.Fatal(err)
	} else if next != nil {
		t.Fatalf("work unlocked before main acceptance: %s", next.Task.ID)
	}
	var downstreamState domain.TaskState
	if err := box.Store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, downstream.ID).Scan(&downstreamState); err != nil {
		t.Fatal(err)
	}
	if downstreamState != domain.TaskCreated {
		t.Fatalf("downstream state=%s before acceptance, want CREATED", downstreamState)
	}

	reservation, err := reserveInOwnTx(ctx, box, envelopeID, 20, resources.Enforceability{
		CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "deterministic-fixture-budget",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := settleInOwnTx(ctx, box, reservation.ID, 7); err != nil {
		t.Fatal(err)
	}
	available, err := box.Resources.Available(ctx, envelopeID)
	if err != nil {
		t.Fatal(err)
	}
	if available != 93 {
		t.Fatalf("available budget=%d, want 93 after settled cost 7", available)
	}

	mainEvidence := putTextEvidence(t, ctx, box, "repair applied and deterministic acceptance check passed", "ATTEMPT_OUTPUT")
	if _, err := box.Verification.CompleteAttempt(ctx, mainAttempt.ID, verification.CompletionManifest{EvidenceIDs: []domain.ID{mainEvidence.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := box.Store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, downstream.ID).Scan(&downstreamState); err != nil {
		t.Fatal(err)
	}
	if downstreamState != domain.TaskCreated {
		t.Fatalf("completion without acceptance unlocked downstream: %s", downstreamState)
	}
	if _, err := box.Verification.AcceptTask(ctx, mainTask.ID, verification.AcceptanceRequest{
		VerifierID: initResult.OwnerPrincipalID, VerifierType: "OWNER_ACCEPTANCE",
		CriteriaMet: true, EvidenceIDs: []domain.ID{mainEvidence.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := box.Store.DB().QueryRowContext(ctx, `SELECT state FROM tasks WHERE task_id = ?`, downstream.ID).Scan(&downstreamState); err != nil {
		t.Fatal(err)
	}
	if downstreamState != domain.TaskEligible {
		t.Fatalf("accepted dependency did not unlock downstream: %s", downstreamState)
	}

	downstreamCandidate, err := box.Scheduler.Next(ctx, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if downstreamCandidate == nil || downstreamCandidate.Task.ID != downstream.ID {
		t.Fatalf("scheduler selected %v, want downstream %s", downstreamCandidate, downstream.ID)
	}
	downstreamAttempt, err := box.Scheduler.Lease(ctx, downstream.ID, "deterministic-record")
	if err != nil {
		t.Fatal(err)
	}
	downstreamEvidence := putTextEvidence(t, ctx, box, "post-repair success recorded", "ATTEMPT_OUTPUT")
	completeAndAccept(t, ctx, box, downstream.ID, downstreamAttempt.ID, initResult.OwnerPrincipalID, downstreamEvidence.ID)

	decision, err := box.Audit.RecordDecision(ctx, audit.DecisionRecord{
		Trigger: "fixture repair accepted",
		Alternatives: []string{"keep broken fixture", "repair fixture"},
		BasisClass: "VERIFIED_ACCEPTANCE",
		EvidenceIDs: []domain.ID{mainEvidence.ID},
		PolicyDecisionID: domain.ID("policy_decision_vertical"),
		AuthorityIDs: []domain.ID{initResult.OwnerPrincipalID, missionID},
		ExpectedOutcome: "fixture remains green",
		Confidence: 1,
		ResourceEnvelopeID: envelopeID,
		ActorID: initResult.CubePrincipalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.ID == "" {
		t.Fatal("decision record was not persisted")
	}

	var wrongPurpose int
	if err := box.Store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE purpose_kind <> ? OR purpose_id <> ?`, domain.PurposeMission, missionID).Scan(&wrongPurpose); err != nil {
		t.Fatal(err)
	}
	if wrongPurpose != 0 {
		t.Fatalf("%d tasks lost authorized Mission lineage", wrongPurpose)
	}
	var goalPurposeKind string
	var goalPurposeID domain.ID
	if err := box.Store.DB().QueryRowContext(ctx, `SELECT purpose_kind, purpose_id FROM goals WHERE goal_id = ?`, goalID).Scan(&goalPurposeKind, &goalPurposeID); err != nil {
		t.Fatal(err)
	}
	if goalPurposeKind != string(domain.PurposeMission) || goalPurposeID != missionID {
		t.Fatalf("goal lineage=%s/%s, want MISSION/%s", goalPurposeKind, goalPurposeID, missionID)
	}

	pulse, err := box.Wake.StrategicPulse(ctx, capacity, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !pulse.Dormant || pulse.AuthorizedWork || pulse.BlockedTasks != 0 || pulse.KnownUnknowns != 0 {
		t.Fatalf("box did not return DORMANT: %+v", pulse)
	}
}

func putTextEvidence(t *testing.T, ctx context.Context, box *summa42runtime.Box, value, kind string) evidence.EvidenceObject {
	t.Helper()
	object, err := box.Evidence.Put(ctx, strings.NewReader(value), evidence.Metadata{MediaType: "text/plain", Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func completeAndAccept(t *testing.T, ctx context.Context, box *summa42runtime.Box, taskID, attemptID, verifierID, evidenceID domain.ID) {
	t.Helper()
	if _, err := box.Verification.CompleteAttempt(ctx, attemptID, verification.CompletionManifest{EvidenceIDs: []domain.ID{evidenceID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := box.Verification.AcceptTask(ctx, taskID, verification.AcceptanceRequest{
		VerifierID: verifierID, VerifierType: "OWNER_ACCEPTANCE", CriteriaMet: true, EvidenceIDs: []domain.ID{evidenceID},
	}); err != nil {
		t.Fatal(err)
	}
}

func describeCandidate(candidate *scheduler.TaskCandidate) string {
	if candidate == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s", candidate.Task.ID, candidate.Task.State)
}
