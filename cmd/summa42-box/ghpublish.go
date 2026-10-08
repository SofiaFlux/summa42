package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/feedbackgithub"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"os"
	"path/filepath"
	"strings"
)

func runGHPublish(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context required")
	}
	var id, source, base, repo, credential string
	flags := flag.NewFlagSet("run-gh-publish", flag.ContinueOnError)
	flags.StringVar(&id, "case", "", "active independently accepted implementation Case")
	flags.StringVar(&source, "source-repo", "", "absolute source repository")
	flags.StringVar(&base, "base-branch", "", "explicit pinned base branch")
	flags.StringVar(&repo, "repository", "", "explicit owner/repository")
	flags.StringVar(&credential, "credential-file", "", "absolute private GitHub token file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || !filepath.IsAbs(source) || !filepath.IsAbs(credential) || base == "" || repo == "" || flags.NArg() != 0 {
		return errors.New("run-gh-publish requires --case, absolute --source-repo, --base-branch, --repository and absolute --credential-file")
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() {
		return errors.New("source must be an existing regular directory")
	}
	credentials := feedbackgithub.FileCredentialSource{Path: credential}
	if _, err := credentials.Token(ctx); err != nil {
		return err
	}
	cfgProvider := ghpublish.Config{Repository: repo, CredentialSource: credentials}
	branch, err := ghpublish.NewBranchProvider(cfgProvider)
	if err != nil {
		return err
	}
	pr, err := ghpublish.NewPRProvider(cfgProvider)
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
	// Explicit provider composition; no model, executor, feedback or worker is run.
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{StatePath: cfg.DatabasePath, EvidencePath: cfg.EvidencePath, CollectiveID: cfg.CollectiveID, OwnerPrincipalID: cfg.OwnerPrincipalID, PolicyEngine: material.policyEngine, OperationProviders: []operations.Provider{branch, pr}})
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
		return errors.New("explicit publication repository differs from Case")
	}
	got, err := ghtriage.NewPublisher(cases, box.Execution, box.Verification, box.Evidence, box.Operations).Publish(ctx, c.ID, source, base)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(os.Stdout).Encode(got); err != nil {
		return err
	}
	if got.Held {
		return errors.New("publication held; approve the exact bound operation within its live lease, then retry")
	}
	if got.Pending {
		return errors.New("publication outcome unknown; reconcile without repeating writes")
	}
	return nil
}
