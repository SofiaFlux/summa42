package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"os"
	"strings"
)

// parseGHPlanReviewFlags is run-gh-plan-review's flag parsing.
func parseGHPlanReviewFlags(args []string) (mission, baseSHA string, modelConfig climodel.Config, err error) {
	flags := flag.NewFlagSet("run-gh-plan-review", flag.ContinueOnError)
	fallbackOf := flags.String("model-fallback-of", "", "pool a replacement reviewer stands in for (set by run-gh-pipeline)")
	flags.StringVar(&mission, "mission", "", "mission whose grounded plans are independently reviewed")
	flags.StringVar(&baseSHA, "source-sha", "", "commit plans must be grounded on; a plan on another base is held unreviewed (optional)")
	registerTriageModelFlags(flags, "model binary reviewing a grounded issue plan")
	if err = flags.Parse(args); err != nil {
		return
	}
	if strings.TrimSpace(mission) == "" || flags.NArg() != 0 {
		err = errors.New("run-gh-plan-review requires --mission and no positional arguments")
		return
	}
	modelConfig, err = triageModelConfig(flags)
	modelConfig.FallbackOf = *fallbackOf
	return
}

// One supervised tick; callers may configure a different model from the planner.
func runGHPlanReview(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	mission, baseSHA, modelConfig, err := parseGHPlanReviewFlags(args)
	if err != nil {
		return err
	}
	adapter, usable := triageModelAdapter(modelConfig, os.Stderr)
	if !usable {
		return errors.New("run-gh-plan-review requires a usable model")
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
	adapter = adapter.WithLedger(box.Store, "plan-review", "")
	reviewer := ghtriage.NewPlanReviewer(workflowcase.New(box.Store, box.Clock, box.Purpose), box.Execution, box.Verification, box.Evidence, adapter).WithModelTimeout(adapter.Timeout()).WithBaseSHA(strings.TrimSpace(baseSHA)).WithReviewer(reviewerIdentity(modelConfig))
	result, err := reviewer.Tick(ctx, domain.ID(strings.TrimSpace(mission)))
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if len(result.Failures) > 0 {
		return fmt.Errorf("plan review failed for %d case(s): %s", len(result.Failures), strings.Join(result.Failures, "; "))
	}
	if result.Busy > 0 {
		return errBusy
	}
	return nil
}
