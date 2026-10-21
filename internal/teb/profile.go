// Package teb holds trusted-execution-boundary primitives (OCI argument
// building, egress proxy, egress assessment). They are primitives, not wired:
// nothing here establishes a guarantee on its own, and several guarantee names
// (for example NO_AMBIENT_CREDENTIALS, NO_CONTAINER_SOCKET) are declared
// without a runtime check. Treat the package as experimental.
package teb

import (
	"fmt"

	"github.com/SofiaFlux/summa42/internal/domain"
)

type Guarantee string

const (
	GuaranteeReadOnlyRoot         Guarantee = "READ_ONLY_ROOT"
	GuaranteeNonRoot              Guarantee = "NON_ROOT"
	GuaranteeNoNewPrivileges      Guarantee = "NO_NEW_PRIVILEGES"
	GuaranteeCapabilitiesDropped  Guarantee = "CAPABILITIES_DROPPED"
	GuaranteePIDLimit             Guarantee = "PID_LIMIT"
	GuaranteeMemoryLimit          Guarantee = "MEMORY_LIMIT"
	GuaranteeCPULimit             Guarantee = "CPU_LIMIT"
	GuaranteeNetworkIsolated      Guarantee = "NETWORK_ISOLATED"
	GuaranteeWorkspaceOnlyWrite   Guarantee = "WORKSPACE_ONLY_WRITE"
	GuaranteeNoAmbientCredentials Guarantee = "NO_AMBIENT_CREDENTIALS"
	GuaranteeNoContainerSocket    Guarantee = "NO_CONTAINER_SOCKET"
	GuaranteeModelEgressMediated  Guarantee = "MODEL_EGRESS_MEDIATED"
)

type Profile struct {
	Name       string
	Level      domain.EnforcementLevel
	Guarantees map[Guarantee]bool
}

func EnforcedOfflineProfile() Profile {
	return Profile{
		Name:  "oci-offline-strong",
		Level: domain.EnforcementEnforced,
		Guarantees: guaranteeSet(
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
		),
	}
}

func EnforcedProxyProfile() Profile {
	return Profile{
		Name:  "oci-proxy-strong",
		Level: domain.EnforcementEnforced,
		Guarantees: guaranteeSet(
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
		),
	}
}

func PartialProfile(name string, guarantees ...Guarantee) Profile {
	return Profile{Name: name, Level: domain.EnforcementPartial, Guarantees: guaranteeSet(guarantees...)}
}

func UnenforcedProfile(name string) Profile {
	return Profile{Name: name, Level: domain.EnforcementUnenforced, Guarantees: map[Guarantee]bool{}}
}

func (p Profile) Validate() error {
	switch p.Level {
	case domain.EnforcementEnforced, domain.EnforcementPartial, domain.EnforcementUnenforced:
	default:
		return fmt.Errorf("invalid TEB enforcement level %q", p.Level)
	}
	if p.Guarantees == nil {
		return fmt.Errorf("TEB profile %q has no guarantee map", p.Name)
	}
	if p.Level == domain.EnforcementEnforced {
		for _, required := range enforcedOfflineGuarantees() {
			if !p.Guarantees[required] {
				return fmt.Errorf("ENFORCED profile %q is missing guarantee %q", p.Name, required)
			}
		}
	}
	return nil
}

func enforcedOfflineGuarantees() []Guarantee {
	return []Guarantee{
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
	}
}

func guaranteeSet(guarantees ...Guarantee) map[Guarantee]bool {
	result := make(map[Guarantee]bool, len(guarantees))
	for _, guarantee := range guarantees {
		result[guarantee] = true
	}
	return result
}
