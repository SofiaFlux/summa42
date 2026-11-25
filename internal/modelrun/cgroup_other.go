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

func (c *Cmd) setupCgroup(string) (*os.File, error) { return nil, nil }
func (c *Cmd) clearCgroupFD()                       {}
func (c *Cmd) killCgroup(context.Context) error     { return nil }
func (c *Cmd) cleanupCgroup(context.Context) error  { return nil }
