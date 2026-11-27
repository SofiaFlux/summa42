package project

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/policy"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

const (
	DataClassPublic  = "public"
	DataClassPrivate = "private"
	HostGitHub       = "github"
	HostADO          = "ado"

	applyDomain = "summa42.project.apply.v1"
)

var (
	ErrManifestUnsigned = errors.New("project has no applied signed manifest")
	ErrManifestMismatch = errors.New("project manifest does not match signed hash on record")
	ErrManifestInvalid  = errors.New("project manifest is invalid")

	sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Manifest is the signed declaration in $SUMMA42_HOME/projects/<slug>/project.json.
type Manifest struct {
	SchemaVersion    int              `json:"schema_version"`
	Project          string           `json:"project"`
	MissionStatement string           `json:"mission_statement,omitempty"`
	DataClass        string           `json:"data_class"`
	WorkspaceRoot    string           `json:"workspace_root"`
	Repositories     []RepositorySpec `json:"repositories"`
	Models           ModelSetSpec     `json:"models"`
	Limits           LimitsSpec       `json:"limits"`
}

type RepositorySpec struct {
	ID              string         `json:"id"`
	Host            string         `json:"host"`
	Organization    string         `json:"organization,omitempty"`
	ADOProject      string         `json:"ado_project,omitempty"`
	Repository      string         `json:"repository"`
	ReviewerID      string         `json:"reviewer_id,omitempty"`
	LocalPath       string         `json:"local_path"`
	BaseBranch      string         `json:"base_branch"`
	CredentialFile  string         `json:"credential_file"`
	DataClass       string         `json:"data_class,omitempty"`
	AllowedPrefixes []string       `json:"allowed_prefixes,omitempty"`
	AllowedPaths    []string       `json:"allowed_paths,omitempty"`
	ContextFiles    []string       `json:"context_files,omitempty"`
	ContextMaxBytes int            `json:"context_max_bytes,omitempty"`
	PlanReadMode    bool           `json:"plan_read_mode,omitempty"`
	Validation      ValidationSpec `json:"validation"`
	Sandbox         SandboxSpec    `json:"sandbox,omitempty"`
	Intake          IntakeSpec     `json:"intake,omitempty"`
	CaseGrants      CaseGrantsSpec `json:"case_grants,omitempty"`
}

type ValidationSpec struct {
	Commands       [][]string `json:"commands"`
	User           string     `json:"user"`
	Timeout        string     `json:"timeout,omitempty"`
	ToolchainPaths []string   `json:"toolchain_paths,omitempty"`
}

type SandboxSpec struct {
	Toolchains        []string              `json:"toolchains,omitempty"`
	FetchAllowlist    []string              `json:"fetch_allowlist,omitempty"`
	RegistryAllowlist RegistryAllowlistSpec `json:"registry_allowlist,omitempty"`
	Greenfield        bool                  `json:"greenfield,omitempty"`
}

type RegistryAllowlistSpec struct {
	Mode     string   `json:"mode,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
}

type IntakeSpec struct {
	Maintainers    []string `json:"maintainers,omitempty"`
	BotLogin       string   `json:"bot_login,omitempty"`
	AutoGrantLabel string   `json:"auto_grant_label,omitempty"`
	PollInterval   string   `json:"poll_interval,omitempty"`
}

type CaseGrantsSpec struct {
	Capabilities     []string `json:"capabilities,omitempty"`
	Actions          []string `json:"actions,omitempty"`
	WorkCapabilities []string `json:"work_capabilities,omitempty"`
}

type ModelSetSpec struct {
	Producer              ModelSpec `json:"producer"`
	Reviewer              ModelSpec `json:"reviewer"`
	FallbackReviewersFile string    `json:"fallback_reviewers_file,omitempty"`
}

type ModelSpec struct {
	Binary           string   `json:"binary,omitempty"`
	Args             []string `json:"args,omitempty"`
	EnvAllowlist     []string `json:"env_allowlist,omitempty"`
	CommandCodeModel string   `json:"commandcode_model,omitempty"`
	Profile          string   `json:"profile,omitempty"`
	Model            string   `json:"model,omitempty"`
	ProfileDir       string   `json:"profile_dir,omitempty"`
	User             string   `json:"user"`
	Timeout          string   `json:"timeout,omitempty"`
	AllowPaid        bool     `json:"allow_paid,omitempty"`
	ZDR              bool     `json:"zdr,omitempty"`
	Local            bool     `json:"local,omitempty"`
}

type LimitsSpec struct {
	MaxSteps          int    `json:"max_steps"`
	RemainingBudget   int64  `json:"remaining_budget"`
	MaxRepairs        int    `json:"max_repairs"`
	LeaseDuration     string `json:"lease_duration,omitempty"`
	HeartbeatInterval string `json:"heartbeat_interval,omitempty"`
	MaxWallTime       string `json:"max_wall_time,omitempty"`
}

// EffectiveDataClass returns the repo's data class inherited monotonically from the project.
func (r RepositorySpec) EffectiveDataClass(projectClass string) string {
	if strings.TrimSpace(r.DataClass) == "" {
		return projectClass
	}
	return strings.TrimSpace(r.DataClass)
}

// ParseManifest strictly decodes and validates a project.json payload.
func ParseManifest(raw []byte) (Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: trailing content after JSON object", ErrManifestInvalid)
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m *Manifest) validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("%w: unsupported schema_version %d (want 1)", ErrManifestInvalid, m.SchemaVersion)
	}
	m.Project = strings.TrimSpace(m.Project)
	if !slugPattern.MatchString(m.Project) {
		return fmt.Errorf("%w: invalid project slug %q", ErrManifestInvalid, m.Project)
	}
	m.DataClass = strings.TrimSpace(m.DataClass)
	if m.DataClass == "" {
		m.DataClass = DataClassPublic
	}
	if m.DataClass != DataClassPublic && m.DataClass != DataClassPrivate {
		return fmt.Errorf("%w: invalid data_class %q", ErrManifestInvalid, m.DataClass)
	}
	if !filepath.IsAbs(strings.TrimSpace(m.WorkspaceRoot)) {
		return fmt.Errorf("%w: workspace_root must be absolute", ErrManifestInvalid)
	}
	if len(m.Repositories) == 0 {
		return fmt.Errorf("%w: at least one repository entry is required", ErrManifestInvalid)
	}
	if m.Limits.MaxSteps <= 0 || m.Limits.RemainingBudget <= 0 || m.Limits.MaxRepairs < 0 {
		return fmt.Errorf("%w: positive limits.max_steps and limits.remaining_budget and non-negative max_repairs are required", ErrManifestInvalid)
	}
	for _, optDur := range []string{m.Limits.LeaseDuration, m.Limits.HeartbeatInterval, m.Limits.MaxWallTime} {
		if strings.TrimSpace(optDur) != "" {
			if d, err := time.ParseDuration(optDur); err != nil || d <= 0 {
				return fmt.Errorf("%w: invalid limit duration %q", ErrManifestInvalid, optDur)
			}
		}
	}
	if err := m.Models.validate(); err != nil {
		return err
	}
	hasPrivate := m.DataClass == DataClassPrivate
	seenRepoIDs := map[string]struct{}{}
	for i := range m.Repositories {
		r := &m.Repositories[i]
		if r.ID == "" {
			r.ID = fmt.Sprintf("repo-%d", i+1)
		}
		if _, dup := seenRepoIDs[r.ID]; dup {
			return fmt.Errorf("%w: duplicate repository id %q", ErrManifestInvalid, r.ID)
		}
		seenRepoIDs[r.ID] = struct{}{}
		if err := r.validate(m.DataClass); err != nil {
			return err
		}
		if r.EffectiveDataClass(m.DataClass) == DataClassPrivate {
			hasPrivate = true
		}
	}
	if hasPrivate {
		for label, mod := range map[string]ModelSpec{"producer": m.Models.Producer, "reviewer": m.Models.Reviewer} {
			if err := checkPrivateModel(label, mod); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *RepositorySpec) validate(projectClass string) error {
	r.Host = strings.TrimSpace(r.Host)
	switch r.Host {
	case HostGitHub:
		if !strings.Contains(strings.TrimSpace(r.Repository), "/") {
			return fmt.Errorf("%w: github repo %q requires owner/name", ErrManifestInvalid, r.Repository)
		}
	case HostADO:
		if strings.TrimSpace(r.Organization) == "" || strings.TrimSpace(r.ADOProject) == "" || strings.TrimSpace(r.Repository) == "" {
			return fmt.Errorf("%w: ado repo %q requires organization, ado_project and repository", ErrManifestInvalid, r.ID)
		}
	default:
		return fmt.Errorf("%w: unsupported host %q", ErrManifestInvalid, r.Host)
	}
	if !filepath.IsAbs(strings.TrimSpace(r.LocalPath)) || strings.TrimSpace(r.BaseBranch) == "" {
		return fmt.Errorf("%w: repository %q requires absolute local_path and base_branch", ErrManifestInvalid, r.ID)
	}
	if !filepath.IsAbs(strings.TrimSpace(r.CredentialFile)) {
		return fmt.Errorf("%w: repository %q requires absolute credential_file", ErrManifestInvalid, r.ID)
	}
	cls := r.EffectiveDataClass(projectClass)
	if cls != DataClassPublic && cls != DataClassPrivate {
		return fmt.Errorf("%w: repository %q invalid data_class %q", ErrManifestInvalid, r.ID, cls)
	}
	if projectClass == DataClassPrivate && cls == DataClassPublic {
		return fmt.Errorf("%w: repository %q cannot downgrade private project data_class to public", ErrManifestInvalid, r.ID)
	}
	if len(r.AllowedPaths) > 0 && len(r.AllowedPrefixes) > 0 {
		return fmt.Errorf("%w: repository %q cannot set both allowed_paths and allowed_prefixes", ErrManifestInvalid, r.ID)
	}
	for _, p := range append(slices.Clone(r.AllowedPaths), r.AllowedPrefixes...) {
		if err := validateRelPath(p); err != nil {
			return err
		}
	}
	for _, p := range r.ContextFiles {
		if err := validateRelPath(p); err != nil {
			return err
		}
	}
	if r.Host == HostGitHub && len(r.Intake.Maintainers) > 0 && len(r.CaseGrants.Capabilities) > 0 {
		if !slices.Contains(r.CaseGrants.Capabilities, "github.issue.read") {
			return fmt.Errorf("%w: repository %q case_grants.capabilities must include github.issue.read", ErrManifestInvalid, r.ID)
		}
	}
	for _, a := range r.CaseGrants.Actions {
		if !slices.Contains(r.CaseGrants.Capabilities, a) {
			return fmt.Errorf("%w: repository %q action %q is not in capabilities", ErrManifestInvalid, r.ID, a)
		}
	}
	for _, wc := range r.CaseGrants.WorkCapabilities {
		if !slices.Contains(r.CaseGrants.Capabilities, wc) {
			return fmt.Errorf("%w: repository %q work_capability %q is not in capabilities", ErrManifestInvalid, r.ID, wc)
		}
	}
	return nil
}

func validateRelPath(p string) error {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" || filepath.IsAbs(trimmed) || trimmed == ".." || strings.HasPrefix(trimmed, "../") || strings.Contains(trimmed, "/../") {
		return fmt.Errorf("%w: invalid relative path %q", ErrManifestInvalid, p)
	}
	return nil
}

func (ms ModelSetSpec) validate() error {
	for label, m := range map[string]ModelSpec{"producer": ms.Producer, "reviewer": ms.Reviewer} {
		if strings.TrimSpace(m.Binary) == "" && strings.TrimSpace(m.CommandCodeModel) == "" && strings.TrimSpace(m.Profile) == "" {
			return fmt.Errorf("%w: %s model requires binary, commandcode_model or profile", ErrManifestInvalid, label)
		}
		if strings.TrimSpace(m.User) == "" {
			return fmt.Errorf("%w: %s model requires dedicated OS user", ErrManifestInvalid, label)
		}
		if strings.TrimSpace(m.Timeout) != "" {
			if d, err := time.ParseDuration(m.Timeout); err != nil || d <= 0 {
				return fmt.Errorf("%w: invalid %s timeout %q", ErrManifestInvalid, label, m.Timeout)
			}
		}
	}
	if ms.Producer.User == ms.Reviewer.User {
		return fmt.Errorf("%w: producer and reviewer model users must differ", ErrManifestInvalid)
	}
	if ms.Producer.Binary == ms.Reviewer.Binary && ms.Producer.CommandCodeModel == ms.Reviewer.CommandCodeModel && ms.Producer.Model == ms.Reviewer.Model && ms.Producer.Profile == ms.Reviewer.Profile {
		return fmt.Errorf("%w: producer and reviewer models must differ", ErrManifestInvalid)
	}
	return nil
}

func checkPrivateModel(label string, m ModelSpec) error {
	if m.Local {
		return nil
	}
	if !m.ZDR || strings.HasSuffix(strings.TrimSpace(m.CommandCodeModel), ":free") || strings.HasSuffix(strings.TrimSpace(m.CommandCodeModel), "-free") {
		return fmt.Errorf("%w: %s model must set zdr=true (non-free) or local=true for private data_class", ErrManifestInvalid, label)
	}
	return nil
}

var capabilityRiskCeiling = map[string]string{
	"github.issue.read":    "LOW",
	"ado.work_item.read":   "LOW",
	"ado.build.status":     "LOW",
	"ado.pr.read":          "LOW",
	"workspace.repo.write": "LOW",
	"workspace.test":       "LOW",
	"feedback.export":      "MEDIUM",
	"github.repo.publish":  "MEDIUM",
	"github.pr.create":     "MEDIUM",
	"ado.repo.publish":     "MEDIUM",
	"ado.pr.create":        "MEDIUM",
	"ado.pr.vote":          "MEDIUM",
}

// ValidateManifestWithPolicy enforces that every declared case capability and
// action is permitted under the active OPA PolicyEngine. Unknown capabilities
// classify fail-closed as HIGH risk.
func ValidateManifestWithPolicy(ctx context.Context, m Manifest, eng policy.PolicyEngine, ownerID domain.ID, now time.Time) error {
	if err := m.validate(); err != nil {
		return err
	}
	if eng == nil {
		return errors.New("policy engine is required to validate project manifest")
	}
	for _, repo := range m.Repositories {
		for _, capName := range repo.CaseGrants.Capabilities {
			risk, known := capabilityRiskCeiling[capName]
			if !known {
				risk = "HIGH"
			}
			dec, err := eng.Evaluate(ctx, policy.PolicyInput{
				Risk:             risk,
				AuthorityValid:   true,
				OwnerPrincipalID: ownerID,
				Now:              now.UTC(),
				Attributes: map[string]any{
					"subject":    "manifest_grant",
					"repository": repo.Repository,
					"capability": capName,
				},
			})
			if err != nil {
				return fmt.Errorf("evaluate policy for %s capability %q: %w", repo.ID, capName, err)
			}
			if dec.Outcome == domain.PolicyDeny {
				return fmt.Errorf("%w: grant capability %q for %s exceeds active policy (%v)", ErrManifestInvalid, capName, repo.ID, dec.ReasonCodes)
			}
		}
	}
	return nil
}

// ManifestPath returns $SUMMA42_HOME/projects/<slug>/project.json without resolving symlinks.
func ManifestPath(home, slug string) string {
	return filepath.Join(home, "projects", strings.TrimSpace(slug), "project.json")
}

// LoadManifestFile validates containment (regular file inside the project dir),
// parses project.json strictly, and returns its canonical SHA-256 hex digest.
func LoadManifestFile(home, slug string) (Manifest, string, error) {
	if !slugPattern.MatchString(strings.TrimSpace(slug)) {
		return Manifest{}, "", fmt.Errorf("%w: invalid project slug %q", ErrManifestInvalid, slug)
	}
	projDir := filepath.Join(home, "projects", strings.TrimSpace(slug))
	resolvedDir, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("resolve project directory: %w", err)
	}
	path := filepath.Join(projDir, "project.json")
	info, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("stat project manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Manifest{}, "", fmt.Errorf("%w: %s must be a regular file", ErrManifestInvalid, path)
	}
	resolvedFile, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Dir(resolvedFile) != resolvedDir {
		return Manifest{}, "", fmt.Errorf("%w: manifest path escapes project directory", ErrManifestInvalid)
	}
	raw, err := os.ReadFile(resolvedFile)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read project manifest: %w", err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return Manifest{}, "", err
	}
	if m.Project != strings.TrimSpace(slug) {
		return Manifest{}, "", fmt.Errorf("%w: manifest project %q does not match slug %q", ErrManifestInvalid, m.Project, slug)
	}
	sum := sha256.Sum256(raw)
	return m, hex.EncodeToString(sum[:]), nil
}

// ApplyCommand is an Owner-signed binding of a Project's manifest SHA-256.
type ApplyCommand struct {
	EventID        domain.ID `json:"event_id"`
	ProjectID      domain.ID `json:"project_id"`
	Slug           string    `json:"slug"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	Reason         string    `json:"reason"`
	PrincipalID    domain.ID `json:"principal_id"`
	IssuedAt       time.Time `json:"issued_at"`
	Signature      []byte    `json:"signature"`
}

func ApplySigningMessage(collectiveID domain.ID, cmd ApplyCommand) []byte {
	return []byte(strings.Join([]string{
		applyDomain,
		string(collectiveID),
		string(cmd.EventID),
		string(cmd.ProjectID),
		strings.TrimSpace(cmd.Slug),
		strings.TrimSpace(cmd.ManifestSHA256),
		string(cmd.PrincipalID),
		cmd.IssuedAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(cmd.Reason),
	}, "\n"))
}

func NewApplyCommand(signer Signer, collectiveID, projectID domain.ID, slug, manifestSHA256, reason string, now time.Time) (ApplyCommand, error) {
	if signer == nil {
		return ApplyCommand{}, errors.New("Owner signer is required")
	}
	cmd := ApplyCommand{
		EventID: domain.NewID("projevent"), ProjectID: projectID,
		Slug: strings.TrimSpace(slug), ManifestSHA256: strings.TrimSpace(manifestSHA256),
		Reason: strings.TrimSpace(reason), PrincipalID: signer.PrincipalID(), IssuedAt: now.UTC(),
	}
	if cmd.Reason == "" {
		cmd.Reason = "apply manifest for project " + cmd.Slug
	}
	if err := cmd.validate(); err != nil {
		return ApplyCommand{}, err
	}
	sig, err := signer.Sign(ApplySigningMessage(collectiveID, cmd))
	if err != nil {
		return ApplyCommand{}, fmt.Errorf("sign manifest apply: %w", err)
	}
	cmd.Signature = sig
	return cmd, nil
}

func (c ApplyCommand) validate() error {
	if c.EventID == "" || c.ProjectID == "" || c.PrincipalID == "" || c.IssuedAt.IsZero() {
		return errors.New("apply command needs event_id, project_id, principal_id and issued_at")
	}
	if !slugPattern.MatchString(c.Slug) || !sha256HexPattern.MatchString(c.ManifestSHA256) {
		return errors.New("apply command needs valid slug and 64-hex sha256 digest")
	}
	if strings.TrimSpace(c.Reason) == "" || strings.ContainsAny(c.Reason, "\n\r") {
		return errors.New("apply command needs a one-line reason")
	}
	return nil
}

// ApplyManifestSigned verifies Owner signature, freshness, and replay protection,
// then persists manifest_sha256 on the Project with an immutable APPLY_MANIFEST event.
func (s *Service) ApplyManifestSigned(ctx context.Context, cmd ApplyCommand) (Project, error) {
	if err := s.configured(); err != nil {
		return Project{}, err
	}
	cmd.Slug = strings.TrimSpace(cmd.Slug)
	cmd.ManifestSHA256 = strings.TrimSpace(cmd.ManifestSHA256)
	cmd.Reason = strings.TrimSpace(cmd.Reason)
	if err := cmd.validate(); err != nil {
		return Project{}, err
	}
	collectiveID, ownerID, publicKey, err := s.loadOwnerKey(ctx)
	if err != nil {
		return Project{}, err
	}
	if cmd.PrincipalID != domain.ID(ownerID) || len(publicKey) != ed25519.PublicKeySize ||
		len(cmd.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, ApplySigningMessage(domain.ID(collectiveID), cmd), cmd.Signature) {
		return Project{}, fmt.Errorf("%w: apply signature does not verify", ErrUnauthorized)
	}
	now := s.clock.Now().UTC()
	if age := now.Sub(cmd.IssuedAt); age > CommandMaxAge || age < -CommandMaxAge {
		return Project{}, ErrExpired
	}
	stamp := domain.FormatTime(now)
	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var seen int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM project_events WHERE event_id = ?`, cmd.EventID).Scan(&seen)
		if err == nil {
			return ErrReplayed
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var dbID, dbState string
		err = tx.QueryRowContext(ctx, `SELECT project_id, state FROM projects WHERE project_id = ? AND slug = ?`, cmd.ProjectID, cmd.Slug).Scan(&dbID, &dbState)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProjectNotFound
		}
		if err != nil {
			return err
		}
		if State(dbState) == StateArchived {
			return errors.New("cannot apply manifest to archived project")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET manifest_sha256 = ?, updated_at = ? WHERE project_id = ?`,
			cmd.ManifestSHA256, stamp, cmd.ProjectID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO project_events(event_id, project_id, action, from_state, to_state, manifest_sha256, owner_principal_id, reason, issued_at, signature, created_at)
			VALUES (?, ?, 'APPLY_MANIFEST', ?, ?, ?, ?, ?, ?, ?, ?)`,
			cmd.EventID, cmd.ProjectID, dbState, dbState, cmd.ManifestSHA256, cmd.PrincipalID, cmd.Reason, domain.FormatTime(cmd.IssuedAt), cmd.Signature, stamp)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.Get(ctx, string(cmd.ProjectID))
}

// VerifyActiveManifest checks recorded signature hash, file digest equality,
// strict schema validity, and active policy compliance.
func (s *Service) VerifyActiveManifest(ctx context.Context, home, slug string, eng policy.PolicyEngine) (Project, Manifest, string, error) {
	proj, err := s.Get(ctx, slug)
	if err != nil {
		return Project{}, Manifest{}, "", err
	}
	if proj.State == StateArchived {
		return Project{}, Manifest{}, "", fmt.Errorf("project %s is archived", proj.Slug)
	}
	if strings.TrimSpace(proj.ManifestSHA256) == "" {
		return Project{}, Manifest{}, "", fmt.Errorf("%w: %s", ErrManifestUnsigned, proj.Slug)
	}
	m, digest, err := LoadManifestFile(home, proj.Slug)
	if err != nil {
		return Project{}, Manifest{}, "", err
	}
	if digest != proj.ManifestSHA256 {
		return Project{}, Manifest{}, "", fmt.Errorf("%w: file=%s recorded=%s", ErrManifestMismatch, digest, proj.ManifestSHA256)
	}
	_, ownerStr, _, err := s.loadOwnerKey(ctx)
	if err != nil {
		return Project{}, Manifest{}, "", err
	}
	if err := ValidateManifestWithPolicy(ctx, m, eng, domain.ID(ownerStr), s.clock.Now().UTC()); err != nil {
		return Project{}, Manifest{}, "", err
	}
	return proj, m, digest, nil
}

// LoadActivePolicyEngine compiles the persisted active OPA policy set from SQLite.
func LoadActivePolicyEngine(ctx context.Context, store *state.Store) (policy.PolicyEngine, domain.ID, error) {
	if store == nil || store.DB() == nil {
		return nil, "", errors.New("state store is required")
	}
	var id, moduleName, moduleText, policyHash string
	err := store.DB().QueryRowContext(ctx,
		`SELECT policy_set_id, module_name, module, policy_hash FROM policy_sets WHERE active = 1`,
	).Scan(&id, &moduleName, &moduleText, &policyHash)
	if err != nil {
		return nil, "", fmt.Errorf("load active policy set: %w", err)
	}
	var ownerID domain.ID
	if err := store.DB().QueryRowContext(ctx, `SELECT owner_principal_id FROM collective_metadata WHERE singleton = 1`).Scan(&ownerID); err != nil {
		return nil, "", fmt.Errorf("load owner principal for policy validation: %w", err)
	}
	if strings.TrimSpace(string(ownerID)) == "" {
		return nil, "", errors.New("owner principal id is empty in collective metadata")
	}
	return policy.NewOPAEngine(policy.OPAConfig{
		ModuleName:    moduleName,
		Module:        moduleText,
		PolicySetID:   domain.ID(id),
		PolicySetHash: policyHash,
	}), ownerID, nil
}

// FormatManifestSummary returns a concise human-readable summary for Owner apply confirmation.
func FormatManifestSummary(m Manifest, digest string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project %s (schema_version=%d data_class=%s sha256=%s)\n", m.Project, m.SchemaVersion, m.DataClass, digest)
	fmt.Fprintf(&b, "Workspace root: %s\nLimits: max_steps=%d remaining_budget=%d max_repairs=%d\n", m.WorkspaceRoot, m.Limits.MaxSteps, m.Limits.RemainingBudget, m.Limits.MaxRepairs)
	for _, r := range m.Repositories {
		fmt.Fprintf(&b, "Repository %s (%s): %s base=%s data_class=%s grants=%v\n",
			r.ID, r.Host, r.Repository, r.BaseBranch, r.EffectiveDataClass(m.DataClass), r.CaseGrants.Capabilities)
	}
	return b.String()
}
