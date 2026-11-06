//go:build realhost

// One real model call through the adapter as the real model user:
//
//	S42_CMD=/opt/summa42-node/bin/cmd go test -tags realhost -run RealHost -v ./internal/ghtriage/climodel
package climodel

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

func init() { realHost = true }

func TestRealHostCommandCodeCall(t *testing.T) {
	binary := os.Getenv("S42_CMD")
	if binary == "" {
		t.Skip("S42_CMD (Command Code binary the model user can run) not set")
	}
	model := os.Getenv("S42_CMD_MODEL")
	if model == "" {
		model = "reflection/beam:free"
	}
	adapter, err := New(Config{RunAsUser: "summa42-model", Binary: binary, CommandCodeModel: model, Timeout: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	answer, err := adapter.run(context.Background(), []byte("Reply with exactly the word PONG and nothing else."))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(string(answer)), "PONG") {
		t.Fatalf("answer = %q", answer)
	}
	t.Logf("answer: %q", answer)
}

func TestRealHostAgyReadTurnFollowedByPlan(t *testing.T) {
	binary := os.Getenv("S42_AGY")
	if binary == "" {
		binary = "/home/topol/.gemini/antigravity-cli/bin/agy"
		if _, err := os.Stat(binary); err != nil {
			t.Skip("S42_AGY or default agy binary not found")
		}
	}
	user := os.Getenv("S42_MODEL_USER")
	if user == "" {
		user = "summa42-model"
	}
	adapter, err := New(Config{
		RunAsUser: user,
		Binary:    binary,
		Profile:   "agy",
		Timeout:   5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Turn 0: Ask agy to read a file
	out1, err := adapter.Plan(context.Background(), ghtriage.PlanInput{
		Schema:   ghtriage.PlanSchemaV2,
		Question: ghtriage.PlanQuestion + "\n\nYou need to inspect the code before planning. Start by requesting to read the repository file 'pkg/example.go' using {\"read_repo_file\": [\"pkg/example.go\"]}.",
		Snapshot: ghtriage.Snapshot{
			Repo:  "SofiaFlux/summa42",
			Issue: 1,
			Title: "Test read turn",
		},
		TrackedFiles: []string{"pkg/example.go"},
	})
	if err != nil {
		t.Fatalf("turn 0 plan call: %v", err)
	}
	if !out1.IsReadRequest() {
		t.Fatalf("expected read request on turn 0, got: %+v", out1)
	}

	// Turn 1: Provide file and ask for final plan
	out2, err := adapter.Plan(context.Background(), ghtriage.PlanInput{
		Schema:   ghtriage.PlanSchemaV2,
		Question: ghtriage.PlanQuestion + "\n\nFinal answer required. Answer now with the final plan JSON object only.",
		Snapshot: ghtriage.Snapshot{
			Repo:  "SofiaFlux/summa42",
			Issue: 1,
			Title: "Test read turn",
		},
		Grounding: &ghtriage.GroundedInput{
			Context: repoworkspace.Snapshot{
				Schema:     repoworkspace.Schema,
				Repository: "SofiaFlux/summa42",
				Commit:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Files: []repoworkspace.File{
					{Path: "pkg/example.go", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Content: "package pkg\n"},
				},
			},
		},
		Turn: 1,
	})
	if err != nil {
		t.Fatalf("turn 1 plan call: %v", err)
	}
	if out2.IsReadRequest() {
		t.Fatalf("expected final plan on turn 1, got read request: %+v", out2)
	}
	if err := out2.Validate(); err != nil {
		t.Fatalf("final plan validation failed: %v", err)
	}
	t.Logf("completed read turn followed by plan successfully: %+v", out2)
}
