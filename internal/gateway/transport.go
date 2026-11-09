package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// InboundMessage represents a protocol-agnostic message arriving from an external collective.
type InboundMessage struct {
	MessageID            domain.ID         `json:"message_id"`
	TransportProtocol    string            `json:"transport_protocol"`
	OriginCollectiveID   domain.ID         `json:"origin_collective_id"`
	SenderPrincipalID    domain.ID         `json:"sender_principal_id"`
	RecipientPrincipalID domain.ID         `json:"recipient_principal_id"`
	PayloadType          string            `json:"payload_type"`
	RawPayload           []byte            `json:"raw_payload"`
	Metadata             map[string]string `json:"metadata,omitempty"`
}

// OutboundMessage represents a message destined for an external agent or collective.
type OutboundMessage struct {
	MessageID          domain.ID         `json:"message_id"`
	TargetCollectiveID domain.ID         `json:"target_collective_id"`
	TargetPrincipalID  domain.ID         `json:"target_principal_id"`
	PayloadType        string            `json:"payload_type"`
	RawPayload         []byte            `json:"raw_payload"`
	Metadata           map[string]string `json:"metadata,omitempty"`
}

// AgentCard represents a discovery advertisement of capabilities for an agent.
type AgentCard struct {
	AgentID              domain.ID `json:"agent_id"`
	CollectiveID         domain.ID `json:"collective_id"`
	Name                 string    `json:"name"`
	Description          string    `json:"description,omitempty"`
	DeclaredCapabilities []string  `json:"declared_capabilities"`
	Endpoints            []string  `json:"endpoints,omitempty"`
	PublicKeyHex         string    `json:"public_key_hex"`
}

// InterCollectiveTransport is the protocol-agnostic interface isolating wire serialization
// (such as A2A, SLIM, HTTPS, MCP) from local Summa42 authority and delegation evaluation.
type InterCollectiveTransport interface {
	ProtocolName() string
	SendMessage(ctx context.Context, msg OutboundMessage) (InboundMessage, error)
	PublishAgentCard(ctx context.Context, card AgentCard) error
}

// MockTransport is an in-memory transport implementation for testing protocol-swapping agnosticism.
type MockTransport struct {
	protocolName   string
	publishedCards []AgentCard
	mu             sync.Mutex
}

func NewMockTransport(protocolName string) *MockTransport {
	return &MockTransport{
		protocolName: protocolName,
	}
}

func (m *MockTransport) ProtocolName() string {
	return m.protocolName
}

func (m *MockTransport) SendMessage(ctx context.Context, msg OutboundMessage) (InboundMessage, error) {
	if err := ctx.Err(); err != nil {
		return InboundMessage{}, err
	}
	if msg.MessageID == "" {
		return InboundMessage{}, errors.New("outbound message ID is required")
	}

	replyPayloadType := "DISCOVERY_REPLY"
	if msg.PayloadType != "TASK_DISCOVERY" {
		replyPayloadType = fmt.Sprintf("%s_REPLY", msg.PayloadType)
	}

	return InboundMessage{
		MessageID:            domain.NewID("msg_reply"),
		TransportProtocol:    m.protocolName,
		OriginCollectiveID:   msg.TargetCollectiveID,
		SenderPrincipalID:    msg.TargetPrincipalID,
		RecipientPrincipalID: "local_gateway",
		PayloadType:          replyPayloadType,
		RawPayload:           []byte(fmt.Sprintf(`{"status":"ACK","origin_protocol":"%s"}`, m.protocolName)),
		Metadata:             map[string]string{"transport": m.protocolName},
	}, nil
}

func (m *MockTransport) PublishAgentCard(ctx context.Context, card AgentCard) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishedCards = append(m.publishedCards, card)
	return nil
}
