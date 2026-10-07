package main

import (
	"encoding/json"
	"errors"
	"flag"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
)

type groundingFlags struct {
	flags                      *flag.FlagSet
	local, repository, sha     string
	context, allowed, commands stringSlice
}

func registerGroundingFlags(flags *flag.FlagSet) *groundingFlags {
	g := &groundingFlags{flags: flags}
	flags.StringVar(&g.local, "source-repo", "", "absolute local Git source repository")
	flags.StringVar(&g.repository, "source-repository", "", "GitHub owner/repo identity of local source")
	flags.StringVar(&g.sha, "source-sha", "", "full pinned source commit SHA")
	flags.Var(&g.context, "context-file", "exact tracked source file to project (repeatable)")
	flags.Var(&g.allowed, "allowed-path", "exact file path changes may affect (repeatable)")
	flags.Var(&g.commands, "validation-command-json", "validation argv as JSON array (repeatable, inert during planning)")
	return g
}
func (g *groundingFlags) config() (*ghtriage.GroundingConfig, error) {
	provided := false
	g.flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "source-repo", "source-repository", "source-sha", "context-file", "allowed-path", "validation-command-json":
			provided = true
		}
	})
	if !provided && g.local == "" && g.repository == "" && g.sha == "" && len(g.context) == 0 && len(g.allowed) == 0 && len(g.commands) == 0 {
		return nil, nil
	}
	cfg := ghtriage.GroundingConfig{Source: repoworkspace.Config{LocalPath: g.local, Repository: g.repository, Commit: g.sha, Paths: []string(g.context)}, AllowedPaths: []string(g.allowed)}
	for _, raw := range g.commands {
		var argv []string
		if err := json.Unmarshal([]byte(raw), &argv); err != nil {
			return nil, errors.New("validation-command-json must be a JSON argv array")
		}
		cfg.ValidationCommands = append(cfg.ValidationCommands, argv)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
