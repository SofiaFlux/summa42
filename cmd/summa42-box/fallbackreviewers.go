package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
)

// fallbackEntry is one replacement reviewer the Owner approved in advance.
// Binary is required for opencode only; commandcode entries use the reviewer's cmd.
type fallbackEntry struct {
	Harness      string `json:"harness"`
	Model        string `json:"model"`
	Binary       string `json:"binary,omitempty"`
	AcceptsNoZDR bool   `json:"accepts_no_zdr"`
}

type fallbackList struct {
	Reviewers []fallbackEntry `json:"reviewers"`
}

// defaultFallbackReviewers is the list's place in the Owner's home.
func defaultFallbackReviewers() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".summa42", "fallback-reviewers.json")
}

// loadFallbackReviewers reads the Owner's list of replacement reviewers. A
// missing file means no replacement. Every entry must pass the reviewer's own
// rules and must not be the producer's model.
func loadFallbackReviewers(path string, reviewer, producer climodel.Config) ([]climodel.Config, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := climodel.CheckTrustedFile(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var list fallbackList
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("fallback reviewers %s: %w", filepath.Base(path), err)
	}
	var out []climodel.Config
	for _, e := range list.Reviewers {
		cfg, err := fallbackConfig(e, reviewer)
		if err != nil {
			return nil, fmt.Errorf("fallback reviewer %s/%s: %w", e.Harness, e.Model, err)
		}
		if sameModel(cfg, producer) {
			return nil, fmt.Errorf("fallback reviewer %s/%s is the producer's model", e.Harness, e.Model)
		}
		out = append(out, cfg)
	}
	return out, nil
}

// fallbackConfig builds the model config for one entry, keeping the reviewer's
// user, timeout and environment. Claude is not listed as a harness: it stays
// out of review until the Owner decides otherwise.
func fallbackConfig(e fallbackEntry, reviewer climodel.Config) (climodel.Config, error) {
	if !e.AcceptsNoZDR {
		return climodel.Config{}, errors.New("set accepts_no_zdr to true: free models do not promise zero data retention")
	}
	cfg := reviewer
	cfg.Args, cfg.CommandCodeModel, cfg.Profile, cfg.ProfileModel, cfg.AllowPaid, cfg.ZDR = nil, "", "", "", false, false
	switch e.Harness {
	case "commandcode":
		if reviewer.CommandCodeModel == "" {
			return climodel.Config{}, errors.New("a commandcode fallback needs a Command Code reviewer")
		}
		if !climodel.IsFreeCommandCodeModel(e.Model) {
			return climodel.Config{}, errors.New("not a free Command Code model")
		}
		cfg.CommandCodeModel = e.Model
	case "opencode":
		if !filepath.IsAbs(e.Binary) {
			return climodel.Config{}, errors.New("opencode needs an absolute binary path")
		}
		cfg.Binary, cfg.Profile, cfg.ProfileModel = e.Binary, "opencode", e.Model
	default:
		return climodel.Config{}, fmt.Errorf("harness %q is not an allowed fallback", e.Harness)
	}
	return cfg, nil
}

// sameModel reports whether two configs name the same model. The pipeline uses
// it to keep the reviewer distinct from the producer.
func sameModel(a, b climodel.Config) bool {
	return a.Binary == b.Binary && slices.Equal(a.Args, b.Args) && a.CommandCodeModel == b.CommandCodeModel &&
		a.Profile == b.Profile && a.ProfileModel == b.ProfileModel && a.ProfileDir == b.ProfileDir
}

// reviewerIdentity names the reviewer a config runs, for the verdict record.
func reviewerIdentity(cfg climodel.Config) ghtriage.ReviewerIdentity {
	harness, model := climodel.Identify(cfg)
	return ghtriage.ReviewerIdentity{Harness: harness, Model: model, FallbackOf: cfg.FallbackOf}
}

// spliceReviewer replaces the primary reviewer's flags in a stage's argv with alt.
// It fails when those flags are not there, so a stage never runs on a silent default.
func spliceReviewer(args, primary, alt []string) ([]string, error) {
	for i := 0; i+len(primary) <= len(args); i++ {
		if slices.Equal(args[i:i+len(primary)], primary) {
			return slices.Concat(args[:i], alt, args[i+len(primary):]), nil
		}
	}
	return nil, errors.New("reviewer flags are missing from the stage argv")
}
