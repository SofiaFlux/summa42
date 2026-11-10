package mission

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrUnauthorizedApproval is returned when an autonomous worker or research routine attempts to self-approve a mission.
	ErrUnauthorizedApproval = errors.New("unauthorized: mission approval requires human/root governance authority")

	// ErrUnauthorizedGovernance is returned when an unauthorized principal attempts a governance action.
	ErrUnauthorizedGovernance = errors.New("unauthorized: mission governance requires human/root governance authority")

	// ErrInvalidGovernanceSignature is returned when cryptographic governance verification fails.
	ErrInvalidGovernanceSignature = errors.New("invalid governance signature or missing authority credential")

	// ErrInvalidStateTransition is returned when a requested lifecycle transition is illegal.
	ErrInvalidStateTransition = errors.New("invalid mission lifecycle state transition")

	// ErrMissionImmutable is returned when attempting to mutate an approved, active, or superseded mission version in-place.
	ErrMissionImmutable = errors.New("mission is immutable in current state; create next version to modify")

	// ErrMissionNotFound is returned when a requested mission version does not exist.
	ErrMissionNotFound = errors.New("mission not found")

	// ErrDuplicateMissionVersion is returned when attempting to insert an already existing mission version.
	ErrDuplicateMissionVersion = errors.New("mission version already exists: append-only version invariant")

	// ErrActiveMissionNotFound is returned when no active version exists for a mission ID.
	ErrActiveMissionNotFound = errors.New("no active mission version found")

	// ErrIncompleteMissionSchema is returned when essential governance sections are missing before proposal or approval.
	ErrIncompleteMissionSchema = errors.New("mission schema incomplete: all required governance sections must be populated")
)

// MissionState represents the canonical lifecycle stages of a versioned mission.
type MissionState string

const (
	MissionStateDraft      MissionState = "DRAFT"
	MissionStateProposed   MissionState = "PROPOSED"
	MissionStateApproved   MissionState = "APPROVED"
	MissionStateActive     MissionState = "ACTIVE"
	MissionStateSuperseded MissionState = "SUPERSEDED"
)

// PrincipalKind categorizes actors in the collective hierarchy.
type PrincipalKind string

const (
	PrincipalKindHuman  PrincipalKind = "HUMAN"
	PrincipalKindRoot   PrincipalKind = "ROOT"
	PrincipalKindWorker PrincipalKind = "WORKER"
)

// Principal represents the caller initiating a mission lifecycle transition.
type Principal struct {
	ID        string            `json:"id"`
	Kind      PrincipalKind     `json:"kind"`
	PublicKey ed25519.PublicKey `json:"public_key,omitempty"`
	Signature []byte            `json:"signature,omitempty"`
}

// IsAuthorizedGovernance evaluates whether the principal possesses sovereign governance authority.
func (p Principal) IsAuthorizedGovernance() bool {
	return p.Kind == PrincipalKindHuman || p.Kind == PrincipalKindRoot
}

// VerifySignature validates cryptographic proof if signature and public key are present.
func (p Principal) VerifySignature(payload []byte) bool {
	if len(p.PublicKey) == ed25519.PublicKeySize && len(p.Signature) > 0 {
		return ed25519.Verify(p.PublicKey, payload, p.Signature)
	}
	// Fall back to trusted kind if no signature payload provided
	return p.IsAuthorizedGovernance()
}

// TradeOffRule defines explicit, deterministic tie-breaking priorities between competing values.
type TradeOffRule struct {
	Priority    string `json:"priority"`
	Subordinate string `json:"subordinate"`
	Rationale   string `json:"rationale"`
}

// MissionProvenance preserves the complete audit lineage of a mission specification.
type MissionProvenance struct {
	BaseVersion         int        `json:"base_version,omitempty"`
	SupersededByVersion int        `json:"superseded_by_version,omitempty"`
	SourceInputs        []string   `json:"source_inputs,omitempty"`
	EvidenceRefs        []string   `json:"evidence_refs,omitempty"`
	AuthorPrincipal     string     `json:"author_principal"`
	ProposedBy          string     `json:"proposed_by,omitempty"`
	ProposedAt          *time.Time `json:"proposed_at,omitempty"`
	ApprovingPrincipal  string     `json:"approving_principal,omitempty"`
	ApprovedAt          *time.Time `json:"approved_at,omitempty"`
	GovernanceSignature string     `json:"governance_signature,omitempty"`
	ChangeSummary       string     `json:"change_summary,omitempty"`
}

// Mission is the canonical schema capturing the top-level intent, boundaries, and trade-offs of the Collective.
type Mission struct {
	ID                   domain.ID         `json:"id"`
	Version              int               `json:"version"`
	State                MissionState      `json:"state"`
	Purpose              string            `json:"purpose"`
	DesiredOutcomes      []string          `json:"desired_outcomes,omitempty"`
	Obligations          []string          `json:"obligations,omitempty"`
	Constraints          []string          `json:"constraints,omitempty"`
	NonGoals             []string          `json:"non_goals,omitempty"`
	TradeOffRules        []TradeOffRule    `json:"trade_off_rules,omitempty"`
	HumanTrustBoundaries []string          `json:"human_trust_boundaries,omitempty"`
	Assumptions          []string          `json:"assumptions,omitempty"`
	Unknowns             []string          `json:"unknowns,omitempty"`
	RevisionTriggers     []string          `json:"revision_triggers,omitempty"`
	Provenance           MissionProvenance `json:"provenance"`
	CreatedAt            time.Time         `json:"created_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

// ValidateCompleteness ensures all required governance sections are populated before proposal or approval.
func (m *Mission) ValidateCompleteness() error {
	if strings.TrimSpace(m.Purpose) == "" {
		return errors.New("purpose is required")
	}
	if len(m.DesiredOutcomes) == 0 {
		return errors.New("desired_outcomes cannot be empty")
	}
	if len(m.Obligations) == 0 {
		return errors.New("obligations cannot be empty")
	}
	if len(m.Constraints) == 0 {
		return errors.New("constraints cannot be empty")
	}
	if len(m.NonGoals) == 0 {
		return errors.New("non_goals cannot be empty")
	}
	if len(m.TradeOffRules) == 0 {
		return errors.New("trade_off_rules cannot be empty")
	}
	if len(m.HumanTrustBoundaries) == 0 {
		return errors.New("human_trust_boundaries cannot be empty")
	}
	return nil
}

// CreateMissionFields encapsulates input fields for drafting a mission.
type CreateMissionFields struct {
	Purpose              string         `json:"purpose"`
	DesiredOutcomes      []string       `json:"desired_outcomes,omitempty"`
	Obligations          []string       `json:"obligations,omitempty"`
	Constraints          []string       `json:"constraints,omitempty"`
	NonGoals             []string       `json:"non_goals,omitempty"`
	TradeOffRules        []TradeOffRule `json:"trade_off_rules,omitempty"`
	HumanTrustBoundaries []string       `json:"human_trust_boundaries,omitempty"`
	Assumptions          []string       `json:"assumptions,omitempty"`
	Unknowns             []string       `json:"unknowns,omitempty"`
	RevisionTriggers     []string       `json:"revision_triggers,omitempty"`
	SourceInputs         []string       `json:"source_inputs,omitempty"`
	EvidenceRefs         []string       `json:"evidence_refs,omitempty"`
	ChangeSummary        string         `json:"change_summary,omitempty"`
}
