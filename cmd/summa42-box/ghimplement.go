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
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// parseGHImplementFlags is run-gh-implement's flag parsing, without filesystem checks.
func parseGHImplementFlags(args []string) (caseID, source, root string, modelConfig climodel.Config, err error) {
	flags := flag.NewFlagSet("run-gh-implement", flag.ContinueOnError)
	flags.StringVar(&caseID, "case", "", "active implementation case ID")
	flags.StringVar(&source, "source-repo", "", "absolute local Git repository matching accepted plan")
	flags.StringVar(&root, "workspace-root", "", "existing absolute directory outside source for isolated attempts")
	registerTriageModelFlags(flags, "model binary returning strict complete-file edits for the accepted plan")
	if err = flags.Parse(args); err != nil {
		return
	}
	if strings.TrimSpace(caseID) == "" || !filepath.IsAbs(source) || !filepath.IsAbs(root) || flags.NArg() != 0 {
		err = errors.New("run-gh-implement requires --case, absolute --source-repo and --workspace-root, and no positional arguments")
		return
	}
	modelConfig, err = triageModelConfig(flags)
	return
}

// One supervised local candidate; completion awaits independent code verification.
func runGHImplement(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	caseID, source, root, modelConfig, err := parseGHImplementFlags(args)
	if err != nil {
		return err
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
	validator, err := validatorFromEnv(modelConfig.RunAsUser)
	if err != nil {
		return err
	}
	impl := ghtriage.NewImplementer(workflowcase.New(box.Store, box.Clock, box.Purpose), box.Execution, box.Verification, box.Evidence, adapter).
		WithModelTimeout(adapter.Timeout()).WithValidator(validator)
	result, err := impl.Prepare(ctx, domain.ID(strings.TrimSpace(caseID)), source, root)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	return implementOutcome(result)
}

// validationToolchainEnv names the colon-separated, absolute directories put in
// front of the model user's fixed PATH when validation commands run (for
// example the Go toolchain's bin directory). The Box's own PATH is never used.
const validationToolchainEnv = "SUMMA42_VALIDATION_TOOLCHAIN_DIRS"

// validatorFromEnv builds the validator that runs the validation commands as
// the model user. It fails closed when no model user is configured.
func validatorFromEnv(runAsUser string) (*repoworkspace.Validator, error) {
	return repoworkspace.NewValidator(runAsUser, splitPathList(os.Getenv(validationToolchainEnv)))
}

func splitPathList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ":") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// implementOutcome is the command's error for a Prepare result: busy when
// another claimant holds the implementation task.
func implementOutcome(result ghtriage.ImplementationResult) error {
	if result.Busy {
		return errBusy
	}
	if result.Record != nil && !result.Record.AllPassed {
		return fmt.Errorf("candidate %s recorded failing validation; independent acceptance remains pending", result.Record.CandidateSHA)
	}
	return nil
}
