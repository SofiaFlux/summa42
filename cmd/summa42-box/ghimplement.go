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

// One supervised local candidate; completion awaits independent code verification.
func runGHImplement(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	var caseID, source, root string
	flags := flag.NewFlagSet("run-gh-implement", flag.ContinueOnError)
	flags.StringVar(&caseID, "case", "", "active implementation case ID")
	flags.StringVar(&source, "source-repo", "", "absolute local Git repository matching accepted plan")
	flags.StringVar(&root, "workspace-root", "", "existing absolute directory outside source for isolated attempts")
	registerTriageModelFlags(flags, "model binary returning strict complete-file edits for the accepted plan")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(caseID) == "" || !filepath.IsAbs(source) || !filepath.IsAbs(root) || flags.NArg() != 0 {
		return errors.New("run-gh-implement requires --case, absolute --source-repo and --workspace-root, and no positional arguments")
	}
	for _, path := range []string{source, root} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return errors.New("source and workspace root must be existing regular directories")
		}
	}
	rel, err := filepath.Rel(source, root)
	if err != nil || rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		return errors.New("workspace root must be outside source")
	}
	modelConfig, err := triageModelConfig(flags)
	if err != nil {
		return err
	}
	adapter, usable := triageModelAdapter(modelConfig, os.Stderr)
	if !usable {
		return errors.New("run-gh-implement requires a usable model")
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
	impl := ghtriage.NewImplementer(workflowcase.New(box.Store, box.Clock, box.Purpose), box.Execution, box.Verification, box.Evidence, adapter)
	result, err := impl.Prepare(ctx, domain.ID(strings.TrimSpace(caseID)), source, root)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	if result.Record != nil && !result.Record.AllPassed {
		return fmt.Errorf("candidate %s recorded failing validation; independent acceptance remains pending", result.Record.CandidateSHA)
	}
	return nil
}
