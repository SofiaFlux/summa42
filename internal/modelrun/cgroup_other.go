//go:build !linux

package modelrun

import (
	"context"
	"os"
)

// CgroupTestConfig configures fake cgroup behavior in unit tests.
type CgroupTestConfig struct {
	Root        string
	SelfRelPath string
	ValidMagic  bool
	UseKernelFD bool
	MkdirErr    error
	OnStart     func(callDir string)
}

// SetCgroupForTest is a no-op on non-Linux hosts.
func SetCgroupForTest(CgroupTestConfig) func() { return func() {} }

func (c *Cmd) setupCgroup(string) (*os.File, error)       { return nil, nil }
func (c *Cmd) clearCgroupFD()                             {}
func killCgroupDir(context.Context, string, string) error { return nil }
func cleanupCgroupDir(context.Context, string, string, int64) error {
	return nil
}
