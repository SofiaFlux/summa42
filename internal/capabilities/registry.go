package capabilities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type Provider interface {
	Name() string
	Call(ctx context.Context, capability string, request any) (any, error)
}

type AssessableProvider interface {
	Provider
	Advertise(ctx context.Context) ([]Definition, error)
	Probe(ctx context.Context, definition Definition) (ProbeResult, error)
}

type Skill struct {
	Name    string
	Version string
}

type Access struct {
	Provider string
	Context  string
}

type AuthorityRequirement struct {
	Capabilities []string
}

type EnvironmentRequirement struct {
	MinimumEnforcement domain.EnforcementLevel
}

type Definition struct {
	ID          domain.ID
	Skill       Skill
	Access      Access
	Authority   AuthorityRequirement
	Environment EnvironmentRequirement
}

type Health string

const (
	HealthHealthy   Health = "HEALTHY"
	HealthDegraded  Health = "DEGRADED"
	HealthUnhealthy Health = "UNHEALTHY"
)

type ProbeResult struct {
	Enforcement  domain.EnforcementLevel
	Evidence     []string
	CostMetadata map[string]any
	Health       Health
	Available    bool
}

type Assessment struct {
	ID            domain.ID
	CapabilityID  domain.ID
	Provider      string
	Enforcement   domain.EnforcementLevel
	AccessContext string
	AssessedAt    time.Time
	Evidence      []string
	CostMetadata  map[string]any
	Health        Health
	Available     bool
}

type Registry struct {
	store     *state.Store
	clock     clock.Clock
	execution *execution.Service
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry(store *state.Store, clk clock.Clock, executionSvc *execution.Service, providers ...Provider) *Registry {
	registered := make(map[string]Provider, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		name := strings.TrimSpace(provider.Name())
		if name != "" {
			registered[name] = provider
		}
	}
	return &Registry{store: store, clock: clk, execution: executionSvc, providers: registered}
}

func (r *Registry) Provider(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[strings.TrimSpace(name)]
	return p, ok
}

func (r *Registry) Register(ctx context.Context, definition Definition) error {
	if err := r.configured(); err != nil {
		return err
	}
	definition, err := r.validateDefinition(definition)
	if err != nil {
		return err
	}
	authorityJSON, err := json.Marshal(definition.Authority.Capabilities)
	if err != nil {
		return err
	}
	now := r.clock.Now().UTC()
	return r.store.WithTx(ctx, func(tx *sql.Tx) error {
		return r.registerDefinitionTx(ctx, tx, definition, authorityJSON, now)
	})
}

func (r *Registry) AssessProvider(ctx context.Context, providerName string) ([]Assessment, error) {
	if err := r.configured(); err != nil {
		return nil, err
	}
	providerName = strings.TrimSpace(providerName)
	provider, ok := r.Provider(providerName)
	if !ok || provider == nil {
		return nil, fmt.Errorf("capability provider %q is not registered", providerName)
	}
	assessor, ok := provider.(AssessableProvider)
	if !ok {
		return nil, fmt.Errorf("capability provider %q does not support assessment", providerName)
	}
	advertised, err := assessor.Advertise(ctx)
	if err != nil {
		return nil, fmt.Errorf("advertise capabilities: %w", err)
	}
	if len(advertised) == 0 {
		return nil, errors.New("provider advertised no primitive capabilities")
	}

	assessments := make([]Assessment, 0, len(advertised))
	for _, raw := range advertised {
		definition, err := r.validateDefinition(raw)
		if err != nil {
			return nil, err
		}
		if definition.Access.Provider != providerName {
			return nil, fmt.Errorf("advertised capability %q names provider %q, want %q", definition.Skill.Name, definition.Access.Provider, providerName)
		}
		probe, err := assessor.Probe(ctx, definition)
		if err != nil {
			return nil, fmt.Errorf("probe capability %q: %w", definition.Skill.Name, err)
		}
		if err := validateProbeResult(probe); err != nil {
			return nil, fmt.Errorf("probe capability %q: %w", definition.Skill.Name, err)
		}
		now := r.clock.Now().UTC()
		authorityJSON, err := json.Marshal(definition.Authority.Capabilities)
		if err != nil {
			return nil, err
		}
		evidenceJSON, err := json.Marshal(probe.Evidence)
		if err != nil {
			return nil, err
		}
		costJSON, err := json.Marshal(probe.CostMetadata)
		if err != nil {
			return nil, err
		}
		assessment := Assessment{
			ID:            domain.NewID("capassessment"),
			CapabilityID:  definition.ID,
			Provider:      providerName,
			Enforcement:   probe.Enforcement,
			AccessContext: definition.Access.Context,
			AssessedAt:    now,
			Evidence:      append([]string(nil), probe.Evidence...),
			CostMetadata:  cloneMap(probe.CostMetadata),
			Health:        probe.Health,
			Available:     probe.Available,
		}
		err = r.store.WithTx(ctx, func(tx *sql.Tx) error {
			if err := r.registerDefinitionTx(ctx, tx, definition, authorityJSON, now); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO capability_assessments(
					assessment_id, capability_id, provider, enforcement_level, access_context,
					assessed_at, evidence_json, cost_metadata_json, health, available
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				assessment.ID, assessment.CapabilityID, assessment.Provider, assessment.Enforcement,
				assessment.AccessContext, formatTime(assessment.AssessedAt), string(evidenceJSON), string(costJSON),
				assessment.Health, boolInt(assessment.Available),
			)
			return err
		})
		if err != nil {
			return nil, err
		}
		assessments = append(assessments, assessment)
	}
	return assessments, nil
}

func (r *Registry) LatestAssessment(ctx context.Context, semanticName string) (Assessment, error) {
	if err := r.configured(); err != nil {
		return Assessment{}, err
	}
	semanticName = strings.TrimSpace(semanticName)
	if semanticName == "" {
		return Assessment{}, errors.New("semantic capability name is required")
	}
	return loadLatestAssessment(ctx, r.store.DB(), semanticName)
}

func loadLatestAssessment(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, semanticName string) (Assessment, error) {
	var assessment Assessment
	var assessedAt string
	var evidenceJSON, costJSON string
	var available int
	err := q.QueryRowContext(ctx, `
		SELECT a.assessment_id, a.capability_id, a.provider, a.enforcement_level, a.access_context,
		       a.assessed_at, a.evidence_json, a.cost_metadata_json, a.health, a.available
		FROM capability_assessments a
		JOIN capability_definitions d ON d.capability_id = a.capability_id
		WHERE d.semantic_name = ?
		ORDER BY a.assessed_at DESC, a.rowid DESC
		LIMIT 1`, semanticName,
	).Scan(
		&assessment.ID, &assessment.CapabilityID, &assessment.Provider, &assessment.Enforcement,
		&assessment.AccessContext, &assessedAt, &evidenceJSON, &costJSON, &assessment.Health, &available,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Assessment{}, fmt.Errorf("capability %q has no assessment", semanticName)
	}
	if err != nil {
		return Assessment{}, err
	}
	assessment.AssessedAt, err = time.Parse(time.RFC3339Nano, assessedAt)
	if err != nil {
		return Assessment{}, fmt.Errorf("parse capability assessment time: %w", err)
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &assessment.Evidence); err != nil {
		return Assessment{}, fmt.Errorf("decode capability assessment evidence: %w", err)
	}
	if err := json.Unmarshal([]byte(costJSON), &assessment.CostMetadata); err != nil {
		return Assessment{}, fmt.Errorf("decode capability assessment cost metadata: %w", err)
	}
	assessment.Available = available == 1
	return assessment, nil
}

func (r *Registry) validateDefinition(definition Definition) (Definition, error) {
	definition.ID = domain.ID(strings.TrimSpace(string(definition.ID)))
	definition.Skill.Name = strings.TrimSpace(definition.Skill.Name)
	definition.Skill.Version = strings.TrimSpace(definition.Skill.Version)
	definition.Access.Provider = strings.TrimSpace(definition.Access.Provider)
	definition.Access.Context = strings.TrimSpace(definition.Access.Context)
	definition.Authority.Capabilities = normalizeStrings(definition.Authority.Capabilities)
	if definition.ID == "" || definition.Skill.Name == "" || definition.Skill.Version == "" || definition.Access.Provider == "" || definition.Access.Context == "" {
		return Definition{}, errors.New("capability id, skill name/version, provider, and access context are required")
	}
	if _, ok := r.Provider(definition.Access.Provider); !ok {
		return Definition{}, fmt.Errorf("capability provider %q is not registered", definition.Access.Provider)
	}
	if !validEnforcement(definition.Environment.MinimumEnforcement) {
		return Definition{}, fmt.Errorf("invalid minimum enforcement %q", definition.Environment.MinimumEnforcement)
	}
	if len(definition.Authority.Capabilities) == 0 {
		return Definition{}, errors.New("capability authority requirements are required")
	}
	return definition, nil
}

func (r *Registry) registerDefinitionTx(ctx context.Context, tx *sql.Tx, definition Definition, authorityJSON []byte, now time.Time) error {
	var existingVersion string
	err := tx.QueryRowContext(ctx,
		`SELECT semantic_version FROM capability_definitions WHERE capability_id = ?`, definition.ID,
	).Scan(&existingVersion)
	if err == nil {
		if existingVersion != definition.Skill.Version {
			return fmt.Errorf("capability id %q already belongs to semantic version %q", definition.ID, existingVersion)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE capability_definitions SET active = 0, updated_at = ? WHERE semantic_name = ? AND capability_id <> ? AND active = 1`,
			formatTime(now), definition.Skill.Name, definition.ID,
		); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE capability_definitions
			SET provider = ?, access_context = ?, authority_requirements_json = ?, minimum_enforcement = ?, active = 1, updated_at = ?
			WHERE capability_id = ?`,
			definition.Access.Provider, definition.Access.Context, string(authorityJSON), definition.Environment.MinimumEnforcement,
			formatTime(now), definition.ID,
		)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE capability_definitions SET active = 0, updated_at = ? WHERE semantic_name = ? AND active = 1`,
		formatTime(now), definition.Skill.Name,
	); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO capability_definitions(
			capability_id, semantic_name, semantic_version, provider, access_context,
			authority_requirements_json, minimum_enforcement, active, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		definition.ID, definition.Skill.Name, definition.Skill.Version, definition.Access.Provider,
		definition.Access.Context, string(authorityJSON), definition.Environment.MinimumEnforcement,
		formatTime(now), formatTime(now),
	)
	return err
}

func (r *Registry) definitionByNameTx(ctx context.Context, tx *sql.Tx, name string) (Definition, error) {
	var definition Definition
	var authorityJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT capability_id, semantic_name, semantic_version, provider, access_context,
		       authority_requirements_json, minimum_enforcement
		FROM capability_definitions
		WHERE semantic_name = ? AND active = 1`, strings.TrimSpace(name),
	).Scan(
		&definition.ID, &definition.Skill.Name, &definition.Skill.Version, &definition.Access.Provider,
		&definition.Access.Context, &authorityJSON, &definition.Environment.MinimumEnforcement,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, fmt.Errorf("active capability %q not found", name)
	}
	if err != nil {
		return Definition{}, err
	}
	if err := json.Unmarshal([]byte(authorityJSON), &definition.Authority.Capabilities); err != nil {
		return Definition{}, fmt.Errorf("decode authority requirements for %s: %w", definition.ID, err)
	}
	return definition, nil
}

func (r *Registry) definitionByIDTx(ctx context.Context, tx *sql.Tx, id domain.ID) (Definition, error) {
	var definition Definition
	var authorityJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT capability_id, semantic_name, semantic_version, provider, access_context,
		       authority_requirements_json, minimum_enforcement
		FROM capability_definitions
		WHERE capability_id = ?`, id,
	).Scan(
		&definition.ID, &definition.Skill.Name, &definition.Skill.Version, &definition.Access.Provider,
		&definition.Access.Context, &authorityJSON, &definition.Environment.MinimumEnforcement,
	)
	if err != nil {
		return Definition{}, err
	}
	if err := json.Unmarshal([]byte(authorityJSON), &definition.Authority.Capabilities); err != nil {
		return Definition{}, fmt.Errorf("decode authority requirements for %s: %w", definition.ID, err)
	}
	return definition, nil
}

func (r *Registry) configured() error {
	if r == nil || r.store == nil || r.clock == nil || r.execution == nil {
		return errors.New("capability registry is not configured")
	}
	return nil
}

func validateProbeResult(probe ProbeResult) error {
	if !validEnforcement(probe.Enforcement) {
		return fmt.Errorf("invalid observed enforcement %q", probe.Enforcement)
	}
	if len(normalizeStrings(probe.Evidence)) == 0 {
		return errors.New("assessment requires probe evidence")
	}
	if probe.CostMetadata == nil {
		return errors.New("assessment requires cost metadata")
	}
	switch probe.Health {
	case HealthHealthy, HealthDegraded, HealthUnhealthy:
	default:
		return fmt.Errorf("invalid capability health %q", probe.Health)
	}
	return nil
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validEnforcement(level domain.EnforcementLevel) bool {
	switch level {
	case domain.EnforcementUnenforced, domain.EnforcementPartial, domain.EnforcementEnforced:
		return true
	default:
		return false
	}
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string { return domain.FormatTime(value) }
