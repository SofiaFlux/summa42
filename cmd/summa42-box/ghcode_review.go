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
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

func runGHCodeReview(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context required")
	}
	var caseID, source string
	flags := flag.NewFlagSet("run-gh-code-review", flag.ContinueOnError)
	flags.StringVar(&caseID, "case", "", "active implementation case ID")
	flags.StringVar(&source, "source-repo", "", "absolute source repository used for the implementation")
	registerTriageModelFlags(flags, "independent reviewer wrapper returning ACCEPT, REVISE or BLOCK")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(caseID) == "" || !filepath.IsAbs(source) || flags.NArg() != 0 {
		return errors.New("run-gh-code-review requires --case and absolute --source-repo with no positional arguments")
	}
	if info, err := os.Lstat(source); err != nil || !info.IsDir() {
		return errors.New("source must be an existing regular directory")
	}
	modelConfig, err := triageModelConfig(flags)
	if err != nil {
		return err
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
	reviewer := ghtriage.NewCodeReviewer(workflowcase.New(box.Store, box.Clock, box.Purpose), box.Execution, box.Verification, box.Evidence, adapter)
	result, err := reviewer.Review(ctx, domain.ID(strings.TrimSpace(caseID)), source)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if result.Record != nil && !result.Accepted {
		return fmt.Errorf("implementation held: code review %s or recorded validation failed", result.Record.Verdict)
	}
	return nil
}
