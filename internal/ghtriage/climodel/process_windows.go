//go:build windows

package climodel

import "os/exec"

// isolateProcessTree is a no-op on Windows, which has no POSIX process group
// to isolate or signal. The configured timeout is still enforced against the
// process the adapter starts.
func isolateProcessTree(cmd *exec.Cmd) {}
