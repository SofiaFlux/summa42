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
