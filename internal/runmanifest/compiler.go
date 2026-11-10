package runmanifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

var (
	// ErrStaleWorkspace indicates the workspace fence does not match the active canonical attempt.
	ErrStaleWorkspace = errors.New("stale attempt workspace: fence generation mismatch")
	// ErrUnauthorizedCapability indicates an attempt to invoke an unmediated/fabricated capability.
	ErrUnauthorizedCapability = errors.New("unauthorized capability: not in canonical visible set")
)

// CAFESAssessment evaluates context projection quality along Google CAFE(S) dimensions.
type CAFESAssessment struct {
	ClarityScore       float64  `json:"clarity_score"`       // Unambiguous structure, clear headings, consistent format
	ActionabilityScore float64  `json:"actionability_score"` // Explicit objectives, concrete acceptance criteria, visible tools
	FidelityScore      float64  `json:"fidelity_score"`      // Accurate reflection of domain state without hallucinated properties
	EfficiencyScore    float64  `json:"efficiency_score"`    // Bounded working set, minimal irrelevant context noise
	SecurityScore      float64  `json:"security_score"`      // Zero ambient credentials, read-only enforcement, unprivileged files
	Passed             bool     `json:"passed"`
	Violations         []string `json:"violations,omitempty"`
}

// WorkingContextProjection is the compiled working context presented to an attempt workspace.
type WorkingContextProjection struct {
	TaskID              domain.ID            `json:"task_id"`
	AttemptID           domain.ID            `json:"attempt_id"`
	FenceGeneration     int64                `json:"fence_generation"`
	Objective           string               `json:"objective"`
	AcceptanceCriteria  []string             `json:"acceptance_criteria"`
	VisibleCapabilities []CapabilitySnapshot `json:"visible_capabilities"`
	InputEvidence       []EvidenceRef        `json:"input_evidence"`
	PolicyHash          string               `json:"policy_hash"`
	ContextHash         string               `json:"context_hash"`
	Artifacts           map[string]string    `json:"artifacts"`
	Assessment          CAFESAssessment      `json:"assessment"`
}

// ContextCompiler compiles canonical domain state into an Attempt Working Context projection.
type ContextCompiler struct{}

// NewContextCompiler creates a new ContextCompiler.
func NewContextCompiler() *ContextCompiler {
	return &ContextCompiler{}
}

// Compile translates canonical domain state into a deterministic WorkingContextProjection.
func (c *ContextCompiler) Compile(
	task domain.Task,
	attempt domain.Attempt,
	evidence []EvidenceRef,
	policy PolicySnapshot,
	capabilities []CapabilitySnapshot,
) (WorkingContextProjection, error) {
	if task.ID == "" || attempt.ID == "" {
		return WorkingContextProjection{}, errors.New("task and attempt IDs are required")
	}
	if attempt.TaskID != task.ID {
		return WorkingContextProjection{}, errors.New("attempt does not belong to specified task")
	}

	artifacts := make(map[string]string)

	// 1. Materialize TASK.md (Clarity & Actionability)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Task: %s\n\n", task.ID))
	sb.WriteString(fmt.Sprintf("**Attempt ID:** %s  \n", attempt.ID))
	sb.WriteString(fmt.Sprintf("**Fence Generation:** %d  \n\n", attempt.FenceGeneration))
	sb.WriteString("## Objective\n")
	sb.WriteString(task.Objective + "\n\n")

	// Parse criteria from task or payload
	var criteria []string
	if len(task.AcceptanceCriteria) > 0 {
		criteria = append([]string(nil), task.AcceptanceCriteria...)
	} else if len(task.PayloadJSON) > 0 {
		var payload struct {
			AcceptanceCriteria []string `json:"acceptanceCriteria"`
		}
		if err := json.Unmarshal(task.PayloadJSON, &payload); err == nil {
			criteria = payload.AcceptanceCriteria
		}
	}
	if len(criteria) > 0 {
		sb.WriteString("## Acceptance Criteria\n")
		for _, crit := range criteria {
			sb.WriteString(fmt.Sprintf("- [ ] %s\n", crit))
		}
		sb.WriteString("\n")
	}
	artifacts["TASK.md"] = sb.String()

	// 2. Materialize CAPABILITIES.json (Actionability & Security)
	cleanCaps := append([]CapabilitySnapshot(nil), capabilities...)
	sort.Slice(cleanCaps, func(i, j int) bool {
		return cleanCaps[i].SemanticName < cleanCaps[j].SemanticName
	})
	capsJSON, err := json.MarshalIndent(cleanCaps, "", "  ")
	if err != nil {
		return WorkingContextProjection{}, fmt.Errorf("marshal capabilities: %w", err)
	}
	artifacts["CAPABILITIES.json"] = string(capsJSON)

	// 3. Materialize EVIDENCE.md (Fidelity & Provenance)
	var evSb strings.Builder
	evSb.WriteString("# Prior Evidence\n\n")
	if len(evidence) == 0 {
		evSb.WriteString("No prior evidence items provided for this attempt.\n")
	} else {
		for _, ev := range evidence {
			evSb.WriteString(fmt.Sprintf("- **Evidence %s**: `sha256:%s`\n", ev.ID, ev.ContentHash))
		}
	}
	artifacts["EVIDENCE.md"] = evSb.String()

	// 4. Materialize POLICIES.md (Security & Constraints)
	var polSb strings.Builder
	polSb.WriteString("# Applicable Policies\n\n")
	polSb.WriteString(fmt.Sprintf("- PolicySet: `%s`\n", policy.PolicySetID))
	polSb.WriteString(fmt.Sprintf("- PolicyHash: `%s`\n", policy.PolicySetHash))
	artifacts["POLICIES.md"] = polSb.String()

	// Deterministic Context Hash calculation
	h := sha256.New()
	fmt.Fprintf(h, "task:%s\nattempt:%s\nfence:%d\n", task.ID, attempt.ID, attempt.FenceGeneration)
	fmt.Fprintf(h, "objective:%s\npolicy_hash:%s\n", task.Objective, policy.PolicySetHash)
	for _, c := range cleanCaps {
		fmt.Fprintf(h, "cap:%s:%s\n", c.SemanticName, c.SemanticVersion)
	}
	for _, ev := range evidence {
		fmt.Fprintf(h, "ev:%s:%s\n", ev.ID, ev.ContentHash)
	}
	contextHash := hex.EncodeToString(h.Sum(nil))

	proj := WorkingContextProjection{
		TaskID:              task.ID,
		AttemptID:           attempt.ID,
		FenceGeneration:     attempt.FenceGeneration,
		Objective:           task.Objective,
		AcceptanceCriteria:  criteria,
		VisibleCapabilities: cleanCaps,
		InputEvidence:       evidence,
		PolicyHash:          policy.PolicySetHash,
		ContextHash:         contextHash,
		Artifacts:           artifacts,
	}

	proj.Assessment = c.AssessCAFES(proj)

	return proj, nil
}

// AssessCAFES evaluates a projection against Google CAFE(S) quality dimensions.
func (c *ContextCompiler) AssessCAFES(proj WorkingContextProjection) CAFESAssessment {
	var violations []string

	// Clarity: Well-structured markdown and required artifact files
	clarity := 1.0
	for _, requiredFile := range []string{"TASK.md", "CAPABILITIES.json", "EVIDENCE.md", "POLICIES.md"} {
		if _, ok := proj.Artifacts[requiredFile]; !ok {
			clarity -= 0.25
			violations = append(violations, fmt.Sprintf("Clarity: missing %s", requiredFile))
		}
	}

	// Actionability: Objective non-empty and visible capabilities defined
	actionability := 1.0
	if strings.TrimSpace(proj.Objective) == "" {
		actionability -= 0.5
		violations = append(violations, "Actionability: empty task objective")
	}

	// Fidelity: Hash integrity and non-zero fence generation
	fidelity := 1.0
	if proj.FenceGeneration <= 0 {
		fidelity -= 0.5
		violations = append(violations, "Fidelity: invalid fence generation")
	}
	if proj.ContextHash == "" {
		fidelity -= 0.5
		violations = append(violations, "Fidelity: missing deterministic context hash")
	}

	// Efficiency: Token economy check (no unbounded raw dumps)
	efficiency := 1.0
	totalBytes := 0
	for _, content := range proj.Artifacts {
		totalBytes += len(content)
	}
	if totalBytes > 100<<10 { // 100 KB limit for lean working context
		efficiency -= 0.5
		violations = append(violations, "Efficiency: context projection exceeds 100KB budget")
	}

	// Security: Zero secret leaks in projected artifacts
	security := 1.0
	for name, content := range proj.Artifacts {
		for _, forbidden := range []string{"PRIVATE KEY", "AWS_SECRET", "SUMMA42_OWNER_KEY", "docker.sock"} {
			if strings.Contains(content, forbidden) {
				security = 0.0
				violations = append(violations, fmt.Sprintf("Security: %s leaked forbidden secret keyword %q", name, forbidden))
			}
		}
	}

	passed := len(violations) == 0 && clarity >= 0.8 && actionability >= 0.8 && fidelity >= 0.8 && security == 1.0

	return CAFESAssessment{
		ClarityScore:       clarity,
		ActionabilityScore: actionability,
		FidelityScore:      fidelity,
		EfficiencyScore:    efficiency,
		SecurityScore:      security,
		Passed:             passed,
		Violations:         violations,
	}
}

// MaterializeToWorkspace writes the compiled artifacts into the target workspace filesystem.
func (c *ContextCompiler) MaterializeToWorkspace(ctx context.Context, proj WorkingContextProjection, workspace string) error {
	if workspace == "" {
		return errors.New("workspace directory path required")
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return fmt.Errorf("create workspace directory: %w", err)
	}

	for filename, content := range proj.Artifacts {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		filePath := filepath.Join(workspace, filename)
		// Write read-only artifacts (0444)
		if err := os.WriteFile(filePath, []byte(content), 0o444); err != nil {
			return fmt.Errorf("write context artifact %s: %w", filename, err)
		}
	}
	return nil
}

// MediatedCapabilityAuthorizer checks whether a capability execution attempt is permitted,
// enforcing that workspace filesystem edits or fake declarations cannot expand authority.
type MediatedCapabilityAuthorizer struct {
	canonicalVisibleCaps map[string]bool
	activeFence          int64
}

// NewMediatedCapabilityAuthorizer constructs an authorizer backed by canonical state.
func NewMediatedCapabilityAuthorizer(canonicalCaps []CapabilitySnapshot, activeFence int64) *MediatedCapabilityAuthorizer {
	set := make(map[string]bool, len(canonicalCaps))
	for _, c := range canonicalCaps {
		set[strings.TrimSpace(c.SemanticName)] = true
	}
	return &MediatedCapabilityAuthorizer{
		canonicalVisibleCaps: set,
		activeFence:          activeFence,
	}
}

// AuthorizeCall strictly verifies the request against canonical state.
func (a *MediatedCapabilityAuthorizer) AuthorizeCall(attemptFence int64, capabilityName string) error {
	if attemptFence != a.activeFence {
		return ErrStaleWorkspace
	}
	if !a.canonicalVisibleCaps[strings.TrimSpace(capabilityName)] {
		return ErrUnauthorizedCapability
	}
	return nil
}
