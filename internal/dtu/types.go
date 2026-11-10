package dtu

import "github.com/SofiaFlux/summa42/internal/runmanifest"

// EnvironmentMode defines whether execution targets the real environment or a behavioral digital twin.
type EnvironmentMode string

const (
	EnvModeLive EnvironmentMode = "LIVE"
	EnvModeTwin EnvironmentMode = "TWIN"
)

func (m EnvironmentMode) Valid() bool {
	return m == EnvModeLive || m == EnvModeTwin
}

// FidelityLevel categorizes the behavioral fidelity of a twin environment.
type FidelityLevel string

const (
	FidelityT0Contract     FidelityLevel = "T0_CONTRACT"
	FidelityT1Behavioral   FidelityLevel = "T1_BEHAVIORAL"
	FidelityT2Adversarial  FidelityLevel = "T2_ADVERSARIAL"
	FidelityT3HighFidelity FidelityLevel = "T3_HIGH_FIDELITY"
	FidelityT4Universe     FidelityLevel = "T4_UNIVERSE"
)

func (f FidelityLevel) Valid() bool {
	switch f {
	case FidelityT0Contract,
		FidelityT1Behavioral,
		FidelityT2Adversarial,
		FidelityT3HighFidelity,
		FidelityT4Universe:
		return true
	default:
		return false
	}
}

// ProviderMetadata advertises capabilities, version, and fidelity of an environment provider.
type ProviderMetadata struct {
	ServiceID          string            `json:"service_id"`
	ProviderID         string            `json:"provider_id"`
	Version            string            `json:"version"`
	Fidelity           FidelityLevel     `json:"fidelity"`
	SupportedProtocols []string          `json:"supported_protocols,omitempty"`
	KnownDivergences   []string          `json:"known_divergences,omitempty"`
	Attributes         map[string]string `json:"attributes,omitempty"`
}

// InvocationRequest represents a semantic invocation through an EnvironmentProvider.
type InvocationRequest struct {
	Action         string            `json:"action"`
	Payload        map[string]any    `json:"payload,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// InvocationResponse encapsulates the outcome of an invocation through an EnvironmentProvider.
type InvocationResponse struct {
	Success    bool              `json:"success"`
	StatusCode int               `json:"status_code,omitempty"`
	Data       map[string]any    `json:"data,omitempty"`
	RawBody    []byte            `json:"raw_body,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// EnvironmentSnapshot aliases runmanifest.EnvironmentSnapshot for single source of truth.
type EnvironmentSnapshot = runmanifest.EnvironmentSnapshot

// CreateEnvironmentSnapshot extracts descriptive environment provenance from a provider.
func CreateEnvironmentSnapshot(p EnvironmentProvider, seed int64) EnvironmentSnapshot {
	meta := p.Metadata()
	divergences := make([]string, len(meta.KnownDivergences))
	copy(divergences, meta.KnownDivergences)

	return EnvironmentSnapshot{
		Mode:              string(p.Mode()),
		ProviderID:        meta.ProviderID,
		ProviderVersion:   meta.Version,
		Fidelity:          string(meta.Fidelity),
		ScenarioSeed:      seed,
		DivergenceProfile: divergences,
	}
}
