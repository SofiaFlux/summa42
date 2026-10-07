package ghtriage

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"path/filepath"
	"strings"
)

const PlanSchemaV2 = "github.issue.plan.v2"
const KindSourceContext = "github.issue.source.context"

type GroundingConfig struct {
	Source             repoworkspace.Config
	AllowedPaths       []string
	ValidationCommands [][]string
}

func (cfg GroundingConfig) Validate() error {
	if !filepath.IsAbs(cfg.Source.LocalPath) || !repoworkspace.FullCommit(cfg.Source.Commit) || strings.Count(cfg.Source.Repository, "/") != 1 || len(cfg.Source.Paths) < 1 || len(cfg.Source.Paths) > repoworkspace.MaxFiles {
		return errors.New("explicit repository, full commit and context selection required")
	}
	seen := map[string]bool{}
	for _, p := range cfg.Source.Paths {
		if !repoworkspace.SafePath(p) || seen[p] {
			return errors.New("unsafe or duplicate context selection")
		}
		seen[p] = true
	}
	return validateContract(cfg.AllowedPaths, cfg.ValidationCommands)
}

type PlanSource struct {
	BaseSHA            string     `json:"base_sha"`
	ContextEvidenceID  domain.ID  `json:"context_evidence_id"`
	ContextHash        string     `json:"context_hash"`
	AllowedPaths       []string   `json:"allowed_paths"`
	ValidationCommands [][]string `json:"validation_commands"`
}
type GroundedInput struct {
	Context  repoworkspace.Snapshot `json:"context"`
	Contract PlanSource             `json:"contract"`
}

func validateContract(paths []string, commands [][]string) error {
	if len(paths) < 1 || len(paths) > 64 || len(commands) < 1 || len(commands) > 8 {
		return errors.New("bounded allowed paths and validation commands required")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if !repoworkspace.SafePath(p) || seen[p] {
			return errors.New("unsafe or duplicate allowed path")
		}
		seen[p] = true
	}
	total := 0
	for _, argv := range commands {
		if len(argv) < 1 || len(argv) > 32 {
			return errors.New("validation command must be a bounded argv array")
		}
		for _, arg := range argv {
			if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
				return errors.New("empty or invalid validation argument")
			}
			total += len(arg)
		}
	}
	if total > 8192 {
		return errors.New("validation arguments exceed limit")
	}
	return nil
}
func (s PlanSource) Validate() error {
	hash, err := hex.DecodeString(s.ContextHash)
	if !repoworkspace.FullCommit(s.BaseSHA) || s.ContextEvidenceID == "" || err != nil || len(hash) != 32 || strings.ToLower(s.ContextHash) != s.ContextHash {
		return errors.New("pinned source citations are required")
	}
	return validateContract(s.AllowedPaths, s.ValidationCommands)
}
func cloneCommands(commands [][]string) [][]string {
	out := make([][]string, len(commands))
	for i, args := range commands {
		out[i] = append([]string(nil), args...)
	}
	return out
}

// SetGrounding freezes configuration before Tick. It must not race a running Tick.
func (p *Planner) SetGrounding(cfg GroundingConfig) error {
	if p == nil {
		return errors.New("planner is not configured")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg.Source.Paths = append([]string(nil), cfg.Source.Paths...)
	cfg.AllowedPaths = append([]string(nil), cfg.AllowedPaths...)
	cfg.ValidationCommands = cloneCommands(cfg.ValidationCommands)
	p.grounding = &cfg
	return nil
}
func (p *Planner) captureGrounding(ctx context.Context, repository string) (*GroundedInput, error) {
	if p.grounding == nil {
		return nil, nil
	}
	cfg := p.grounding
	if cfg.Source.Repository != repository {
		return nil, errors.New("configured source repository differs from issue repository")
	}
	snapshot, err := repoworkspace.Capture(ctx, cfg.Source)
	if err != nil {
		return nil, err
	}
	raw, err := snapshot.Canonical()
	if err != nil {
		return nil, err
	}
	object, err := p.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: "application/json", Kind: KindSourceContext})
	if err != nil {
		return nil, err
	}
	source := PlanSource{BaseSHA: snapshot.Commit, ContextEvidenceID: object.ID, ContextHash: object.ContentHash, AllowedPaths: append([]string(nil), cfg.AllowedPaths...), ValidationCommands: cloneCommands(cfg.ValidationCommands)}
	return &GroundedInput{Context: snapshot, Contract: source}, nil
}
