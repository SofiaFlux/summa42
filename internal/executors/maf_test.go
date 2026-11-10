package executors_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/epistemic"
	"github.com/SofiaFlux/summa42/internal/executors"
)

type mockMediator struct {
	calls []string
	mu    sync.Mutex
}

func (m *mockMediator) InvokeCapability(ctx context.Context, attemptID domain.ID, capability string, payload []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, capability)
	return []byte(`{"status":"success"}`), nil
}

type sampleMAFWorkflow struct {
	executeFunc func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error)
}

func (w *sampleMAFWorkflow) Execute(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
	if w.executeFunc != nil {
		return w.executeFunc(ctx, env, invokeTool)
	}
	return executors.MAFWorkflowResult{
		Status: executors.MAFStatusCompleted,
		Output: "Workflow successfully processed task objective",
	}, nil
}

func TestMAFAdapter_Invariant_WorkflowSuccessNotEqualTaskAcceptance(t *testing.T) {
	// Scenario: MAF workflow internal status completes successfully (COMPLETED),
	// but Meeseek's independent acceptance verifier finds the output does not meet
	// acceptance criteria (e.g. required security audit proof is missing).
	wf := &sampleMAFWorkflow{
		executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
			return executors.MAFWorkflowResult{
				Status: executors.MAFStatusCompleted, // Internal MAF reports success!
				Output: "Claimed all tasks done without evidence",
				Steps: []executors.MAFStepResult{
					{AgentName: "PlannerAgent", Content: "Plan drafted"},
					{AgentName: "CoderAgent", Content: "Code written"},
				},
			}, nil
		},
	}

	adapter := executors.NewMAFAdapter(wf, &mockMediator{})
	envelope := executors.AttemptEnvelope{
		TaskID:              "task_1",
		AttemptID:           "att_1",
		Objective:           "Implement secure audit logging",
		Workspace:           t.TempDir(),
		AcceptanceCriteria:  []string{"Must include verifiable audit signature"},
		VisibleCapabilities: []string{"audit_log"},
	}

	res, err := adapter.Start(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected adapter error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected workflow exit code 0, got %d", res.ExitCode)
	}

	// Now run the independent verification gate
	evaluator := &executors.IndependentTaskAcceptanceEvaluator{}
	accepted, err := evaluator.Evaluate(envelope, res, func(env executors.AttemptEnvelope, r executors.ExecutionResult) (bool, error) {
		// Verifier checks for required criterion: audit signature
		for _, ev := range r.Evidence {
			if ev.Content == "audit_signature_verified" {
				return true, nil
			}
		}
		// Verification fails because required evidence is absent!
		return false, nil
	})

	if err != nil {
		t.Fatalf("evaluator error: %v", err)
	}
	// INVARIANT PROVEN: MAF completed successfully, but Meeseek Task is REJECTED!
	if accepted {
		t.Fatal("invariant violation: MAF internal success erroneously caused task acceptance")
	}
}

func TestMAFAdapter_CancellationCleanlinessWithoutOrphans(t *testing.T) {
	var goroutineRunning int32
	started := make(chan struct{})

	wf := &sampleMAFWorkflow{
		executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
			atomic.StoreInt32(&goroutineRunning, 1)
			close(started)

			// Long-running multi-agent loop respecting ctx
			select {
			case <-ctx.Done():
				atomic.StoreInt32(&goroutineRunning, 0)
				return executors.MAFWorkflowResult{
					Status: executors.MAFStatusCancelled,
				}, ctx.Err()
			case <-time.After(5 * time.Second):
				atomic.StoreInt32(&goroutineRunning, 0)
				return executors.MAFWorkflowResult{Status: executors.MAFStatusCompleted}, nil
			}
		},
	}

	adapter := executors.NewMAFAdapter(wf, &mockMediator{})
	ctx, cancel := context.WithCancel(context.Background())

	envelope := executors.AttemptEnvelope{
		TaskID:    "task_cancel",
		AttemptID: "att_cancel",
		Workspace: t.TempDir(),
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := adapter.Start(ctx, envelope)
		errCh <- err
	}()

	<-started
	// Cancel the Attempt context
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error on cancelled attempt")
	}
	if !errors.Is(err, executors.ErrAttemptCancelled) {
		t.Fatalf("expected ErrAttemptCancelled, got: %v", err)
	}

	// Verify no orphan background process/goroutine is left running
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&goroutineRunning) != 0 {
		t.Fatal("leak detected: background agent loop remained active after cancellation")
	}
}

func TestMAFAdapter_CapabilityMediationEnforcement(t *testing.T) {
	mediator := &mockMediator{}

	t.Run("AllowedVisibleCapabilitySucceeds", func(t *testing.T) {
		wf := &sampleMAFWorkflow{
			executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
				_, err := invokeTool("git_commit", []byte(`{"message":"test"}`))
				if err != nil {
					return executors.MAFWorkflowResult{Status: executors.MAFStatusFailed}, err
				}
				return executors.MAFWorkflowResult{
					Status: executors.MAFStatusCompleted,
					Steps: []executors.MAFStepResult{
						{AgentName: "GitAgent", ToolCalls: []executors.MAFToolCall{{Capability: "git_commit"}}},
					},
				}, nil
			},
		}

		adapter := executors.NewMAFAdapter(wf, mediator)
		envelope := executors.AttemptEnvelope{
			TaskID:              "task_cap",
			AttemptID:           "att_cap",
			Workspace:           t.TempDir(),
			VisibleCapabilities: []string{"git_commit"},
		}

		res, err := adapter.Start(context.Background(), envelope)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("expected exit code 0, got %d", res.ExitCode)
		}
	})

	t.Run("InvisibleCapabilityDenied", func(t *testing.T) {
		wf := &sampleMAFWorkflow{
			executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
				// Agent attempts to invoke capability not in VisibleCapabilities (e.g. direct shell or rm)
				_, err := invokeTool("admin_rm_rf", []byte(`{"path":"/"}`))
				return executors.MAFWorkflowResult{Status: executors.MAFStatusFailed}, err
			},
		}

		adapter := executors.NewMAFAdapter(wf, mediator)
		envelope := executors.AttemptEnvelope{
			TaskID:              "task_denied",
			AttemptID:           "att_denied",
			Workspace:           t.TempDir(),
			VisibleCapabilities: []string{"git_status"}, // admin_rm_rf is NOT visible
		}

		_, err := adapter.Start(context.Background(), envelope)
		if err == nil {
			t.Fatal("expected capability invocation to be denied")
		}
		if !errors.Is(err, executors.ErrCapabilityDenied) {
			t.Fatalf("expected ErrCapabilityDenied, got: %v", err)
		}
	})

	t.Run("AmbientExecutionWithoutMediatorDenied", func(t *testing.T) {
		wf := &sampleMAFWorkflow{
			executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
				_, err := invokeTool("any_cap", []byte(`{}`))
				return executors.MAFWorkflowResult{Status: executors.MAFStatusFailed}, err
			},
		}

		// Adapter created with nil mediator
		adapter := executors.NewMAFAdapter(wf, nil)
		envelope := executors.AttemptEnvelope{
			TaskID:              "task_unmediated",
			AttemptID:           "att_unmediated",
			Workspace:           t.TempDir(),
			VisibleCapabilities: []string{"any_cap"},
		}

		_, err := adapter.Start(context.Background(), envelope)
		if err == nil {
			t.Fatal("expected unmediated tool execution to be denied")
		}
		if !errors.Is(err, executors.ErrCapabilityUnmediated) {
			t.Fatalf("expected ErrCapabilityUnmediated, got: %v", err)
		}
	})
}

func TestMAFAdapter_ProducesProposedKnowledgeDeltaWithoutDirectStateMutation(t *testing.T) {
	delta := &epistemic.KnowledgeDelta{
		DeltaHash: "hash_proposed_delta_123",
		ProposedClaims: []epistemic.Claim{
			{ID: "claim_maf_1", Subject: "MAF", Predicate: "latency_reduced", Object: "15%"},
		},
	}

	wf := &sampleMAFWorkflow{
		executeFunc: func(ctx context.Context, env executors.AttemptEnvelope, invokeTool executors.ToolInvoker) (executors.MAFWorkflowResult, error) {
			return executors.MAFWorkflowResult{
				Status:        executors.MAFStatusCompleted,
				Output:        "Analysis finished",
				ProposedDelta: delta,
			}, nil
		},
	}

	adapter := executors.NewMAFAdapter(wf, &mockMediator{})
	envelope := executors.AttemptEnvelope{
		TaskID:    "task_epistemic",
		AttemptID: "att_epistemic",
		Workspace: t.TempDir(),
	}

	res, err := adapter.Start(context.Background(), envelope)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.ProposedDelta == nil {
		t.Fatal("expected non-nil ProposedDelta")
	}
	if res.ProposedDelta.DeltaHash != "hash_proposed_delta_123" {
		t.Fatalf("unexpected DeltaHash: %s", res.ProposedDelta.DeltaHash)
	}
	if len(res.ProposedDelta.ProposedClaims) != 1 || res.ProposedDelta.ProposedClaims[0].ID != "claim_maf_1" {
		t.Fatalf("unexpected ProposedClaims: %+v", res.ProposedDelta.ProposedClaims)
	}
}
