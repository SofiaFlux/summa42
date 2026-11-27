package project

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// State represents the lifecycle state of a Project.
type State string

const (
	StateActive   State = "ACTIVE"
	StatePaused   State = "PAUSED"
	StateArchived State = "ARCHIVED"

	DefaultProjectID         domain.ID = "project_default"
	DefaultProjectSlug       string    = "default"
	CollectiveRootEnvelopeID domain.ID = "envelope_collective_root"
	DefaultProjectEnvelopeID domain.ID = "envelope_project_default"

	CommandMaxAge = 5 * time.Minute
	createDomain  = "summa42.project.create.v1"
)

var (
	ErrUnauthorized    = errors.New("project command is not signed by the Collective's Owner")
	ErrExpired         = errors.New("signed project command is not recent")
	ErrReplayed        = errors.New("signed project command was already applied")
	ErrProjectExists   = errors.New("project slug already exists")
	ErrProjectNotFound = errors.New("project not found")

	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,47}$`)
)

// Signer signs Project governance commands for one principal.
type Signer interface {
	PrincipalID() domain.ID
	Sign(message []byte) ([]byte, error)
}

// Project is a kernel scope inside the Collective.
type Project struct {
	ID             domain.ID  `json:"project_id"`
	Slug           string     `json:"slug"`
	Name           string     `json:"name"`
	State          State      `json:"state"`
	EnvelopeID     domain.ID  `json:"envelope_id"`
	ManifestSHA256 string     `json:"manifest_sha256,omitempty"`
	AutonomyStep   string     `json:"autonomy_step"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ArchivedAt     *time.Time `json:"archived_at,omitempty"`
}

// CreateCommand is an Owner-signed instruction to register a new Project.
type CreateCommand struct {
	EventID     domain.ID `json:"event_id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Reason      string    `json:"reason"`
	PrincipalID domain.ID `json:"principal_id"`
	IssuedAt    time.Time `json:"issued_at"`
	Signature   []byte    `json:"signature"`
}

// CreateSigningMessage builds the canonical byte sequence signed by the Owner key.
func CreateSigningMessage(collectiveID domain.ID, cmd CreateCommand) []byte {
	return []byte(strings.Join([]string{
		createDomain,
		string(collectiveID),
		string(cmd.EventID),
		strings.TrimSpace(cmd.Slug),
		strings.TrimSpace(cmd.Name),
		string(cmd.PrincipalID),
		cmd.IssuedAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(cmd.Reason),
	}, "\n"))
}

// NewCreateCommand validates and signs a Project creation command for collectiveID.
func NewCreateCommand(signer Signer, collectiveID domain.ID, slug, name, reason string, now time.Time) (CreateCommand, error) {
	if signer == nil {
		return CreateCommand{}, errors.New("Owner signer is required")
	}
	if strings.TrimSpace(string(collectiveID)) == "" {
		return CreateCommand{}, errors.New("collective id is required")
	}
	cmd := CreateCommand{
		EventID:     domain.NewID("projevent"),
		Slug:        strings.TrimSpace(slug),
		Name:        strings.TrimSpace(name),
		Reason:      strings.TrimSpace(reason),
		PrincipalID: signer.PrincipalID(),
		IssuedAt:    now.UTC(),
	}
	if cmd.Name == "" {
		cmd.Name = cmd.Slug
	}
	if cmd.Reason == "" {
		cmd.Reason = "create project " + cmd.Slug
	}
	if err := cmd.validate(); err != nil {
		return CreateCommand{}, err
	}
	sig, err := signer.Sign(CreateSigningMessage(collectiveID, cmd))
	if err != nil {
		return CreateCommand{}, fmt.Errorf("sign project create: %w", err)
	}
	cmd.Signature = sig
	return cmd, nil
}

func (c CreateCommand) validate() error {
	if c.EventID == "" || c.PrincipalID == "" || c.IssuedAt.IsZero() {
		return errors.New("project command needs an event id, principal and issue time")
	}
	if !slugPattern.MatchString(c.Slug) {
		return fmt.Errorf("project slug %q must be 2 to 48 characters of a-z, 0-9, and -, starting with a letter or digit", c.Slug)
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("project name is required")
	}
	if strings.TrimSpace(c.Reason) == "" || strings.ContainsAny(c.Reason, "\n\r") {
		return errors.New("project command needs a one-line reason")
	}
	return nil
}

// Service manages durable Project scope records in SQLite.
type Service struct {
	store *state.Store
	clock clock.Clock
}

func New(store *state.Store, clk clock.Clock) *Service {
	return &Service{store: store, clock: clk}
}

// CreateSigned verifies the Collective Owner's Ed25519 signature, freshness,
// and replay status, then atomically creates the Project, its zero-limit child
// envelope under envelope_collective_root, and its immutable audit event.
func (s *Service) CreateSigned(ctx context.Context, cmd CreateCommand) (Project, error) {
	if err := s.configured(); err != nil {
		return Project{}, err
	}
	cmd.Slug = strings.TrimSpace(cmd.Slug)
	cmd.Name = strings.TrimSpace(cmd.Name)
	cmd.Reason = strings.TrimSpace(cmd.Reason)
	if err := cmd.validate(); err != nil {
		return Project{}, err
	}

	var collectiveID, ownerID string
	var publicKey []byte
	err := s.store.DB().QueryRowContext(ctx, `
		SELECT m.collective_id, m.owner_principal_id, p.public_key
		FROM collective_metadata m
		JOIN principals p ON p.principal_id = m.owner_principal_id
		WHERE m.singleton = 1`,
	).Scan(&collectiveID, &ownerID, &publicKey)
	if err != nil {
		return Project{}, fmt.Errorf("load the Collective's Owner: %w", err)
	}
	if cmd.PrincipalID != domain.ID(ownerID) {
		return Project{}, fmt.Errorf("%w: principal %q is not the Owner", ErrUnauthorized, cmd.PrincipalID)
	}
	if len(publicKey) != ed25519.PublicKeySize || len(cmd.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, CreateSigningMessage(domain.ID(collectiveID), cmd), cmd.Signature) {
		return Project{}, fmt.Errorf("%w: signature does not verify", ErrUnauthorized)
	}
	now := s.clock.Now().UTC()
	if age := now.Sub(cmd.IssuedAt); age > CommandMaxAge || age < -CommandMaxAge {
		return Project{}, ErrExpired
	}

	projectID := domain.ID("project_" + strings.ReplaceAll(cmd.Slug, "-", "_"))
	envelopeID := domain.ID("envelope_project_" + strings.ReplaceAll(cmd.Slug, "-", "_"))
	stamp := domain.FormatTime(now)

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var seen int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_events WHERE event_id = ?`, cmd.EventID).Scan(&seen)
		if err == nil {
			return ErrReplayed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check project event replay: %w", err)
		}

		err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE slug = ? OR project_id = ?`, cmd.Slug, projectID).Scan(&seen)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrProjectExists, cmd.Slug)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO resource_envelopes(envelope_id, parent_envelope_id, hard_limit, created_at) VALUES (?, ?, 0, ?)`,
			envelopeID, CollectiveRootEnvelopeID, stamp,
		); err != nil {
			return fmt.Errorf("create project child envelope: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO projects(project_id, slug, name, state, envelope_id, manifest_sha256, autonomy_step, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, '', 'A1', ?, ?)`,
			projectID, cmd.Slug, cmd.Name, StateActive, envelopeID, stamp, stamp,
		); err != nil {
			return fmt.Errorf("insert project: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE resource_envelopes SET project_id = ? WHERE envelope_id = ?`,
			projectID, envelopeID,
		); err != nil {
			return fmt.Errorf("bind project envelope: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_events(event_id, project_id, action, from_state, to_state, manifest_sha256, owner_principal_id, reason, issued_at, signature, created_at)
			VALUES (?, ?, 'CREATE', '', ?, '', ?, ?, ?, ?, ?)`,
			cmd.EventID, projectID, StateActive, cmd.PrincipalID, cmd.Reason, domain.FormatTime(cmd.IssuedAt), cmd.Signature, stamp,
		); err != nil {
			return fmt.Errorf("record project create event: %w", err)
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return s.Get(ctx, string(projectID))
}

// Get loads a Project by ID or slug. An empty ref resolves to the default Project.
func (s *Service) Get(ctx context.Context, ref string) (Project, error) {
	if err := s.configured(); err != nil {
		return Project{}, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = string(DefaultProjectID)
	}
	return scanProject(s.store.DB().QueryRowContext(ctx, `
		SELECT project_id, slug, name, state, envelope_id, manifest_sha256, autonomy_step, created_at, updated_at, archived_at
		FROM projects
		WHERE project_id = ? OR slug = ?`, ref, ref))
}

// List returns all Projects ordered deterministically by creation time and slug.
func (s *Service) List(ctx context.Context) ([]Project, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT project_id, slug, name, state, envelope_id, manifest_sha256, autonomy_step, created_at, updated_at, archived_at
		FROM projects
		ORDER BY created_at, slug`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (Project, error) {
	var p Project
	var createdS, updatedS string
	var archivedS sql.NullString
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.State, &p.EnvelopeID, &p.ManifestSHA256, &p.AutonomyStep, &createdS, &updatedS, &archivedS)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrProjectNotFound
	}
	if err != nil {
		return Project{}, err
	}
	if p.CreatedAt, err = time.Parse(time.RFC3339Nano, createdS); err != nil {
		return Project{}, fmt.Errorf("parse project created_at: %w", err)
	}
	if p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedS); err != nil {
		return Project{}, fmt.Errorf("parse project updated_at: %w", err)
	}
	if archivedS.Valid && archivedS.String != "" {
		t, err := time.Parse(time.RFC3339Nano, archivedS.String)
		if err != nil {
			return Project{}, fmt.Errorf("parse project archived_at: %w", err)
		}
		p.ArchivedAt = &t
	}
	return p, nil
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil {
		return errors.New("project service is not configured")
	}
	return nil
}
