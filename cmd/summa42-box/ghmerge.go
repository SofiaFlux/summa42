package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/feedbackgithub"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func runGHMerge(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context required")
	}
	var id, source, base, repo, credential, method, requiredChecksStr string
	flags := flag.NewFlagSet("run-gh-merge", flag.ContinueOnError)
	flags.StringVar(&id, "case", "", "active published implementation Case")
	flags.StringVar(&source, "source-repo", "", "absolute source repository")
	flags.StringVar(&base, "base-branch", "main", "explicit pinned base branch")
	flags.StringVar(&repo, "repository", "", "explicit owner/repository")
	flags.StringVar(&credential, "credential-file", "", "absolute private GitHub token file")
	flags.StringVar(&method, "merge-method", "merge", "merge method (merge, squash, rebase)")
	flags.StringVar(&requiredChecksStr, "required-checks", "", "comma-separated required CI check run names")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || !filepath.IsAbs(source) || !filepath.IsAbs(credential) || base == "" || repo == "" || flags.NArg() != 0 {
		return errors.New("run-gh-merge requires --case, absolute --source-repo, --base-branch, --repository and absolute --credential-file")
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() {
		return errors.New("source must be an existing regular directory")
	}
	credentials := feedbackgithub.FileCredentialSource{Path: credential}
	if _, err := credentials.Token(ctx); err != nil {
		return err
	}
	cfgProvider := ghpublish.Config{Repository: repo, CredentialSource: credentials}
	mergeProvider, err := ghpublish.NewMergeProvider(cfgProvider)
	if err != nil {
		return err
	}
	observer, err := ghpublish.NewObserver(cfgProvider)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return err
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:          cfg.DatabasePath,
		EvidencePath:       cfg.EvidencePath,
		CollectiveID:       cfg.CollectiveID,
		OwnerPrincipalID:   cfg.OwnerPrincipalID,
		PolicyEngine:       material.policyEngine,
		OperationProviders: []operations.Provider{mergeProvider},
	})
	if err != nil {
		return err
	}
	defer box.Close()
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	c, err := cases.Get(ctx, domain.ID(strings.TrimSpace(id)))
	if err != nil {
		return err
	}
	caseRepo, _, err := ghissue.ParseObjectID(c.ObjectID)
	if err != nil || caseRepo != repo {
		return errors.New("explicit merge repository differs from Case")
	}
	var requiredChecks []string
	if strings.TrimSpace(requiredChecksStr) != "" {
		for _, part := range strings.Split(requiredChecksStr, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				requiredChecks = append(requiredChecks, trimmed)
			}
		}
	}
	got, err := ghtriage.NewMerger(cases, box.Execution, box.Verification, box.Evidence, box.Operations, observer).Merge(ctx, c.ID, source, base, method, requiredChecks)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(got); err != nil {
		return err
	}
	if got.Held {
		return errors.New("merge held; approve the exact bound operation within its live lease, then retry")
	}
	if got.Pending {
		return errors.New("merge outcome pending or CI in progress; retry once checks complete")
	}
	return nil
}
