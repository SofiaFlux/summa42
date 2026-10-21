package teb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

type EgressPolicy struct {
	AllowedHosts map[string]struct{}
}

var errEgressDenied = errors.New("target not allowed")

var nonPublicRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicEgressIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, blocked := range nonPublicRanges {
		if blocked.Contains(ip) {
			return false
		}
	}
	return true
}

type lookupIPs func(context.Context, string) ([]netip.Addr, error)
type dialTarget func(context.Context, string, string) (net.Conn, error)

func dialAllowedTarget(ctx context.Context, policy EgressPolicy, target string, lookup lookupIPs, dial dialTarget) (net.Conn, error) {
	if !policy.AllowConnect(target) {
		return nil, errEgressDenied
	}
	host, port, _ := net.SplitHostPort(target)
	if literal, err := netip.ParseAddr(host); err == nil {
		if literal.Zone() != "" {
			return nil, errEgressDenied
		}
		return dial(ctx, "tcp", net.JoinHostPort(literal.Unmap().String(), port))
	}
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("DNS returned no IP addresses")
	}
	for _, ip := range ips {
		if !publicEgressIP(ip) {
			return nil, errEgressDenied
		}
	}
	return dial(ctx, "tcp", net.JoinHostPort(ips[0].Unmap().String(), port))
}

func (p EgressPolicy) AllowConnect(hostport string) bool {
	host, port, err := net.SplitHostPort(strings.TrimSpace(hostport))
	if err != nil || host == "" || port == "" {
		return false
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	host = strings.ToLower(host)
	for allowed := range p.AllowedHosts {
		if strings.ToLower(strings.TrimSpace(allowed)) == host {
			return true
		}
	}
	return false
}

type ProxyEndpoint struct {
	Address string
}

type EgressProxy interface {
	Start(ctx context.Context, policy EgressPolicy) (ProxyEndpoint, error)
	Close() error
}

type ConnectProxy struct {
	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	bindAddr string
}

// NewConnectProxy returns a proxy bound to the loopback interface only.
func NewConnectProxy() *ConnectProxy {
	return &ConnectProxy{bindAddr: "127.0.0.1:0"}
}

// NewConnectProxyOn returns a proxy bound to exactly gatewayAddr (host:port),
// typically the executor network gateway. Wildcard addresses are refused at Start.
func NewConnectProxyOn(gatewayAddr string) *ConnectProxy {
	return &ConnectProxy{bindAddr: gatewayAddr}
}

func (p *ConnectProxy) Start(ctx context.Context, policy EgressPolicy) (ProxyEndpoint, error) {
	if p == nil {
		return ProxyEndpoint{}, errors.New("egress proxy is not configured")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		return ProxyEndpoint{}, errors.New("egress proxy is already started")
	}
	host, _, err := net.SplitHostPort(p.bindAddr)
	if err != nil {
		return ProxyEndpoint{}, fmt.Errorf("egress proxy bind address: %w", err)
	}
	if ip, perr := netip.ParseAddr(host); perr != nil || ip.IsUnspecified() {
		return ProxyEndpoint{}, fmt.Errorf("egress proxy must bind a specific gateway address, got %q", p.bindAddr)
	}
	ln, err := net.Listen("tcp", p.bindAddr)
	if err != nil {
		return ProxyEndpoint{}, err
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleConnect(w, r, policy)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
	}
	p.listener = ln
	p.server = server
	go func() {
		_ = server.Serve(ln)
	}()
	if ctx != nil {
		go func() {
			<-ctx.Done()
			_ = p.Close()
		}()
	}
	return ProxyEndpoint{Address: ln.Addr().String()}, nil
}

func (p *ConnectProxy) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	server := p.server
	listener := p.listener
	p.server = nil
	p.listener = nil
	p.mu.Unlock()
	if server == nil {
		return nil
	}
	err := server.Close()
	if listener != nil {
		_ = listener.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func handleConnect(w http.ResponseWriter, r *http.Request, policy EgressPolicy) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.Host)
	upstream, err := dialAllowedTarget(r.Context(), policy, target,
		func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}, (&net.Dialer{}).DialContext)
	if err != nil {
		if errors.Is(err, errEgressDenied) {
			http.Error(w, "target not allowed", http.StatusForbidden)
			return
		}
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, rw, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	defer client.Close()
	defer upstream.Close()
	if _, err := fmt.Fprint(rw, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := rw.Flush(); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, rw.Reader)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}
