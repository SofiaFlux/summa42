package teb_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/teb"
)

func TestOCIEnforcedProxyTopologyHasNoDirectProviderRoute(t *testing.T) {
	docker := requireDocker(t)
	image := buildEgressProbeImage(t, docker)
	defer exec.Command(docker, "image", "rm", "-f", image).Run()

	executorNetwork, executorGateway := createDockerNetwork(t, docker, true)
	defer exec.Command(docker, "network", "rm", executorNetwork).Run()
	providerNetwork, _ := createDockerNetwork(t, docker, false)
	defer exec.Command(docker, "network", "rm", providerNetwork).Run()

	providerName, providerTarget := startFixtureContainer(t, docker, image, providerNetwork)
	defer exec.Command(docker, "rm", "-f", providerName).Run()
	disallowedName, disallowedTarget := startFixtureContainer(t, docker, image, providerNetwork)
	defer exec.Command(docker, "rm", "-f", disallowedName).Run()

	providerHost, _, err := net.SplitHostPort(providerTarget)
	if err != nil {
		t.Fatal(err)
	}
	proxy := teb.NewConnectProxyOn(net.JoinHostPort(executorGateway, "0"))
	endpoint, err := proxy.Start(context.Background(), teb.EgressPolicy{AllowedHosts: map[string]struct{}{providerHost: {}}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	_, proxyPort, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		t.Fatal(err)
	}
	proxyFromExecutor := net.JoinHostPort(executorGateway, proxyPort)

	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o777); err != nil {
		t.Fatal(err)
	}
	backend := teb.NewDockerBackend(docker)
	base := teb.OCIRequest{
		Image:       image,
		Workspace:   workspace,
		Profile:     teb.EnforcedProxyProfile(),
		NetworkName: executorNetwork,
		Limits: teb.ResourceLimits{
			PIDs:        64,
			MemoryBytes: 64 << 20,
			CPUs:        0.5,
		},
	}

	direct := base
	direct.Command = []string{"--dial-expect-fail", providerTarget}
	if result, err := backend.Run(context.Background(), direct); err != nil {
		t.Fatalf("direct-route isolation probe failed: %v; stdout=%s stderr=%s", err, result.Stdout, result.Stderr)
	}

	allowed := base
	allowed.Command = []string{"--proxy-allowed", proxyFromExecutor, providerTarget}
	if result, err := backend.Run(context.Background(), allowed); err != nil {
		t.Fatalf("allowlisted proxy route failed: %v; stdout=%s stderr=%s", err, result.Stdout, result.Stderr)
	}

	denied := base
	denied.Command = []string{"--proxy-denied", proxyFromExecutor, disallowedTarget}
	if result, err := backend.Run(context.Background(), denied); err != nil {
		t.Fatalf("non-allowlisted proxy route was not cleanly denied: %v; stdout=%s stderr=%s", err, result.Stdout, result.Stderr)
	}
}

func createDockerNetwork(t *testing.T, docker string, internal bool) (string, string) {
	t.Helper()
	name := "summa42-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{"network", "create"}
	if internal {
		args = append(args, "--internal")
	}
	args = append(args, name)
	if output, err := exec.Command(docker, args...).CombinedOutput(); err != nil {
		t.Fatalf("create Docker network: %v: %s", err, output)
	}
	output, err := exec.Command(docker, "network", "inspect", name, "--format", "{{(index .IPAM.Config 0).Gateway}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect Docker network gateway: %v: %s", err, output)
	}
	gateway := strings.TrimSpace(string(output))
	if gateway == "" {
		t.Fatal("Docker network gateway is empty")
	}
	return name, gateway
}

func startFixtureContainer(t *testing.T, docker, image, network string) (string, string) {
	t.Helper()
	name := "summa42-fixture-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	output, err := exec.Command(docker, "run", "-d", "--rm", "--network="+network, "--name="+name, image, "--serve").CombinedOutput()
	if err != nil {
		t.Fatalf("start provider fixture: %v: %s", err, output)
	}
	ipOutput, err := exec.Command(docker, "inspect", name, "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect provider fixture: %v: %s", err, ipOutput)
	}
	ip := strings.TrimSpace(string(ipOutput))
	if ip == "" {
		t.Fatal("provider fixture IP is empty")
	}
	target := net.JoinHostPort(ip, "8080")
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", target, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider fixture %s not reachable from Box host: %v", target, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return name, target
}

func buildEgressProbeImage(t *testing.T, docker string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.go")
	binary := filepath.Join(dir, "probe")
	if err := os.WriteFile(source, []byte(egressProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build egress probe: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY probe /probe\nENTRYPOINT [\"/probe\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tag := "summa42-egress-probe:" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, docker, "build", "--network=none", "-t", tag, dir).CombinedOutput(); err != nil {
		t.Fatalf("docker build egress probe: %v: %s", err, output)
	}
	return tag
}

const egressProbeSource = `package main

import (
    "bufio"
    "fmt"
    "net"
    "net/http"
    "os"
    "time"
)

func main() {
    if len(os.Args) < 2 { os.Exit(2) }
    switch os.Args[1] {
    case "--serve": serve()
    case "--dial-expect-fail":
        if len(os.Args) != 3 { os.Exit(3) }
        conn, err := net.DialTimeout("tcp", os.Args[2], 400*time.Millisecond)
        if conn != nil { conn.Close() }
        if err == nil { os.Exit(4) }
    case "--proxy-allowed":
        if len(os.Args) != 4 { os.Exit(3) }
        if err := proxyCheck(os.Args[2], os.Args[3], http.StatusOK, true); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(5) }
    case "--proxy-denied":
        if len(os.Args) != 4 { os.Exit(3) }
        if err := proxyCheck(os.Args[2], os.Args[3], http.StatusForbidden, false); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(6) }
    default:
        os.Exit(7)
    }
}

func serve() {
    ln, err := net.Listen("tcp", ":8080")
    if err != nil { os.Exit(10) }
    for {
        conn, err := ln.Accept()
        if err != nil { os.Exit(11) }
        go func(c net.Conn) {
            defer c.Close()
            buf := make([]byte, 4)
            if _, err := c.Read(buf); err == nil { _, _ = c.Write(buf) }
        }(conn)
    }
}

func proxyCheck(proxyAddr, target string, wantStatus int, echo bool) error {
    conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
    if err != nil { return err }
    defer conn.Close()
    if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil { return err }
    reader := bufio.NewReader(conn)
    resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
    if err != nil { return err }
    resp.Body.Close()
    if resp.StatusCode != wantStatus { return fmt.Errorf("status=%d want=%d", resp.StatusCode, wantStatus) }
    if !echo { return nil }
    if _, err := conn.Write([]byte("ping")); err != nil { return err }
    got := make([]byte, 4)
    if _, err := reader.Read(got); err != nil { return err }
    if string(got) != "ping" { return fmt.Errorf("echo=%q", got) }
    return nil
}
`
