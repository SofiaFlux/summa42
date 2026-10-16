//go:build windows

package repoworkspace

import "os/exec"

// Windows native execution remains UNENFORCED; no subtree-containment claim.
func isolateValidationProcess(cmd *exec.Cmd) {}
