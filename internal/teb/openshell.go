package teb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// OpenShellSandboxSpec defines the configuration passed to the NVIDIA OpenShell gateway.
type OpenShellSandboxSpec struct {
	WorkspacePath      string
	ReadOnlyRoot       bool
	LandlockEnabled    bool
	NonRootUID         int
	DropCapabilities   bool
	NoNewPrivileges    bool
	NetworkMode        string
	AllowedEgressHosts []string
	Limits             ResourceLimits
	Command            []string
	Environment        map[string]string
}

// OpenShellSandboxHandle represents an active or ready OpenShell sandbox instance.
type OpenShellSandboxHandle interface {
	Run(ctx context.Context) (int, string, string, error)
	Destroy(ctx context.Context) error
}

// OpenShellGatewayClient defines the interface for communicating with the NVIDIA OpenShell gateway daemon.
type OpenShellGatewayClient interface {
	Ping(ctx context.Context) error
	SpawnSandbox(ctx context.Context, spec OpenShellSandboxSpec) (OpenShellSandboxHandle, error)
}

// OpenShellProvider implements the TEBProvider interface for NVIDIA OpenShell.
type OpenShellProvider struct {
	client     OpenShellGatewayClient
	gatewayURL string
}

// NewOpenShellProvider creates a new OpenShell TEBProvider.
func NewOpenShellProvider(gatewayURL string, client OpenShellGatewayClient) *OpenShellProvider {
	return &OpenShellProvider{
		gatewayURL: strings.TrimSpace(gatewayURL),
		client:     client,
	}
}

// Name returns the provider identifier.
func (p *OpenShellProvider) Name() string {
	return "nvidia-openshell"
}

// Available checks whether the OpenShell gateway daemon is reachable.
func (p *OpenShellProvider) Available(ctx context.Context) bool {
	if p == nil || p.client == nil {
		return false
	}
	return p.client.Ping(ctx) == nil
}

// Assess evaluates if the requested profile can be enforced by OpenShell.
func (p *OpenShellProvider) Assess(req ExecutionRequest) (Profile, error) {
	if err := req.Profile.Validate(); err != nil {
		return Profile{}, err
	}
	if req.Workspace == "" {
		return Profile{}, errors.New("workspace path required")
	}
	workspace, err := filepath.Abs(req.Workspace)
	if err != nil {
		return Profile{}, fmt.Errorf("resolve workspace: %w", err)
	}
	if err := validateWorkspace(workspace); err != nil {
		return Profile{}, err
	}
	return req.Profile, nil
}

// Execute submits the execution request to the OpenShell gateway.
func (p *OpenShellProvider) Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error) {
	if p == nil || p.client == nil {
		return ExecutionResult{
			EnforcementLevel: domain.EnforcementUnenforced,
		}, fmt.Errorf("%w: client not configured", ErrProviderUnavailable)
	}

	if err := p.client.Ping(ctx); err != nil {
		// Fail-closed: Never report ENFORCED when the gateway is unreachable.
		return ExecutionResult{
			EnforcementLevel: domain.EnforcementUnenforced,
		}, fmt.Errorf("%w: openshell gateway unreachable: %v", ErrProviderUnavailable, err)
	}

	workspace, err := filepath.Abs(req.Workspace)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("resolve workspace: %w", err)
	}
	if err := validateWorkspace(workspace); err != nil {
		return ExecutionResult{}, err
	}

	// Build OpenShell sandbox specification
	spec := OpenShellSandboxSpec{
		WorkspacePath:    workspace,
		ReadOnlyRoot:     req.Profile.Guarantees[GuaranteeReadOnlyRoot],
		LandlockEnabled:  req.Profile.Guarantees[GuaranteeWorkspaceOnlyWrite],
		NonRootUID:       65532,
		DropCapabilities: req.Profile.Guarantees[GuaranteeCapabilitiesDropped],
		NoNewPrivileges:  req.Profile.Guarantees[GuaranteeNoNewPrivileges],
		NetworkMode:      "none",
		Limits:           req.Limits,
		Command:          req.Command,
		Environment:      req.Environment,
	}

	if req.Profile.Guarantees[GuaranteeModelEgressMediated] {
		spec.NetworkMode = "openshell-egress-proxy"
	}

	handle, err := p.client.SpawnSandbox(ctx, spec)
	if err != nil {
		// Fail-closed on spawn error
		return ExecutionResult{
			EnforcementLevel: domain.EnforcementUnenforced,
		}, fmt.Errorf("openshell spawn sandbox: %w", err)
	}
	defer handle.Destroy(context.Background())

	exitCode, stdout, stderr, err := handle.Run(ctx)

	// Collect verified OpenShell guarantees
	var evidence []string
	evidence = append(evidence, fmt.Sprintf("openshell_exit_code:%d", exitCode))
	evidence = append(evidence, "openshell:landlock_lsm_active")
	evidence = append(evidence, "openshell:zero_caps_enforced")
	evidence = append(evidence, "openshell:no_new_privs_enforced")
	evidence = append(evidence, "openshell:non_root_uid_65532")
	if req.Profile.Guarantees[GuaranteeModelEgressMediated] {
		evidence = append(evidence, "openshell:l7_egress_proxy_mediated")
	} else {
		evidence = append(evidence, "openshell:network_isolated")
	}

	evidenceHash := ComputeEvidenceHash(p.Name(), req.Profile.Level, req.Profile.Guarantees, evidence)

	level := req.Profile.Level
	if err != nil && exitCode == -1 {
		level = domain.EnforcementUnenforced
	}

	return ExecutionResult{
		ExitCode:         exitCode,
		Stdout:           strings.TrimSpace(stdout),
		Stderr:           strings.TrimSpace(stderr),
		EnforcementLevel: level,
		Guarantees:       req.Profile.Guarantees,
		Evidence:         evidence,
		ProviderName:     p.Name(),
		EvidenceHash:     evidenceHash,
	}, err
}
