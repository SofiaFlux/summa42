//go:build !windows

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// DefaultEndpoint returns the per-user control socket path, or "" when the
// user config directory is unknown. There is deliberately no /tmp fallback: a
// world-writable directory lets another user squat the endpoint.
func DefaultEndpoint() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "summa42", "control.sock")
	}
	return ""
}

func ListenLocal(socketPath string) (net.Listener, error) {
	if socketPath == "" {
		socketPath = DefaultEndpoint()
	}
	if socketPath == "" {
		return nil, errors.New("control endpoint unavailable: user config directory is unknown and no socket path was given")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("control endpoint exists and is not a Unix socket")
		}

		conn, dialErr := net.DialTimeout("unix", socketPath, 250*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("control endpoint is already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("control endpoint exists but cannot be proven stale: %w", dialErr)
		}
		if !errors.Is(dialErr, os.ErrNotExist) {
			if err := os.Remove(socketPath); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func localHTTPClient(socketPath string) *http.Client {
	if socketPath == "" {
		socketPath = DefaultEndpoint()
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
