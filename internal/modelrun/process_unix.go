//go:build !windows

package modelrun

import (
	"os/exec"
	"syscall"
)

// isolateProcessTree puts the launcher in its own process group and makes
// cancellation a best-effort SIGTERM to sudo, which relays it to the model.
//
// The model runs as another uid, so the Box cannot signal its processes: a
// group SIGKILL would be EPERM. The real bound is the timeout command on the
// model user's side (it kills its own group as that user) and cmd.WaitDelay,
// which abandons a launcher that ignores the signal.
func isolateProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return cmd.Process.Signal(syscall.SIGTERM)
	}
}
