package teb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrProviderUnavailable indicates the requested TEB provider is offline or missing.
	ErrProviderUnavailable = errors.New("TEB provider unavailable")
	// ErrBypassDetected indicates an attempted escape from the sandbox boundary.
	ErrBypassDetected = errors.New("security boundary bypass detected")
	// ErrEnforcementMismatch indicates the actual enforcement failed to reach the required profile level.
	ErrEnforcementMismatch = errors.New("enforcement level mismatch")
	// ErrAuthorityTampering indicates an attempt by sandbox outputs to alter or inject authority tokens.
	ErrAuthorityTampering = errors.New("sandbox cannot mint or alter authority tokens")
)

// ExecutionRequest specifies container and sandbox execution parameters.
type ExecutionRequest struct {
	Image       string
	Workspace   string
	Command     []string
	Profile     Profile
	NetworkName string
	Limits      ResourceLimits
	Environment map[string]string
}

// ExecutionResult records the outcome, verified guarantees, and evidence of execution.
type ExecutionResult struct {
	ExitCode         int
	Stdout           string
	Stderr           string
	EnforcementLevel domain.EnforcementLevel
	Guarantees       map[Guarantee]bool
	Evidence         []string
	ProviderName     string
	EvidenceHash     string
}

// TEBProvider defines the provider-neutral execution boundary contract.
// Meeseek retains sovereign ownership of authority and policy contracts, while
// TEBProvider implementations supply the low-level sandbox isolation mechanics.
type TEBProvider interface {
	Name() string
	Available(ctx context.Context) bool
	Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error)
	Assess(req ExecutionRequest) (Profile, error)
}

// ComputeEvidenceHash creates a deterministic SHA-256 fingerprint of the verification evidence.
func ComputeEvidenceHash(provider string, level domain.EnforcementLevel, guarantees map[Guarantee]bool, evidence []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "provider:%s\nlevel:%s\n", strings.TrimSpace(provider), level)

	// Deterministic guarantee order
	canonicalGuarantees := []Guarantee{
		GuaranteeReadOnlyRoot,
		GuaranteeNonRoot,
		GuaranteeNoNewPrivileges,
		GuaranteeCapabilitiesDropped,
		GuaranteePIDLimit,
		GuaranteeMemoryLimit,
		GuaranteeCPULimit,
		GuaranteeNetworkIsolated,
		GuaranteeWorkspaceOnlyWrite,
		GuaranteeNoAmbientCredentials,
		GuaranteeNoContainerSocket,
		GuaranteeModelEgressMediated,
	}
	for _, g := range canonicalGuarantees {
		if guarantees[g] {
			fmt.Fprintf(h, "guarantee:%s\n", g)
		}
	}

	cleanEvidence := make([]string, 0, len(evidence))
	for _, e := range evidence {
		trimmed := strings.TrimSpace(e)
		if trimmed != "" {
			cleanEvidence = append(cleanEvidence, trimmed)
		}
	}
	sort.Strings(cleanEvidence)
	for _, e := range cleanEvidence {
		fmt.Fprintf(h, "evidence:%s\n", e)
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Manager coordinates execution across multiple TEB providers with strict fail-closed semantics.
type Manager struct {
	providers        map[string]TEBProvider
	primaryProvider  string
	fallbackProvider string
}

// NewManager creates a Manager configured with a primary provider and optional fallbacks.
func NewManager(primary string, fallback string, providers ...TEBProvider) *Manager {
	m := &Manager{
		providers:        make(map[string]TEBProvider, len(providers)),
		primaryProvider:  strings.TrimSpace(primary),
		fallbackProvider: strings.TrimSpace(fallback),
	}
	for _, p := range providers {
		if p != nil {
			m.providers[p.Name()] = p
		}
	}
	return m
}

// Register adds or updates a provider.
func (m *Manager) Register(p TEBProvider) {
	if m == nil || p == nil {
		return
	}
	if m.providers == nil {
		m.providers = make(map[string]TEBProvider)
	}
	m.providers[p.Name()] = p
}

// Execute dispatches the request to the appropriate provider, enforcing fail-closed guarantees.
func (m *Manager) Execute(ctx context.Context, req ExecutionRequest) (ExecutionResult, error) {
	if m == nil || len(m.providers) == 0 {
		return ExecutionResult{}, fmt.Errorf("%w: no providers registered", ErrProviderUnavailable)
	}

	primary, hasPrimary := m.providers[m.primaryProvider]
	var selected TEBProvider

	if hasPrimary && primary.Available(ctx) {
		selected = primary
	} else if m.fallbackProvider != "" {
		fallback, hasFallback := m.providers[m.fallbackProvider]
		if hasFallback && fallback.Available(ctx) {
			selected = fallback
		}
	}

	if selected == nil {
		// Fail-closed: when requested enforcement is ENFORCED, unavailability cannot degrade silently.
		if req.Profile.Level == domain.EnforcementEnforced {
			return ExecutionResult{
				EnforcementLevel: domain.EnforcementUnenforced,
			}, fmt.Errorf("%w: all capable providers unavailable for ENFORCED profile", ErrProviderUnavailable)
		}
		// If partial/unenforced is permitted, return honest degradation error
		return ExecutionResult{
			EnforcementLevel: domain.EnforcementUnenforced,
		}, fmt.Errorf("%w: no available provider for level %s", ErrProviderUnavailable, req.Profile.Level)
	}

	res, err := selected.Execute(ctx, req)
	if err != nil {
		return res, err
	}

	// Verify that the provider reached the required enforcement level.
	if req.Profile.Level == domain.EnforcementEnforced && res.EnforcementLevel != domain.EnforcementEnforced {
		return res, fmt.Errorf("%w: requested %s but got %s", ErrEnforcementMismatch, req.Profile.Level, res.EnforcementLevel)
	}

	// Validate authority isolation: sandbox stdout/stderr must never inject authority tokens
	if strings.Contains(res.Stdout, "SUMMA42_SOVEREIGN_AUTHORITY_GRANT") ||
		strings.Contains(res.Stderr, "SUMMA42_SOVEREIGN_AUTHORITY_GRANT") {
		return res, fmt.Errorf("%w: sandbox output attempted authority spoofing", ErrAuthorityTampering)
	}

	return res, nil
}
