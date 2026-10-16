//go:build !windows

package repoworkspace

import (
	"os/exec"
	"syscall"
	"time"
)

func isolateValidationProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
}
