package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
)

type compositionPolicy struct{}

func (compositionPolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	return domain.PolicyDecision{
		ID: domain.NewID("policy-decision"), Outcome: domain.PolicyAllow,
		PolicySetID: "policy-test", PolicySetHash: "hash", PolicyCapabilitiesHash: "caps",
		InputDigest: "input", EvaluatedAt: in.Now,
	}, nil
}

type compositionSink struct{}

func (compositionSink) Create(context.Context, fieldfeedback.IssuePayload) (string, int64, error) {
	return "issue-1", 0, nil
}
func (compositionSink) FindByMarker(context.Context, string) (string, bool, error) {
	return "", false, nil
}

var _ operations.Provider = (*fieldfeedback.Provider)(nil)

func TestBoxComposesFieldFeedbackServicesWhenOutboundDisabled(t *testing.T) {
	root := t.TempDir()
	box, err := Open(context.Background(), Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: compositionPolicy{},
		FieldFeedback: localconfig.FieldFeedbackConfig{Enabled: false, Mode: localconfig.FeedbackModeLocalOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if box.Approvals == nil || box.FieldObserver == nil || box.Feedback == nil || box.Sanitizer == nil || box.Experience == nil {
		t.Fatalf("new services not fully composed: approvals=%v observer=%v feedback=%v sanitizer=%v experience=%v",
			box.Approvals != nil, box.FieldObserver != nil, box.Feedback != nil, box.Sanitizer != nil, box.Experience != nil)
	}
	if _, exists := box.Executors["feedback-emitter"]; exists {
		t.Fatal("feedback-emitter registered while outbound feedback disabled")
	}
}

func TestBoxRegistersFeedbackEmitterOnlyWithConfiguredOutboundSink(t *testing.T) {
	root := t.TempDir()
	box, err := Open(context.Background(), Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: compositionPolicy{},
		FieldFeedback: localconfig.FieldFeedbackConfig{
			Enabled: true, Mode: localconfig.FeedbackModeAutoIfAllowed, Provider: "github",
			Destination: "owner/repo", MaintenanceEnvelopeID: "maintenance", RequiredEnforcement: domain.EnforcementEnforced,
		},
		FeedbackSink: compositionSink{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	if _, exists := box.Executors["feedback-emitter"]; !exists {
		t.Fatal("feedback-emitter missing with explicitly configured outbound sink")
	}
}

func TestBoxSanitizerDoesNotExportCallerControlledExecutorMetadata(t *testing.T) {
	root := t.TempDir()
	box, err := Open(context.Background(), Config{
		StatePath: filepath.Join(root, "state.db"), EvidencePath: filepath.Join(root, "evidence"),
		CollectiveID: "collective-1", OwnerPrincipalID: "owner-1", PolicyEngine: compositionPolicy{},
		FieldFeedback: localconfig.FieldFeedbackConfig{Enabled: false, Mode: localconfig.FeedbackModeLocalOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	ctx := context.Background()
	observation, err := box.FieldObserver.Record(ctx, fieldfeedback.ObservationInput{
		Category: "RECOVERY_FRICTION", BasisClass: "TEST", SourceKind: "TEST",
		SummaryLocal: "AcmeBank local-only observation", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := box.Feedback.CreateCandidate(ctx, fieldfeedback.CandidateInput{
		ObservationIDs: []domain.ID{observation.ID}, GenericTaskClass: domain.GenericTaskDebugging,
		Category: "RECOVERY_FRICTION", ExpectedBehavior: "AcmeBank expected", ObservedBehavior: "Phoenix observed",
		StateTransitions: []string{"EXECUTING", "FAILED"}, RuntimeVersion: "AcmeBank-v1",
		ExecutorKind: "AcmeBank", ExecutorVersion: "Phoenix-1", Enforcement: domain.EnforcementEnforced,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, result, err := box.Sanitizer.Sanitize(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.SanitizationPass {
		t.Fatalf("outcome=%s", result.Outcome)
	}
	for _, forbidden := range []string{"AcmeBank", "Phoenix", "runtime_version", "executor_kind", "executor_version"} {
		if strings.Contains(artifact.ContentJSON, forbidden) {
			t.Fatalf("runtime sanitizer leaked caller-controlled metadata %q: %s", forbidden, artifact.ContentJSON)
		}
	}
}
