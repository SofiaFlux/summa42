package teb

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// DockerProvider adapts DockerBackend to the provider-neutral TEBProvider interface.
type DockerProvider struct {
	backend *DockerBackend
}

// NewDockerProvider creates a new DockerProvider using the specified docker binary.
func NewDockerProvider(binary string) *DockerProvider {
	return &DockerProvider{
		backend: NewDockerBackend(binary),
	}
}

// Name returns the provider identifier.
func (p *DockerProvider) Name() string {
	return "docker-oci"
}

// Available checks whether the docker CLI and daemon are operational.
func (p *DockerProvider) Available(ctx context.Context) bool {
	if p == nil || p.backend == nil || p.backend.binary == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, p.backend.binary, "info", "--format", "{{.ServerVersion}}")
	return cmd.Run() == nil
}

// Assess validates whether the request satisfies the profile prerequisites.
func (p *DockerProvider) Assess(req ExecutionRequest) (Profile, error) {
	if err := req.Profile.Validate(); err != nil {
		return Profile{}, err
	}
	ociReq := OCIRequest{
		Image:       req.Image,
		Workspace:   req.Workspace,
		Command:     req.Command,
		Profile:     req.Profile,
		NetworkName: req.NetworkName,
		Limits:      req.Limits,
	}
	if _, err := DockerArgs(ociReq); err != nil {
		return Profile{}, err
	}
	return req.Profile, nil
}

// Execute runs the container via the Docker backend and collects verified evidence.
func (p *DockerProvider) Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error) {
	if p == nil || p.backend == nil {
		return ExecutionResult{}, ErrProviderUnavailable
	}

	ociReq := OCIRequest{
		Image:       req.Image,
		Workspace:   req.Workspace,
		Command:     req.Command,
		Profile:     req.Profile,
		NetworkName: req.NetworkName,
		Limits:      req.Limits,
	}

	result, err := p.backend.Run(ctx, ociReq)

	var evidence []string
	evidence = append(evidence, fmt.Sprintf("docker_exit_code:%d", result.ExitCode))
	if req.Profile.Guarantees[GuaranteeReadOnlyRoot] {
		evidence = append(evidence, "flag:--read-only")
	}
	if req.Profile.Guarantees[GuaranteeCapabilitiesDropped] {
		evidence = append(evidence, "flag:--cap-drop=ALL")
	}
	if req.Profile.Guarantees[GuaranteeNoNewPrivileges] {
		evidence = append(evidence, "flag:--security-opt=no-new-privileges")
	}
	if req.Profile.Guarantees[GuaranteeNonRoot] {
		evidence = append(evidence, "flag:--user=65532:65532")
	}
	if req.Profile.Guarantees[GuaranteeNetworkIsolated] {
		if req.NetworkName != "" {
			evidence = append(evidence, "flag:--network="+req.NetworkName)
		} else {
			evidence = append(evidence, "flag:--network=none")
		}
	}
	if req.Profile.Guarantees[GuaranteeWorkspaceOnlyWrite] {
		evidence = append(evidence, "mount:type=bind,dst=/workspace")
	}

	evidenceHash := ComputeEvidenceHash(p.Name(), req.Profile.Level, req.Profile.Guarantees, evidence)

	level := req.Profile.Level
	if err != nil && result.ExitCode == -1 {
		// Complete invocation failure degrades to unenforced
		level = domain.EnforcementUnenforced
	}

	return ExecutionResult{
		ExitCode:         result.ExitCode,
		Stdout:           strings.TrimSpace(result.Stdout),
		Stderr:           strings.TrimSpace(result.Stderr),
		EnforcementLevel: level,
		Guarantees:       req.Profile.Guarantees,
		Evidence:         evidence,
		ProviderName:     p.Name(),
		EvidenceHash:     evidenceHash,
	}, err
}
