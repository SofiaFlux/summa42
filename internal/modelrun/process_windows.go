//go:build windows

package modelrun

import "os/exec"

// isolateProcessTree is a no-op on Windows; WaitDelay still bounds the call.
func isolateProcessTree(cmd *exec.Cmd) {}
