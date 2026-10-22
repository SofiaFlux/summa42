package ghtriage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const PlanSchemaV2 = "github.issue.plan.v2"
const KindSourceContext = "github.issue.source.context"

// GroundingConfig is either exact (AllowedPaths plus Source.Paths) or prefix mode
// (AllowedPrefixes; a per-issue model selection picks files under them, Source.Paths pins extras).
type GroundingConfig struct {
	Source             repoworkspace.Config
	AllowedPaths       []string
	AllowedPrefixes    []string
	ValidationCommands [][]string
}

func (cfg GroundingConfig) Validate() error {
	prefixMode := len(cfg.AllowedPrefixes) > 0
	if !filepath.IsAbs(cfg.Source.LocalPath) || !repoworkspace.FullCommit(cfg.Source.Commit) || strings.Count(cfg.Source.Repository, "/") != 1 || (!prefixMode && len(cfg.Source.Paths) < 1) || len(cfg.Source.Paths) > repoworkspace.MaxFiles {
		return errors.New("explicit repository, full commit and context selection required")
	}
	seen := map[string]bool{}
	for _, p := range cfg.Source.Paths {
		if !repoworkspace.SafePath(p) || seen[p] {
			return errors.New("unsafe or duplicate context selection")
		}
		seen[p] = true
	}
	if !prefixMode {
		return validateContract(cfg.AllowedPaths, cfg.ValidationCommands)
	}
	if len(cfg.AllowedPaths) > 0 {
		return errors.New("allowed prefixes and exact allowed paths are mutually exclusive")
	}
	if len(cfg.AllowedPrefixes) > 16 {
		return errors.New("at most 16 allowed prefixes")
	}
	seen = map[string]bool{}
	for _, prefix := range cfg.AllowedPrefixes {
		dir, ok := strings.CutSuffix(prefix, "/")
		if !ok || !repoworkspace.SafePath(dir) || seen[prefix] {
			return errors.New("allowed prefix must be a safe, unique directory ending in /")
		}
		seen[prefix] = true
	}
	return validateCommands(cfg.ValidationCommands)
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
	if len(paths) < 1 || len(paths) > 64 {
		return errors.New("bounded allowed paths and validation commands required")
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if !repoworkspace.SafePath(p) || seen[p] {
			return errors.New("unsafe or duplicate allowed path")
		}
		seen[p] = true
	}
	return validateCommands(commands)
}

// MaxValidationCommands bounds the validation argv list of a plan.
const MaxValidationCommands = 8

func validateCommands(commands [][]string) error {
	if len(commands) < 1 || len(commands) > MaxValidationCommands {
		return errors.New("bounded allowed paths and validation commands required")
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
	cfg.AllowedPrefixes = append([]string(nil), cfg.AllowedPrefixes...)
	cfg.ValidationCommands = cloneCommands(cfg.ValidationCommands)
	p.grounding = &cfg
	return nil
}
func (p *Planner) captureGrounding(ctx context.Context, repository string, issue Snapshot) (*GroundedInput, error) {
	if p.grounding == nil {
		return nil, nil
	}
	cfg := p.grounding
	if cfg.Source.Repository != repository {
		return nil, errors.New("configured source repository differs from issue repository")
	}
	capture, allowed := cfg.Source, cfg.AllowedPaths
	if len(cfg.AllowedPrefixes) > 0 {
		selection, err := p.selectFiles(ctx, cfg, issue)
		if err != nil {
			return nil, err
		}
		capture.Paths = contextPaths(cfg.Source.Paths, selection.ContextFiles)
		allowed = selection.AllowedPaths
	}
	snapshot, err := repoworkspace.Capture(ctx, capture)
	if err != nil {
		return nil, err
	}
	raw, err := snapshot.Canonical()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	object, found, err := p.evidence.FindByContentHash(ctx, hex.EncodeToString(sum[:]), KindSourceContext)
	if err != nil {
		return nil, err
	}
	if !found {
		if object, err = p.evidence.Put(ctx, bytes.NewReader(raw), evidence.Metadata{MediaType: "application/json", Kind: KindSourceContext}); err != nil {
			return nil, err
		}
	}
	source := PlanSource{BaseSHA: snapshot.Commit, ContextEvidenceID: object.ID, ContextHash: object.ContentHash, AllowedPaths: append([]string(nil), allowed...), ValidationCommands: cloneCommands(cfg.ValidationCommands)}
	return &GroundedInput{Context: snapshot, Contract: source}, nil
}

// selectFiles asks the model for this issue's files under the owner prefixes and
// checks every path deterministically. A failed call counts as a model failure,
// an unusable selection as a validation failure.
func (p *Planner) selectFiles(ctx context.Context, cfg *GroundingConfig, issue Snapshot) (FileSelection, error) {
	selector, ok := p.model.(FileSelector)
	if !ok {
		return FileSelection{}, errors.New("allowed prefixes need a model that selects files")
	}
	tracked, err := repoworkspace.TrackedFiles(ctx, cfg.Source.LocalPath, cfg.Source.Commit, cfg.AllowedPrefixes)
	if err != nil {
		return FileSelection{}, err
	}
	regular := []string{}
	for path, isRegular := range tracked {
		if isRegular {
			regular = append(regular, path)
		}
	}
	sort.Strings(regular)
	selection, err := selector.SelectFiles(ctx, FileSelectionInput{
		Schema: FileSelectionSchema, Question: FileSelectionQuestion, Snapshot: issue,
		AllowedPrefixes: append([]string(nil), cfg.AllowedPrefixes...), TrackedFiles: regular,
	})
	if err != nil {
		return FileSelection{}, &planFailure{"model-exit", fmt.Errorf("file selection model: %w", err)}
	}
	if err := checkSelection(selection, cfg, tracked); err != nil {
		return FileSelection{}, &planFailure{"validation", fmt.Errorf("file selection: %w", err)}
	}
	return selection, nil
}

func checkSelection(s FileSelection, cfg *GroundingConfig, tracked map[string]bool) error {
	if err := s.Validate(); err != nil {
		return err
	}
	under := func(p string) bool {
		for _, part := range strings.Split(p, "/") {
			if strings.EqualFold(part, ".git") {
				return false // never let a model-chosen path reach Git metadata
			}
		}
		for _, prefix := range cfg.AllowedPrefixes {
			if strings.HasPrefix(p, prefix) {
				return true
			}
		}
		return false
	}
	for _, p := range s.ContextFiles {
		if !under(p) || !tracked[p] {
			return fmt.Errorf("context file %q is not a tracked regular file under an allowed prefix", p)
		}
	}
	if n := len(contextPaths(cfg.Source.Paths, s.ContextFiles)); n < 1 || n > repoworkspace.MaxFiles {
		return errors.New("context selection must hold 1 to 32 files including pinned files")
	}
	for _, p := range s.AllowedPaths {
		if !under(p) {
			return fmt.Errorf("allowed path %q is outside the allowed prefixes", p)
		}
		if regular, exists := tracked[p]; exists {
			if !regular {
				return fmt.Errorf("allowed path %q is not a regular file", p)
			}
			continue
		}
		// A new file must not collide with a tracked directory or sit beneath a tracked file.
		for existing := range tracked {
			if strings.HasPrefix(existing, p+"/") || strings.HasPrefix(p, existing+"/") {
				return fmt.Errorf("allowed path %q collides with tracked path %q", p, existing)
			}
		}
	}
	return nil
}

// contextPaths is the owner-pinned files followed by the selected ones not already pinned.
func contextPaths(pinned, selected []string) []string {
	out := append([]string(nil), pinned...)
	for _, p := range selected {
		if !slices.Contains(pinned, p) {
			out = append(out, p)
		}
	}
	return out
}
