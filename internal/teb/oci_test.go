package teb_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/teb"
)

func TestEnforcedOfflineProfileDeclaresMachineReadableGuarantees(t *testing.T) {
	profile := teb.EnforcedOfflineProfile()
	if profile.Level != domain.EnforcementEnforced {
		t.Fatalf("level = %q, want ENFORCED", profile.Level)
	}
	for _, guarantee := range []teb.Guarantee{
		teb.GuaranteeReadOnlyRoot,
		teb.GuaranteeNonRoot,
		teb.GuaranteeNoNewPrivileges,
		teb.GuaranteeCapabilitiesDropped,
		teb.GuaranteePIDLimit,
		teb.GuaranteeMemoryLimit,
		teb.GuaranteeCPULimit,
		teb.GuaranteeNetworkIsolated,
		teb.GuaranteeWorkspaceOnlyWrite,
		teb.GuaranteeNoAmbientCredentials,
		teb.GuaranteeNoContainerSocket,
	} {
		if !profile.Guarantees[guarantee] {
			t.Fatalf("guarantee %q not asserted by enforced offline profile", guarantee)
		}
	}
}

func TestDockerArgsForEnforcedProfileContainHardeningAndOnlyWorkspaceMount(t *testing.T) {
	workspace := t.TempDir()
	args, err := teb.DockerArgs(teb.OCIRequest{
		Image:     "local/probe:test",
		Workspace: workspace,
		Command:   []string{"literal;not-shell"},
		Profile:   teb.EnforcedOfflineProfile(),
		Limits: teb.ResourceLimits{
			PIDs:        64,
			MemoryBytes: 64 << 20,
			CPUs:        0.5,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\x00")
	for _, want := range []string{
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--user=65532:65532",
		"--pids-limit=64",
		"--memory=67108864",
		"--cpus=0.5",
		"--network=none",
		"type=bind,src=" + workspace + ",dst=/workspace",
		"--workdir=/workspace",
		"local/probe:test",
		"literal;not-shell",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("docker args missing %q: %#v", want, args)
		}
	}
	for _, forbidden := range []string{"/var/run/docker.sock", os.Getenv("HOME"), ".ssh", "AWS_", "AZURE_"} {
		if forbidden != "" && strings.Contains(joined, forbidden) {
			t.Fatalf("docker args contain forbidden host/credential path %q: %#v", forbidden, args)
		}
	}
}

func TestEgressPolicyUsesExactHostAllowlist(t *testing.T) {
	policy := teb.EgressPolicy{AllowedHosts: map[string]struct{}{"api.example.test": {}}}
	if !policy.AllowConnect("api.example.test:443") {
		t.Fatal("exact allowlisted host was rejected")
	}
	for _, target := range []string{
		"evil.api.example.test:443",
		"api.example.test.evil:443",
		"127.0.0.1:443",
		"api.example.test",
		"",
	} {
		if policy.AllowConnect(target) {
			t.Fatalf("non-exact or malformed target %q was allowed", target)
		}
	}
}

func TestConnectProxyAllowsOnlyAllowlistedTargets(t *testing.T) {
	allowed, allowedAddr := startEchoServer(t)
	defer allowed.Close()
	disallowed, disallowedAddr := startEchoServer(t)
	defer disallowed.Close()

	proxy := teb.NewConnectProxy()
	endpoint, err := proxy.Start(context.Background(), teb.EgressPolicy{AllowedHosts: map[string]struct{}{"127.0.0.1": {}}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	_, allowedPort, err := net.SplitHostPort(allowedAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := connectRoundTrip(endpoint.Address, net.JoinHostPort("127.0.0.1", allowedPort)); err != nil {
		t.Fatalf("allowlisted CONNECT failed: %v", err)
	}

	_, disallowedPort, err := net.SplitHostPort(disallowedAddr)
	if err != nil {
		t.Fatal(err)
	}
	if err := connectExpectStatus(endpoint.Address, net.JoinHostPort("localhost", disallowedPort), http.StatusForbidden); err != nil {
		t.Fatalf("disallowed CONNECT was not denied: %v", err)
	}
}

func TestConnectProxyRejectsAllowedHostnameResolvingToLoopback(t *testing.T) {
	proxy := teb.NewConnectProxy()
	endpoint, err := proxy.Start(context.Background(), teb.EgressPolicy{AllowedHosts: map[string]struct{}{"localhost": {}}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if err := connectExpectStatus(endpoint.Address, "localhost:80", http.StatusForbidden); err != nil {
		t.Fatal(err)
	}
}

func TestOCIBypass(t *testing.T) {
	docker := requireDocker(t)
	image := buildLocalProbeImage(t, docker)
	defer exec.Command(docker, "image", "rm", "-f", image).Run()

	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatal(err)
	}
	hostSecret := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(hostSecret, []byte("must-not-be-visible"), 0o600); err != nil {
		t.Fatal(err)
	}

	listener, target := startDockerBridgeReachableFixture(t, docker)
	defer listener.Close()
	verifyBaselineContainerCanReach(t, docker, image, target)

	backend := teb.NewDockerBackend(docker)
	result, err := backend.Run(context.Background(), teb.OCIRequest{
		Image:     image,
		Workspace: workspace,
		Command:   []string{hostSecret, target},
		Profile:   teb.EnforcedOfflineProfile(),
		Limits: teb.ResourceLimits{
			PIDs:        64,
			MemoryBytes: 64 << 20,
			CPUs:        0.5,
		},
	})
	if err != nil {
		t.Fatalf("hardened OCI probe failed: %v; stdout=%s stderr=%s", err, result.Stdout, result.Stderr)
	}

	var got struct {
		WorkspaceWriteOK    bool `json:"workspace_write_ok"`
		OutsideWriteBlocked bool `json:"outside_write_blocked"`
		HostSecretBlocked   bool `json:"host_secret_blocked"`
		NetworkBlocked      bool `json:"network_blocked"`
		DockerSocketBlocked bool `json:"docker_socket_blocked"`
		NonRoot             bool `json:"non_root"`
		NoEffectiveCaps     bool `json:"no_effective_caps"`
		NoNewPrivileges     bool `json:"no_new_privileges"`
		SetuidRootBlocked   bool `json:"setuid_root_blocked"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(result.Stdout)), &got); err != nil {
		t.Fatalf("decode OCI probe output %q: %v", result.Stdout, err)
	}
	if !got.WorkspaceWriteOK || !got.OutsideWriteBlocked || !got.HostSecretBlocked || !got.NetworkBlocked || !got.DockerSocketBlocked || !got.NonRoot || !got.NoEffectiveCaps || !got.NoNewPrivileges || !got.SetuidRootBlocked {
		t.Fatalf("OCI bypass guarantees failed: %#v", got)
	}
}

func startEchoServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := c.Read(buf); err == nil {
					_, _ = c.Write(buf)
				}
			}(conn)
		}
	}()
	return ln, ln.Addr().String()
}

func connectRoundTrip(proxyAddr, target string) error {
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT status = %d", resp.StatusCode)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	got := make([]byte, 4)
	if _, err := reader.Read(got); err != nil {
		return err
	}
	if string(got) != "ping" {
		return fmt.Errorf("echo = %q", got)
	}
	return nil
}

func connectExpectStatus(proxyAddr, target string, want int) error {
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return fmt.Errorf("CONNECT status = %d, want %d", resp.StatusCode, want)
	}
	return nil
}

func requireDocker(t *testing.T) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker CLI unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v: %s", err, output)
	}
	return docker
}

func buildLocalProbeImage(t *testing.T, docker string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.go")
	binary := filepath.Join(dir, "probe")
	if err := os.WriteFile(source, []byte(ociProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build OCI probe: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY probe /probe\nENTRYPOINT [\"/probe\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "summa42-oci-probe:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "build", "--network=none", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build local probe: %v: %s", err, output)
	}
	return tag
}

func startDockerBridgeReachableFixture(t *testing.T, docker string) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, docker, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").CombinedOutput()
	if err != nil {
		ln.Close()
		t.Fatalf("inspect Docker bridge gateway: %v: %s", err, output)
	}
	gateway := strings.TrimSpace(string(output))
	if gateway == "" {
		ln.Close()
		t.Fatal("Docker bridge gateway is empty")
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, net.JoinHostPort(gateway, strconv.Itoa(port))
}

func verifyBaselineContainerCanReach(t *testing.T, docker, image, target string) {
	t.Helper()
	// Hosted runners can need several seconds to cold-start the first container.
	// This timeout bounds infrastructure startup; reachability is still proven by
	// the probe's own exit status once the container is running.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, docker, "run", "--rm", "--network=bridge", image, "--dial-only", target).CombinedOutput()
	if err != nil {
		t.Fatalf("Docker network fixture is not reachable from baseline container: %v: %s", err, output)
	}
}

const ociProbeSource = `package main

import (
    "encoding/json"
    "net"
    "os"
    "strconv"
    "strings"
    "syscall"
    "time"
)

type result struct {
    WorkspaceWriteOK    bool ` + "`json:\"workspace_write_ok\"`" + `
    OutsideWriteBlocked bool ` + "`json:\"outside_write_blocked\"`" + `
    HostSecretBlocked   bool ` + "`json:\"host_secret_blocked\"`" + `
    NetworkBlocked      bool ` + "`json:\"network_blocked\"`" + `
    DockerSocketBlocked bool ` + "`json:\"docker_socket_blocked\"`" + `
    NonRoot             bool ` + "`json:\"non_root\"`" + `
    NoEffectiveCaps     bool ` + "`json:\"no_effective_caps\"`" + `
    NoNewPrivileges     bool ` + "`json:\"no_new_privileges\"`" + `
    SetuidRootBlocked   bool ` + "`json:\"setuid_root_blocked\"`" + `
}

func main() {
    if len(os.Args) == 3 && os.Args[1] == "--dial-only" {
        conn, err := net.DialTimeout("tcp", os.Args[2], time.Second)
        if err != nil { os.Exit(2) }
        conn.Close()
        os.Exit(0)
    }
    if len(os.Args) != 3 { os.Exit(3) }
    hostSecret := os.Args[1]
    target := os.Args[2]
    _, workspaceErr := os.OpenFile("/workspace/probe-write", os.O_CREATE|os.O_WRONLY, 0600)
    outsideErr := os.WriteFile("/escape.txt", []byte("escape"), 0600)
    _, secretErr := os.ReadFile(hostSecret)
    conn, networkErr := net.DialTimeout("tcp", target, 300*time.Millisecond)
    if conn != nil { conn.Close() }
    _, dockerErr := os.Stat("/var/run/docker.sock")
    noCaps, noNewPrivs := procStatus()
    got := result{
        WorkspaceWriteOK: workspaceErr == nil,
        OutsideWriteBlocked: outsideErr != nil,
        HostSecretBlocked: secretErr != nil,
        NetworkBlocked: networkErr != nil,
        DockerSocketBlocked: dockerErr != nil,
        NonRoot: os.Geteuid() != 0,
        NoEffectiveCaps: noCaps,
        NoNewPrivileges: noNewPrivs,
        SetuidRootBlocked: syscall.Setuid(0) != nil,
    }
    _ = json.NewEncoder(os.Stdout).Encode(got)
    if !got.WorkspaceWriteOK || !got.OutsideWriteBlocked || !got.HostSecretBlocked || !got.NetworkBlocked || !got.DockerSocketBlocked || !got.NonRoot || !got.NoEffectiveCaps || !got.NoNewPrivileges || !got.SetuidRootBlocked {
        os.Exit(4)
    }
}

func procStatus() (bool, bool) {
    data, err := os.ReadFile("/proc/self/status")
    if err != nil { return false, false }
    noCaps := false
    noNewPrivs := false
    for _, line := range strings.Split(string(data), "\n") {
        if strings.HasPrefix(line, "CapEff:") {
            raw := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
            value, err := strconv.ParseUint(raw, 16, 64)
            noCaps = err == nil && value == 0
        }
        if strings.HasPrefix(line, "NoNewPrivs:") {
            noNewPrivs = strings.TrimSpace(strings.TrimPrefix(line, "NoNewPrivs:")) == "1"
        }
    }
    return noCaps, noNewPrivs
}
`

func TestDockerArgsRejectsRootWorkspace(t *testing.T) {
	req := teb.OCIRequest{
		Image:     "img",
		Workspace: "/",
		Profile:   teb.EnforcedOfflineProfile(),
		Limits:    teb.ResourceLimits{PIDs: 1, MemoryBytes: 1, CPUs: 1},
	}
	if _, err := teb.DockerArgs(req); err == nil {
		t.Fatal("expected root workspace to be rejected")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	req.Workspace = dir
	if _, err := teb.DockerArgs(req); err == nil {
		t.Fatal("expected workspace containing docker.sock to be rejected")
	}
}

func TestProxyBindsGatewayOnly(t *testing.T) {
	proxy := teb.NewConnectProxyOn("0.0.0.0:0")
	if _, err := proxy.Start(context.Background(), teb.EgressPolicy{}); err == nil {
		t.Fatal("wildcard bind must be refused")
	}
	proxy = teb.NewConnectProxy()
	endpoint, err := proxy.Start(context.Background(), teb.EgressPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	host, _, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("default proxy must bind loopback only, got %q (%v)", endpoint.Address, err)
	}
}
