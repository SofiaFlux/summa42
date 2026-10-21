package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/capabilities"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server is a primitive with no production caller. It is a transport-facing MCP view over one capability Session.
// It deliberately owns no policy, authority, budget, or operation semantics.
type Server struct {
	inner *sdkmcp.Server
}

func NewServer(session *capabilities.Session, tools []Tool) (*Server, error) {
	if session == nil {
		return nil, errors.New("capability session is required")
	}

	inner := sdkmcp.NewServer(&sdkmcp.Implementation{
		Name:    "summa42-attempt-capabilities",
		Version: "v1",
	}, nil)
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if _, exists := seen[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate MCP tool %q", tool.Name)
		}
		if err := addTool(inner, session, tool); err != nil {
			return nil, err
		}
		seen[tool.Name] = struct{}{}
	}
	return &Server{inner: inner}, nil
}

func (s *Server) Connect(ctx context.Context, transport sdkmcp.Transport) (*sdkmcp.ServerSession, error) {
	if s == nil || s.inner == nil {
		return nil, errors.New("MCP server is not configured")
	}
	if transport == nil {
		return nil, errors.New("MCP transport is required")
	}
	return s.inner.Connect(ctx, transport, nil)
}
