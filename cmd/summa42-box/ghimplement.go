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
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// parseGHImplementFlags is run-gh-implement's flag parsing, without filesystem checks.
func parseGHImplementFlags(args []string) (caseID, source, root string, modelConfig climodel.Config, validation validationConfig, err error) {
	flags := flag.NewFlagSet("run-gh-implement", flag.ContinueOnError)
	flags.StringVar(&caseID, "case", "", "active implementation case ID")
	flags.StringVar(&source, "source-repo", "", "absolute local Git repository matching accepted plan")
	flags.StringVar(&root, "workspace-root", "", "existing absolute directory outside source for isolated attempts")
	registerTriageModelFlags(flags, "model binary returning strict complete-file edits for the accepted plan")
	registerValidationFlags(flags)
	if err = flags.Parse(args); err != nil {
		return
	}
	if strings.TrimSpace(caseID) == "" || !filepath.IsAbs(source) || !filepath.IsAbs(root) || flags.NArg() != 0 {
		err = errors.New("run-gh-implement requires --case, absolute --source-repo and --workspace-root, and no positional arguments")
		return
	}
	if modelConfig, err = triageModelConfig(flags); err != nil {
		return
	}
	validation, err = validationConfigFrom(flags)
	return
}

// One supervised local candidate; completion awaits independent code verification.
func runGHImplement(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	caseID, source, root, modelConfig, validation, err := parseGHImplementFlags(args)
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
	validator, err := validatorFor(validation, modelConfig.RunAsUser)
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
// front of the validation user's fixed PATH when validation commands run (for
// example a Go toolchain's bin directory that the validation user can
// execute). The Box's own PATH is never used.
const validationToolchainEnv = "SUMMA42_VALIDATION_TOOLCHAIN_DIRS"

// validationConfig is the validation user and the per-command time limit.
type validationConfig struct {
	User    string
	Timeout time.Duration
}

// registerValidationFlags puts the validation flags on a flag set. The user
// is required (empty fails closed when the validator is built) and must be a
// dedicated account with no credentials, different from the model user.
func registerValidationFlags(flags *flag.FlagSet) {
	flags.String("validation-user", os.Getenv("SUMMA42_VALIDATION_USER"),
		"dedicated OS user validation commands (candidate code) run as via sudo -n -u (required; env SUMMA42_VALIDATION_USER; "+
			"must have no credentials and differ from the model user, the Box user and root; see scripts/setup-model-user.sh)")
	timeout := os.Getenv("SUMMA42_VALIDATION_TIMEOUT")
	if timeout == "" {
		timeout = repoworkspace.DefaultCommandTimeout.String()
	}
	flags.String("validation-timeout", timeout,
		"per-command validation time limit (env SUMMA42_VALIDATION_TIMEOUT; generous by default)")
}

func validationConfigFrom(flags *flag.FlagSet) (validationConfig, error) {
	cfg := validationConfig{User: strings.TrimSpace(flags.Lookup("validation-user").Value.String())}
	timeout, err := time.ParseDuration(flags.Lookup("validation-timeout").Value.String())
	if err != nil {
		return cfg, fmt.Errorf("--validation-timeout: %w", err)
	}
	if timeout <= 0 {
		return cfg, errors.New("--validation-timeout must be positive")
	}
	cfg.Timeout = timeout
	return cfg, nil
}

// validatorFor builds the validator that runs the validation commands as the
// validation user. It fails closed when no validation user is configured.
func validatorFor(cfg validationConfig, modelUser string) (*repoworkspace.Validator, error) {
	return repoworkspace.NewValidator(cfg.User, modelUser, splitPathList(os.Getenv(validationToolchainEnv)), cfg.Timeout)
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
