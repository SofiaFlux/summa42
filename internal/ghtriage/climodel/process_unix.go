//go:build !windows

package climodel

import (
	"os/exec"
	"syscall"
)

// isolateProcessTree runs the model in its own process group and makes the
// command's cancellation kill that whole group.
//
// The model binary is a wrapper script or a CLI around a provider SDK, so the
// process exec starts is rarely the one doing the work. The default
// cancellation kills only that direct child, and a grandchild keeps the
// inherited stdout pipe open, so Run blocks on the copy goroutine until the
// grandchild exits on its own: a 200ms timeout against a 5s child took 5s.
// Setting the group and killing the group is what makes the configured timeout
// bound the subtree rather than just the process it happens to start.
func isolateProcessTree(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// The negative pid targets the process group Setpgid created, whose id
		// is the child's pid. SIGKILL matches the default cancellation, which
		// is a kill rather than a request the child could ignore.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
