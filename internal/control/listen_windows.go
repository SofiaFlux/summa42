//go:build windows

package control

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/Microsoft/go-winio"
)

// ownerOnlySDDL grants full access to the object owner only (protected DACL).
const ownerOnlySDDL = "D:P(A;;GA;;;OW)"

func DefaultEndpoint() string {
	return `\\.\pipe\summa42-control`
}

func ListenLocal(pipePath string) (net.Listener, error) {
	if pipePath == "" {
		pipePath = DefaultEndpoint()
	}
	return winio.ListenPipe(pipePath, &winio.PipeConfig{SecurityDescriptor: ownerOnlySDDL})
}

func localHTTPClient(pipePath string) *http.Client {
	if pipePath == "" {
		pipePath = DefaultEndpoint()
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, pipePath)
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}
