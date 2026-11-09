package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/SofiaFlux/summa42/internal/delegation"
)

var (
	ErrTransportAlreadyRegistered = errors.New("transport protocol already registered")
)

// BoundaryGateway manages inter-collective transports and isolates internal execution
// and authority evaluation from external transport protocols.
type BoundaryGateway struct {
	transports map[string]InterCollectiveTransport
	sanitizer  *GatewaySanitizer
	mu         sync.RWMutex
}

func NewBoundaryGateway(validator *delegation.Validator) *BoundaryGateway {
	if validator == nil {
		validator = delegation.NewValidator(delegation.MapPublicKeyResolver{})
	}
	return &BoundaryGateway{
		transports: make(map[string]InterCollectiveTransport),
		sanitizer:  NewGatewaySanitizer(validator),
	}
}

// RegisterTransport registers a replaceable transport adapter (e.g. A2A, SLIM, HTTPS, MCP).
// It rejects duplicate registrations to avoid accidental transport hijacking.
func (g *BoundaryGateway) RegisterTransport(transport InterCollectiveTransport) error {
	if transport == nil {
		return errors.New("transport cannot be nil")
	}
	name := transport.ProtocolName()
	if name == "" {
		return errors.New("transport protocol name cannot be empty")
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, exists := g.transports[name]; exists {
		return fmt.Errorf("%w: %s", ErrTransportAlreadyRegistered, name)
	}
	g.transports[name] = transport
	return nil
}

// SendMessage dispatches an outbound message via the designated transport adapter,
// verifies origin bindings, and sanitizes the inbound response before returning it to the caller.
func (g *BoundaryGateway) SendMessage(ctx context.Context, protocol string, msg OutboundMessage) (InboundMessage, error) {
	if err := ctx.Err(); err != nil {
		return InboundMessage{}, err
	}

	g.mu.RLock()
	transport, ok := g.transports[protocol]
	g.mu.RUnlock()

	if !ok {
		return InboundMessage{}, fmt.Errorf("unsupported transport protocol %q", protocol)
	}

	// Dispatch through transport
	resp, err := transport.SendMessage(ctx, msg)
	if err != nil {
		return InboundMessage{}, err
	}

	// Verify protocol & origin binding
	if resp.TransportProtocol != protocol {
		return InboundMessage{}, fmt.Errorf("response protocol %s does not match requested protocol %s", resp.TransportProtocol, protocol)
	}
	if msg.TargetCollectiveID != "" && resp.OriginCollectiveID != msg.TargetCollectiveID {
		return InboundMessage{}, fmt.Errorf("response origin collective %s does not match target %s", resp.OriginCollectiveID, msg.TargetCollectiveID)
	}

	// Always sanitize inbound response before exposing to internal caller
	return g.IngestInboundMessage(ctx, resp)
}

// IngestInboundMessage sanitizes and checks security invariants for any message arriving from the boundary.
func (g *BoundaryGateway) IngestInboundMessage(ctx context.Context, msg InboundMessage) (InboundMessage, error) {
	if err := ctx.Err(); err != nil {
		return InboundMessage{}, err
	}
	return g.sanitizer.SanitizeInboundMessage(msg)
}

// ValidateAndSanitizeAgentCard checks external agent cards for integrity, injection, and privilege boundaries.
func (g *BoundaryGateway) ValidateAndSanitizeAgentCard(card AgentCard) (AgentCard, error) {
	return g.sanitizer.ValidateAndSanitizeAgentCard(card)
}
