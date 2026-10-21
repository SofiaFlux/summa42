package teb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type ResourceLimits struct {
	PIDs        int
	MemoryBytes int64
	CPUs        float64
}

type OCIRequest struct {
	Image       string
	Workspace   string
	Command     []string
	Profile     Profile
	NetworkName string
	Limits      ResourceLimits
}

type OCIResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type DockerBackend struct {
	binary string
}

func NewDockerBackend(binary string) *DockerBackend {
	return &DockerBackend{binary: strings.TrimSpace(binary)}
}

func DockerArgs(request OCIRequest) ([]string, error) {
	request.Image = strings.TrimSpace(request.Image)
	request.Workspace = strings.TrimSpace(request.Workspace)
	request.NetworkName = strings.TrimSpace(request.NetworkName)
	if request.Image == "" {
		return nil, errors.New("OCI image is required")
	}
	if request.Workspace == "" {
		return nil, errors.New("OCI workspace is required")
	}
	if err := request.Profile.Validate(); err != nil {
		return nil, err
	}
	if request.Limits.PIDs <= 0 || request.Limits.MemoryBytes <= 0 || request.Limits.CPUs <= 0 {
		return nil, errors.New("positive PID, memory, and CPU limits are required")
	}
	if request.Profile.Guarantees[GuaranteeModelEgressMediated] && request.NetworkName == "" {
		return nil, errors.New("model-egress-mediated OCI profile requires an isolated network name")
	}
	if request.NetworkName != "" && !request.Profile.Guarantees[GuaranteeModelEgressMediated] {
		return nil, errors.New("OCI network attachment requires a model-egress-mediated profile")
	}
	workspace, err := filepath.Abs(request.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve OCI workspace: %w", err)
	}
	if err := validateWorkspace(workspace); err != nil {
		return nil, err
	}

	args := []string{"run", "--rm"}
	if request.Profile.Guarantees[GuaranteeReadOnlyRoot] {
		args = append(args, "--read-only")
	}
	if request.Profile.Guarantees[GuaranteeCapabilitiesDropped] {
		args = append(args, "--cap-drop=ALL")
	}
	if request.Profile.Guarantees[GuaranteeNoNewPrivileges] {
		args = append(args, "--security-opt=no-new-privileges")
	}
	if request.Profile.Guarantees[GuaranteeNonRoot] {
		args = append(args, "--user=65532:65532")
	}
	if request.Profile.Guarantees[GuaranteePIDLimit] {
		args = append(args, "--pids-limit="+strconv.Itoa(request.Limits.PIDs))
	}
	if request.Profile.Guarantees[GuaranteeMemoryLimit] {
		args = append(args, "--memory="+strconv.FormatInt(request.Limits.MemoryBytes, 10))
	}
	if request.Profile.Guarantees[GuaranteeCPULimit] {
		args = append(args, "--cpus="+strconv.FormatFloat(request.Limits.CPUs, 'f', -1, 64))
	}
	if request.Profile.Guarantees[GuaranteeNetworkIsolated] {
		if request.Profile.Guarantees[GuaranteeModelEgressMediated] {
			args = append(args, "--network="+request.NetworkName)
		} else {
			args = append(args, "--network=none")
		}
	}
	if request.Profile.Guarantees[GuaranteeWorkspaceOnlyWrite] {
		args = append(args,
			"--mount=type=bind,src="+workspace+",dst=/workspace",
			"--workdir=/workspace",
		)
	}
	args = append(args, request.Image)
	args = append(args, request.Command...)
	return args, nil
}

func (b *DockerBackend) Run(ctx context.Context, request OCIRequest) (OCIResult, error) {
	if b == nil || b.binary == "" {
		return OCIResult{}, errors.New("Docker backend is not configured")
	}
	args, err := DockerArgs(request)
	if err != nil {
		return OCIResult{}, err
	}
	cmd := exec.CommandContext(ctx, b.binary, args...)
	cmd.Env = []string{}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	result := OCIResult{ExitCode: dockerExitCode(err), Stdout: stdout.String(), Stderr: stderr.String()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		return result, fmt.Errorf("Docker OCI execution failed: %w", err)
	}
	return result, nil
}

func dockerExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// validateWorkspace refuses a bind-mount source that would hand the container
// the host root or a container-runtime socket.
func validateWorkspace(workspace string) error {
	if filepath.Dir(workspace) == workspace {
		return errors.New("OCI workspace must not be the filesystem root")
	}
	var found string
	_ = filepath.WalkDir(workspace, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.Name() == "docker.sock" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found != "" {
		return fmt.Errorf("OCI workspace contains a container socket: %s", found)
	}
	return nil
}
