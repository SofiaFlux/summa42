package gateway

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/SofiaFlux/summa42/internal/delegation"
	"github.com/SofiaFlux/summa42/internal/domain"
)

const (
	MaxPayloadSize         = 2 * 1024 * 1024 // 2 MB
	MaxMetadataEntries     = 32
	MaxMetadataKeyLength   = 64
	MaxMetadataValueLength = 1024
	MaxAgentCardNameLength = 128
	MaxAgentCardDescLength = 2048
)

var (
	ErrSecurityRejection             = errors.New("security boundary rejection")
	ErrPromptInjectionDetected       = fmt.Errorf("%w: prompt injection vector detected", ErrSecurityRejection)
	ErrInternalIdentityImpersonation = fmt.Errorf("%w: foreign principal cannot assume internal identity", ErrSecurityRejection)
	ErrAnonymousSenderForbidden      = fmt.Errorf("%w: foreign message must specify a valid sender principal", ErrSecurityRejection)
	ErrMalformedAgentCard            = fmt.Errorf("%w: malformed or prohibited agent card", ErrSecurityRejection)
	ErrProhibitedCapability          = fmt.Errorf("%w: foreign card declared prohibited kernel capability", ErrSecurityRejection)
	ErrPayloadTooLarge               = fmt.Errorf("%w: payload exceeds maximum allowable size", ErrSecurityRejection)
	ErrProhibitedEndpoint            = fmt.Errorf("%w: agent card contains prohibited endpoint URL", ErrSecurityRejection)
)

func IsSecurityRejection(err error) bool {
	return errors.Is(err, ErrSecurityRejection)
}

var promptInjectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)system\s+prompt\s*:`),
	regexp.MustCompile(`(?i)\[inst\]`),
	regexp.MustCompile(`(?i)<<sys>>`),
	regexp.MustCompile(`(?i)<\|im_start\|>`),
	regexp.MustCompile(`(?i)<\|im_end\|>`),
	regexp.MustCompile(`(?i)ignore\s+(all\s+)?previous\s+(instructions|constraints|directives)`),
}

// StripInvisibleUnicode removes zero-width characters, bidi overrides, format characters and control chars.
func StripInvisibleUnicode(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x00 {
			return ' '
		}
		// Format characters (Cf) such as zero-width space, joiners, bidi overrides
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		// Control characters (Cc) except standard newline and tab
		if unicode.Is(unicode.Cc, r) && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, s)
}

// ContainsPromptInjectionMarker checks whether text contains known prompt injection signatures.
func ContainsPromptInjectionMarker(text string) bool {
	cleaned := StripInvisibleUnicode(text)
	for _, p := range promptInjectionPatterns {
		if p.MatchString(cleaned) {
			return true
		}
	}
	return false
}

// SanitizePayload neutralizes prompt injection vectors and strips control characters.
func SanitizePayload(raw []byte) ([]byte, bool) {
	if len(raw) == 0 {
		return raw, false
	}

	cleanedText := StripInvisibleUnicode(string(raw))
	modified := cleanedText != string(raw)

	text := cleanedText
	for _, p := range promptInjectionPatterns {
		if p.MatchString(text) {
			text = p.ReplaceAllString(text, "[REDACTED_INJECTION_VECTOR]")
			modified = true
		}
	}

	return []byte(text), modified
}

// GatewaySanitizer coordinates security checks at the boundary.
type GatewaySanitizer struct {
	validator *delegation.Validator
}

func NewGatewaySanitizer(validator *delegation.Validator) *GatewaySanitizer {
	if validator == nil {
		validator = delegation.NewValidator(delegation.MapPublicKeyResolver{})
	}
	return &GatewaySanitizer{
		validator: validator,
	}
}

// isAllowedMetadataKey implements a strict allowlist for headers traversing the boundary.
func isAllowedMetadataKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	switch lower {
	case "correlation_id", "traceparent", "tracestate", "idempotency_key",
		"content_type", "reply_to", "timestamp", "session_id":
		return true
	}
	if strings.HasPrefix(lower, "x-correlation-") || strings.HasPrefix(lower, "x-request-") {
		return true
	}
	return false
}

// SanitizeInboundMessage validates and cleanses an inbound message from an external transport.
func (s *GatewaySanitizer) SanitizeInboundMessage(msg InboundMessage) (InboundMessage, error) {
	// 0. Payload size check
	if len(msg.RawPayload) > MaxPayloadSize {
		return InboundMessage{}, fmt.Errorf("%w: size %d exceeds limit %d", ErrPayloadTooLarge, len(msg.RawPayload), MaxPayloadSize)
	}

	// 1. Mandatory sender check
	senderStr := strings.TrimSpace(string(msg.SenderPrincipalID))
	if senderStr == "" {
		return InboundMessage{}, ErrAnonymousSenderForbidden
	}

	// 2. Confused-deputy guard: Foreign sender must not claim internal summand or root identity
	if s.validator.IsInternalIdentity(msg.SenderPrincipalID) {
		return InboundMessage{}, fmt.Errorf("%w: sender %s is internal identity", ErrInternalIdentityImpersonation, msg.SenderPrincipalID)
	}

	// 3. Prompt injection defense on payload
	sanitizedPayload, _ := SanitizePayload(msg.RawPayload)
	msg.RawPayload = sanitizedPayload

	// 4. Strict allowlist and sanitization on metadata
	if len(msg.Metadata) > 0 {
		if len(msg.Metadata) > MaxMetadataEntries {
			return InboundMessage{}, fmt.Errorf("%w: too many metadata entries", ErrSecurityRejection)
		}
		cleanedMeta := make(map[string]string)
		for k, v := range msg.Metadata {
			if len(k) > MaxMetadataKeyLength || len(v) > MaxMetadataValueLength {
				continue
			}
			if !isAllowedMetadataKey(k) {
				continue // Strip un-allowlisted metadata
			}
			// Sanitize metadata values
			sanitizedVal, _ := SanitizePayload([]byte(v))
			cleanedMeta[k] = string(sanitizedVal)
		}
		msg.Metadata = cleanedMeta
	}

	return msg, nil
}

// ValidateAndSanitizeAgentCard checks external agent cards for integrity, injection, and privilege boundaries.
func (s *GatewaySanitizer) ValidateAndSanitizeAgentCard(card AgentCard) (AgentCard, error) {
	card.AgentID = domain.ID(strings.TrimSpace(string(card.AgentID)))
	card.CollectiveID = domain.ID(strings.TrimSpace(string(card.CollectiveID)))
	card.Name = strings.TrimSpace(card.Name)
	card.Description = strings.TrimSpace(card.Description)

	if card.AgentID == "" {
		return AgentCard{}, fmt.Errorf("%w: agent_id is required", ErrMalformedAgentCard)
	}
	if card.CollectiveID == "" {
		return AgentCard{}, fmt.Errorf("%w: collective_id is required", ErrMalformedAgentCard)
	}
	if len(card.Name) > MaxAgentCardNameLength {
		return AgentCard{}, fmt.Errorf("%w: agent name too long", ErrMalformedAgentCard)
	}
	if len(card.Description) > MaxAgentCardDescLength {
		return AgentCard{}, fmt.Errorf("%w: agent description too long", ErrMalformedAgentCard)
	}

	// 1. Identity isolation
	if s.validator.IsInternalIdentity(card.AgentID) {
		return AgentCard{}, fmt.Errorf("%w: agent_id %s is internal identity", ErrInternalIdentityImpersonation, card.AgentID)
	}

	// 2. Validate Public Key format
	if card.PublicKeyHex == "" {
		return AgentCard{}, fmt.Errorf("%w: public key cannot be empty", ErrMalformedAgentCard)
	}
	pubBytes, err := hex.DecodeString(card.PublicKeyHex)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		return AgentCard{}, fmt.Errorf("%w: invalid ed25519 public key hex", ErrMalformedAgentCard)
	}

	// 3. Prompt injection checks on text metadata
	if ContainsPromptInjectionMarker(card.Name) || ContainsPromptInjectionMarker(card.Description) {
		return AgentCard{}, fmt.Errorf("%w: injection marker in card name or description", ErrPromptInjectionDetected)
	}

	// 4. Capability scope isolation: Foreign cards cannot declare internal kernel capabilities
	for _, cap := range card.DeclaredCapabilities {
		lowerCap := strings.ToLower(cap)
		if strings.HasPrefix(lowerCap, "summa:kernel:") ||
			strings.HasPrefix(lowerCap, "kernel.") ||
			strings.HasPrefix(lowerCap, "summa:db:") ||
			strings.HasPrefix(lowerCap, "internal:") {
			return AgentCard{}, fmt.Errorf("%w: capability %s is reserved for internal kernel", ErrProhibitedCapability, cap)
		}
	}

	// 5. Endpoint URL validation: Must be https and not private/loopback/metadata endpoints
	for _, ep := range card.Endpoints {
		u, err := url.Parse(ep)
		if err != nil || u.Scheme != "https" {
			return AgentCard{}, fmt.Errorf("%w: endpoint must be secure https url: %s", ErrProhibitedEndpoint, ep)
		}
		host := strings.ToLower(u.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "169.254.169.254" || strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "192.168.") {
			return AgentCard{}, fmt.Errorf("%w: loopback or cloud-metadata endpoint forbidden: %s", ErrProhibitedEndpoint, ep)
		}
	}

	return card, nil
}
