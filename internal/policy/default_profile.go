package policy

import (
	"fmt"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// DefaultModule is policy_mvc_default_v2. Collectives initialized before it
// keep policy_mvc_default_v1 (ALLOW only LOW) until the Owner runs
// `summa42 policy upgrade`.
const (
	DefaultModuleName = "summa42-default.rego"
	DefaultModule     = `package summa42

default decision := {"outcome": "DENY", "reason_codes": ["default_deny"]}

decision := {"outcome": "ALLOW", "reason_codes": ["low_risk"]} if {
  input.risk == "LOW"
  input.authority_valid == true
}

decision := {"outcome": "REQUIRE_APPROVAL", "required_approvals": [input.owner_principal_id], "reason_codes": ["medium_risk_owner_approval"]} if {
  input.risk == "MEDIUM"
  input.authority_valid == true
  input.owner_principal_id != ""
}
`
	DefaultPolicySetID domain.ID = "policy_mvc_default_v2"
)

type Profile struct {
	PolicySetID      domain.ID
	ModuleName       string
	Module           string
	PolicyHash       string
	CapabilitiesHash string
}

func DefaultProfile() (Profile, error) {
	capabilitiesHash, err := SafeCapabilitiesHash()
	if err != nil {
		return Profile{}, fmt.Errorf("hash safe OPA capabilities: %w", err)
	}
	return Profile{
		PolicySetID:      DefaultPolicySetID,
		ModuleName:       DefaultModuleName,
		Module:           DefaultModule,
		PolicyHash:       digestBytes([]byte(DefaultModule)),
		CapabilitiesHash: capabilitiesHash,
	}, nil
}
