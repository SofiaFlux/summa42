package ghtriage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	TriageRulesVersion      = "ghtriage.rules.v1"
	DispositionRulesVersion = "ghtriage.dispositions.v1"
	ReviewerVersion         = "ghtriage.reviewer.v1"

	DecisionSchema    = "github.issue.triage.decision.v1"
	Stage2InputSchema = "github.issue.triage.stage2.input.v1"
	ReviewSchema      = "github.issue.triage.review.v1"

	KindDecision = "github.issue.triage.decision"
	KindFailure  = "github.issue.triage.failure"
	KindReview   = "github.issue.triage.review"
	KindAccepted = "github.issue.triage.accepted"
)

// Disposition is a closed vocabulary. Stage 3 is total over it, so no
// decision can name a disposition outside this set.
type Disposition string

const (
	DispositionReadyToPlan   Disposition = "ready-to-plan"
	DispositionNotActionable Disposition = "not-actionable"
	DispositionDuplicate     Disposition = "duplicate"
	DispositionNeedsHuman    Disposition = "needs-human"
)

func (d Disposition) valid() bool {
	switch d {
	case DispositionReadyToPlan, DispositionNotActionable, DispositionDuplicate, DispositionNeedsHuman:
		return true
	}
	return false
}

type Triage string

const (
	TriageBug          Triage = "bug"
	TriageFeature      Triage = "feature"
	TriageUnclassified Triage = "unclassified"
	TriageQuestion     Triage = "question"
	TriageDuplicate    Triage = "duplicate"
)

type Scope string

const (
	ScopeSmall   Scope = "small"
	ScopeMedium  Scope = "medium"
	ScopeLarge   Scope = "large"
	ScopeUnknown Scope = "unknown"
)

type SuggestedType string

const (
	SuggestedTypeBug      SuggestedType = "bug"
	SuggestedTypeFeature  SuggestedType = "feature"
	SuggestedTypeQuestion SuggestedType = "question"
	SuggestedTypeOther    SuggestedType = "other"
)

// Snapshot mirrors the JSON that ghissue.CanonicalSnapshot writes, so a stored
// issue snapshot decodes without a conversion.
type Snapshot struct {
	Repo      string   `json:"repo"`
	Issue     int64    `json:"issue"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Author    string   `json:"author"`
	Assignees []string `json:"assignees"`
	Labels    []string `json:"labels"`
	URL       string   `json:"url"`
	UpdatedAt string   `json:"updatedAt"`
	Triage    string   `json:"triage"`
}

type Stage1Result struct {
	Triage      Triage      `json:"triage"`
	Signals     []string    `json:"signals"`
	Disposition Disposition `json:"disposition,omitempty"`
	Rule        string      `json:"rule,omitempty"`
}

func (s Stage1Result) Resolved() bool { return s.Disposition != "" }

type Stage2Output struct {
	IsActionable  bool          `json:"is_actionable"`
	NeedsRepro    bool          `json:"needs_repro"`
	Scope         Scope         `json:"scope"`
	SuggestedType SuggestedType `json:"suggested_type"`
	Rationale     string        `json:"rationale"`
}

type Stage3Result struct {
	Disposition Disposition `json:"disposition"`
	Rule        string      `json:"rule"`
}

type Decision struct {
	Schema                  string        `json:"schema"`
	Repository              string        `json:"repository"`
	Issue                   int64         `json:"issue"`
	Revision                string        `json:"revision"`
	SnapshotEvidenceID      string        `json:"snapshot_evidence_id"`
	TriageRulesVersion      string        `json:"triage_rules_version"`
	DispositionRulesVersion string        `json:"disposition_rules_version"`
	Stage1                  Stage1Result  `json:"stage1"`
	Stage2                  *Stage2Output `json:"stage2,omitempty"`
	Stage3                  *Stage3Result `json:"stage3,omitempty"`
}

func (d Decision) FinalDisposition() Disposition {
	if d.Stage1.Resolved() {
		return d.Stage1.Disposition
	}
	if d.Stage3 != nil {
		return d.Stage3.Disposition
	}
	return ""
}

// Canonical is deterministic because Decision is a struct, so encoding/json
// emits its fields in declaration order. The absence of stage 2 and stage 3 on
// a stage 1 decision is the record that the model was not consulted.
func (d Decision) Canonical() ([]byte, error) { return json.Marshal(d) }

func (d *Decision) Validate() error {
	if d == nil {
		return errors.New("decision is required")
	}
	if d.Schema != DecisionSchema {
		return fmt.Errorf("decision schema = %q, want %q", d.Schema, DecisionSchema)
	}
	if strings.TrimSpace(d.Repository) == "" {
		return errors.New("decision repository is required")
	}
	if d.Issue <= 0 {
		return errors.New("decision issue number must be positive")
	}
	if strings.TrimSpace(d.Revision) == "" {
		return errors.New("decision revision is required")
	}
	if strings.TrimSpace(d.SnapshotEvidenceID) == "" {
		return errors.New("decision snapshot evidence id is required")
	}
	if d.TriageRulesVersion == "" || d.DispositionRulesVersion == "" {
		return errors.New("decision rule versions are required")
	}
	if d.Stage1.Resolved() {
		if d.Stage2 != nil || d.Stage3 != nil {
			return errors.New("a stage 1 decision must not carry stage 2 or stage 3")
		}
		if !d.Stage1.Disposition.valid() {
			return fmt.Errorf("decision stage 1 disposition %q is outside the closed set", d.Stage1.Disposition)
		}
		return nil
	}
	if d.Stage2 == nil || d.Stage3 == nil {
		return errors.New("an unresolved stage 1 requires both stage 2 and stage 3")
	}
	if !d.Stage3.Disposition.valid() {
		return fmt.Errorf("decision stage 3 disposition %q is outside the closed set", d.Stage3.Disposition)
	}
	return nil
}
