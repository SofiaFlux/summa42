package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// parseGHCodeReviewFlags is run-gh-code-review's flag parsing, without filesystem checks.
func parseGHCodeReviewFlags(args []string) (caseID, source string, modelConfig climodel.Config, maxRepairs int, err error) {
	flags := flag.NewFlagSet("run-gh-code-review", flag.ContinueOnError)
	fallbackOf := flags.String("model-fallback-of", "", "pool a replacement reviewer stands in for (set by run-gh-pipeline)")
	flags.StringVar(&caseID, "case", "", "active implementation case ID")
	flags.StringVar(&source, "source-repo", "", "absolute source repository used for the implementation")
	flags.IntVar(&maxRepairs, "max-repairs", 0, "repairs a case may get after a REVISE, counted over plan and code review; 0 holds the case on every REVISE")
	registerTriageModelFlags(flags, "independent reviewer wrapper returning ACCEPT, REVISE or BLOCK")
	if err = flags.Parse(args); err != nil {
		return
	}
	if strings.TrimSpace(caseID) == "" || !filepath.IsAbs(source) || flags.NArg() != 0 {
		err = errors.New("run-gh-code-review requires --case and absolute --source-repo with no positional arguments")
		return
	}
	if maxRepairs < 0 {
		err = errors.New("run-gh-code-review requires a non-negative --max-repairs")
		return
	}
	modelConfig, err = triageModelConfig(flags)
	modelConfig.FallbackOf = *fallbackOf
	return
}

func runGHCodeReview(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context required")
	}
	caseID, source, modelConfig, maxRepairs, err := parseGHCodeReviewFlags(args)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() {
		return errors.New("source must be an existing regular directory")
	}
	adapter, usable := triageModelAdapter(modelConfig, os.Stderr)
	if !usable {
		return errors.New("run-gh-code-review requires a usable independent reviewer")
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
	box, err := openGHTriageDriverBox(ctx, summa42runtime.Config{StatePath: cfg.DatabasePath, EvidencePath: cfg.EvidencePath, CollectiveID: cfg.CollectiveID, OwnerPrincipalID: cfg.OwnerPrincipalID, PolicyEngine: material.policyEngine})
	if err != nil {
		return err
	}
	defer box.Close()
	adapter = adapter.WithLedger(box.Store, "code-review", strings.TrimSpace(caseID))
	reviewer := ghtriage.NewCodeReviewer(workflowcase.New(box.Store, box.Clock, box.Purpose), box.Execution, box.Verification, box.Evidence, adapter).WithModelTimeout(adapter.Timeout()).WithReviewer(reviewerIdentity(modelConfig))
	reviewer.WithMaxRepairs(maxRepairs)
	result, err := reviewer.Review(ctx, domain.ID(strings.TrimSpace(caseID)), source)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if result.Busy {
		return errBusy
	}
	if result.Record != nil && !result.Accepted {
		// With repairs enabled a REVISE may have continued the case into new
		// implementation work; the message covers both outcomes.
		return fmt.Errorf("implementation not accepted: code review %s or recorded validation failed", result.Record.Verdict)
	}
	return nil
}
