//go:build linux

package modelrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const cgroup2SuperMagic = 0x63677270

// CgroupTestConfig configures fake cgroup behavior in unit tests.
type CgroupTestConfig struct {
	Root        string
	SelfRelPath string
	ValidMagic  bool
	UseKernelFD bool
	MkdirErr    error
	OnStart     func(callDir string)
}

type cgroupRuntime struct {
	active      bool
	root        string
	selfRelPath string
	validMagic  bool
	useKernelFD bool
	mkdirErr    error
	onStart     func(callDir string)
}

var cgroupCfg = cgroupRuntime{
	root:        "/sys/fs/cgroup",
	useKernelFD: true,
}

// SetCgroupForTest replaces Linux cgroup detection for unit tests and returns
// the restore function.
func SetCgroupForTest(cfg CgroupTestConfig) func() {
	old := cgroupCfg
	cgroupCfg = cgroupRuntime{
		active:      true,
		root:        cfg.Root,
		selfRelPath: cfg.SelfRelPath,
		validMagic:  cfg.ValidMagic,
		useKernelFD: cfg.UseKernelFD,
		mkdirErr:    cfg.MkdirErr,
		onStart:     cfg.OnStart,
	}
	return func() { cgroupCfg = old }
}

func (c *Cmd) setupCgroup(callID string) (*os.File, error) {
	boxDir, useKernelFD, err := detectWritableBoxCgroup()
	if err != nil {
		return nil, err
	}
	if cgroupCfg.active && cgroupCfg.mkdirErr != nil {
		return nil, cgroupCfg.mkdirErr
	}
	callDir := filepath.Join(boxDir, "call-"+callID)
	if err := os.Mkdir(callDir, 0o755); err != nil {
		return nil, err
	}
	if cgroupCfg.active && cgroupCfg.onStart != nil {
		cgroupCfg.onStart(callDir)
	}
	var fdFile *os.File
	if useKernelFD {
		f, err := os.Open(callDir)
		if err != nil {
			_ = os.Remove(callDir)
			return nil, err
		}
		if c.Cmd.SysProcAttr == nil {
			c.Cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		c.Cmd.SysProcAttr.UseCgroupFD = true
		c.Cmd.SysProcAttr.CgroupFD = int(f.Fd())
		fdFile = f
	}
	c.cgroupDir = callDir
	return fdFile, nil
}

func (c *Cmd) clearCgroupFD() {
	if c.Cmd.SysProcAttr != nil {
		c.Cmd.SysProcAttr.UseCgroupFD = false
		c.Cmd.SysProcAttr.CgroupFD = 0
	}
	if c.cgroupDir != "" {
		_ = os.Remove(c.cgroupDir)
		c.cgroupDir = ""
	}
}

func detectWritableBoxCgroup() (boxDir string, useKernelFD bool, err error) {
	if cgroupCfg.active {
		if !cgroupCfg.validMagic || cgroupCfg.root == "" {
			return "", false, errors.New("cgroup v2 unavailable")
		}
		rel := strings.TrimPrefix(cgroupCfg.selfRelPath, "/")
		dir := filepath.Join(cgroupCfg.root, filepath.FromSlash(rel))
		return dir, cgroupCfg.useKernelFD, nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs("/sys/fs/cgroup", &st); err != nil || uint64(st.Type) != cgroup2SuperMagic {
		return "", false, errors.New("cgroup v2 not mounted at /sys/fs/cgroup")
	}
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", false, err
	}
	var rel string
	for _, line := range strings.Split(string(raw), "\n") {
		if after, ok := strings.CutPrefix(line, "0::"); ok {
			rel = strings.TrimSpace(after)
			break
		}
	}
	if rel == "" {
		return "", false, errors.New("no unified cgroup v2 entry in /proc/self/cgroup")
	}
	dir := filepath.Join("/sys/fs/cgroup", filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	probe, err := os.MkdirTemp(dir, ".s42-probe-")
	if err != nil {
		return "", false, err
	}
	_ = os.Remove(probe)
	return dir, true, nil
}

func (c *Cmd) killCgroup(ctx context.Context) error {
	if c.cgroupDir == "" {
		return nil
	}
	killPath := filepath.Join(c.cgroupDir, "cgroup.kill")
	f, err := os.OpenFile(killPath, os.O_WRONLY, 0)
	if err == nil {
		_, writeErr := f.WriteString("1")
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Kernels < 5.14 lack cgroup.kill: signal cgroup PIDs via sudo as the model user.
	pids, err := liveCgroupPIDs(c.cgroupDir)
	if err != nil || len(pids) == 0 {
		return err
	}
	args := []string{"-KILL", "--"}
	for _, p := range pids {
		args = append(args, strconv.Itoa(p))
	}
	argv := append(launchPrefix(c.runAs, nil), append([]string{killBinary}, args...)...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=" + defaultPATH}
	cmd.Dir = "/"
	_ = cmd.Run()
	return nil
}

func (c *Cmd) cleanupCgroup(ctx context.Context) error {
	if c.cgroupDir == "" {
		return nil
	}
	_ = c.killCgroup(ctx)
	if pgid := c.pgid.Load(); pgid > 1 {
		_ = sendGroupSignal(ctx, c.runAs, "KILL", pgid)
	}
	var alive []int
	for i := 0; i < 25; i++ {
		var err error
		alive, err = liveCgroupPIDs(c.cgroupDir)
		if err != nil && errors.Is(err, os.ErrNotExist) {
			c.cgroupDir = ""
			return nil
		}
		if len(alive) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
		_ = c.killCgroup(ctx)
	}
	if len(alive) > 0 {
		return fmt.Errorf("%w: pids %v in cgroup %s", ErrSurvivorDetected, alive, c.cgroupDir)
	}
	removeFn := os.Remove
	if cgroupCfg.active {
		// Fake cgroup dirs on a regular filesystem contain test files (cgroup.kill, cgroup.procs).
		removeFn = os.RemoveAll
	}
	if err := removeFn(c.cgroupDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("modelrun: remove cgroup %s: %v", c.cgroupDir, err)
	}
	c.cgroupDir = ""
	return nil
}

func liveCgroupPIDs(cgroupDir string) ([]int, error) {
	raw, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.procs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var alive []int
	for _, field := range strings.Fields(string(raw)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			continue
		}
		if pidAliveLinux(pid) {
			alive = append(alive, pid)
		}
	}
	return alive, nil
}

func pidAliveLinux(pid int) bool {
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := bytes.LastIndexByte(raw, ')'); i >= 0 && len(raw) > i+2 {
			if state := raw[i+2]; state == 'Z' || state == 'X' {
				return false
			}
		}
		return true
	} else if errors.Is(err, os.ErrNotExist) {
		if _, procErr := os.Stat("/proc/self/stat"); procErr == nil {
			return false
		}
	}
	return true
}
