//go:build !windows

package control

import (
	"path/filepath"
	"testing"
)

func TestListenLocalRefusesToReplaceActiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	first, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := ListenLocal(path)
	if err == nil {
		_ = second.Close()
		t.Fatal("second listener replaced an active control socket")
	}
}

func TestNoTmpSocketFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got := DefaultEndpoint(); got != "" {
		t.Fatalf("DefaultEndpoint() = %q, want empty without a config dir", got)
	}
	if l, err := ListenLocal(""); err == nil {
		_ = l.Close()
		t.Fatal("ListenLocal must refuse when no per-user endpoint is available")
	}
}
