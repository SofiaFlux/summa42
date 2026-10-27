package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/capacity"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	statesqlite "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// errBusy means the work a run-gh-* subcommand would advance is held by
// another claimant. It exits 3, so a supervisor can tell it from done (0) and
// failed (1).
var errBusy = errors.New("busy")

// errCapacityGated means a stage was held because a pool it needs is exhausted.
// Like errBusy it is a skip, not a failure.
var errCapacityGated = errors.New("capacity")

// A gate is the pools a stage can spend, and the stage that spends them. A stage
// is held only when every pool in its gate is exhausted. An empty pool (a harness
// with no capacity model) is always available.
type gate struct {
	pools []string
	by    string
}

const busyExitCode = 3

// exitStatus prints err and returns the subcommand's exit code.
func exitStatus(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errBusy):
		fmt.Fprintln(stderr, "busy")
		return busyExitCode
	default:
		fmt.Fprintln(stderr, err)
		return 1
	}
}

// pipelineWorkerLease is the lease run-gh-pipeline gives the triage worker: the
// model timeout plus a margin, so a slow but healthy model call is never cut
// off by its own lease. The worker stage's own timeout sits below it, so the
// executor's deadline is the stage's and the triage retry guard sees the real
// time left.
func pipelineWorkerLease(model time.Duration) time.Duration { return model + 5*time.Minute }

// pipelineBatchTimeout bounds a stage that makes one model call per case in a
// tick: never below 15m, and never below the planner's TickBudget (two model
// calls per case in grounding mode plus a margin), so the stage cannot be cut
// off before the planner has started a case.
func pipelineBatchTimeout(model time.Duration) time.Duration {
	return max(15*time.Minute, ghtriage.PlanStageTimeout(model))
}

// A pipelineStage is one run-gh-* subcommand run in-process with fixed argv.
type pipelineStage struct {
	name    string
	timeout time.Duration
	run     func(context.Context, []string) error
	args    []string
}

// pipeline is one run-gh-pipeline pass: the mission stages, then implement,
// code review and publish for every case at implementation work.
type pipeline struct {
	stages         []pipelineStage
	perCase        []pipelineStage
	implementCases func(context.Context) ([]domain.ID, error)
	stderr         io.Writer
	// gates lists, per stage name, the pools it needs before it may call a model.
	gates map[string][]gate
	// ledger is the usage ledger the gates read; nil turns the gates off.
	ledger capacity.Ledger
	// fallbacks are the replacement reviewers the Owner approved, in order.
	fallbacks []climodel.Config
	// reviewerPool and reviewArgv are the primary reviewer's pool and its flags,
	// which a replacement is spliced in for.
	reviewerPool string
	reviewArgv   []string
	// exhausted holds the pools the last report found exhausted, so a
	// transition is logged once rather than every pass.
	exhausted map[string]bool
}

// parsePipelineFlags returns the pipeline and its interval; zero means --once.
// Each stage keeps its own validation, so only what the pipeline itself
// decides is checked here.
func parsePipelineFlags(args []string) (*pipeline, time.Duration, error) {
	var mission, root, repo, base, credential, fallbackPath string
	var once bool
	var interval time.Duration
	flags := flag.NewFlagSet("run-gh-pipeline", flag.ContinueOnError)
	flags.StringVar(&mission, "mission", "", "mission whose GitHub cases are advanced")
	flags.StringVar(&root, "workspace-root", "", "absolute directory outside --source-repo for worker and implementation attempts")
	flags.StringVar(&repo, "repository", "", "GitHub owner/name publications go to")
	flags.StringVar(&base, "base-branch", "", "base branch publications target")
	flags.StringVar(&credential, "credential-file", "", "absolute private GitHub token file for publication")
	flags.BoolVar(&once, "once", false, "run one pass and exit")
	flags.DurationVar(&interval, "interval", 0, "run a pass every interval until stopped")
	flags.StringVar(&fallbackPath, "fallback-reviewers", defaultFallbackReviewers(), "Owner's list of replacement reviewers; a missing file means none")
	grounding := registerGroundingFlags(flags)
	registerTriageModelFlags(flags, "producer model binary (triage worker, planner, implementer)")
	registerModelFlags(flags, "review-model", "reviewer model binary (triage review, plan review, code review)")
	registerValidationFlags(flags)
	if err := flags.Parse(args); err != nil {
		return nil, 0, err
	}
	mission = strings.TrimSpace(mission)
	if mission == "" || flags.NArg() != 0 {
		return nil, 0, errors.New("run-gh-pipeline requires --mission and no positional arguments")
	}
	if !filepath.IsAbs(grounding.local) || !filepath.IsAbs(root) || !filepath.IsAbs(credential) ||
		strings.TrimSpace(base) == "" || strings.TrimSpace(repo) == "" {
		return nil, 0, errors.New("run-gh-pipeline requires absolute --source-repo, --workspace-root and --credential-file, and --base-branch and --repository")
	}
	if once == (interval != 0) || interval < 0 {
		return nil, 0, errors.New("run-gh-pipeline requires exactly one of --once or a positive --interval")
	}
	producer, err := triageModelConfig(flags)
	if err != nil {
		return nil, 0, err
	}
	validation, err := validationConfigFrom(flags)
	if err != nil {
		return nil, 0, err
	}
	reviewer, err := prefixedModelConfig(flags, "review-model")
	if err != nil {
		return nil, 0, err
	}
	if sameModel(producer, reviewer) {
		return nil, 0, errors.New("review model must differ from producer model")
	}
	fallbacks, err := loadFallbackReviewers(fallbackPath, reviewer, producer)
	if err != nil {
		return nil, 0, err
	}
	producerPool, err := climodel.PoolOf(producer)
	if err != nil {
		return nil, 0, err
	}
	reviewerPool, err := climodel.PoolOf(reviewer)
	if err != nil {
		return nil, 0, err
	}
	// A replacement reviewer stands in for the primary reviewer's pool.
	reviewerPools := []string{reviewerPool}
	for i := range fallbacks {
		fallbacks[i].FallbackOf = reviewerPool
		pool, err := climodel.PoolOf(fallbacks[i])
		if err != nil {
			return nil, 0, err
		}
		reviewerPools = append(reviewerPools, pool)
	}
	source := grounding.local
	missionArgs := []string{"--mission=" + mission}
	produce, review := modelFlagArgv(producer), modelFlagArgv(reviewer)
	// ponytail: fixed stage timeouts; the per-case ones sit a minute under
	// their stage's attempt lease. Make them flags if a deployment needs to.
	p := &pipeline{
		stderr:       os.Stderr,
		fallbacks:    fallbacks,
		reviewerPool: reviewerPool,
		reviewArgv:   review,
		exhausted:    map[string]bool{},
		// A producing stage also needs a reviewer for the output it hands on. Triage
		// review takes no replacement, so its gate is the primary pool alone.
		gates: map[string][]gate{
			"worker":        {{[]string{producerPool}, "worker"}, {[]string{reviewerPool}, "triage-review"}},
			"triage-review": {{[]string{reviewerPool}, "triage-review"}},
			"plan":          {{[]string{producerPool}, "plan"}, {reviewerPools, "plan-review"}},
			"plan-review":   {{reviewerPools, "plan-review"}},
			"implement":     {{[]string{producerPool}, "implement"}, {reviewerPools, "code-review"}},
			"code-review":   {{reviewerPools, "code-review"}},
		},
		stages: []pipelineStage{
			{"recover", time.Minute, recoverExpiredLeases, nil},
			{"triage-driver", 5 * time.Minute, runGHTriageDriver, missionArgs},
			{"worker", pipelineWorkerLease(producer.Timeout) - time.Minute, runWorker, slices.Concat(
				[]string{"--once", "--workspace-root=" + root, "--lease-duration=" + pipelineWorkerLease(producer.Timeout).String(), "--credential-file=" + credential}, produce)},
			{"triage-review", pipelineBatchTimeout(reviewer.Timeout), runGHTriageReview, slices.Concat(missionArgs, review)},
			{"plan", pipelineBatchTimeout(producer.Timeout), runGHPlan, slices.Concat(missionArgs, grounding.argv(), produce)},
			{"plan-review", pipelineBatchTimeout(reviewer.Timeout), runGHPlanReview, slices.Concat(missionArgs, planBase(grounding), review)},
		},
		perCase: []pipelineStage{
			{"implement", ghtriage.ImplementationLease(producer.Timeout, ghtriage.MaxValidationCommands, validation.Timeout) - time.Minute, runGHImplement, slices.Concat(
				[]string{"--source-repo=" + source, "--workspace-root=" + root, "--validation-user=" + validation.User, "--validation-timeout=" + validation.Timeout.String()}, produce)},
			{"code-review", ghtriage.CodeReviewLease(reviewer.Timeout) - time.Minute, runGHCodeReview, slices.Concat(
				[]string{"--source-repo=" + source}, review)},
			{"publish", ghtriage.PublicationLeaseDuration - time.Minute, runGHPublish, []string{
				"--source-repo=" + source, "--base-branch=" + base, "--repository=" + repo, "--credential-file=" + credential}},
		},
	}
	p.implementCases = func(ctx context.Context) ([]domain.ID, error) {
		box, err := openPipelineBox(ctx)
		if err != nil {
			return nil, err
		}
		defer box.Close()
		return implementationCases(ctx, workflowcase.New(box.Store, box.Clock, box.Purpose), domain.ID(mission))
	}
	return p, interval, nil
}

// modelFlagArgv renders cfg as the --model-* flags every run-gh-* stage takes.
func modelFlagArgv(cfg climodel.Config) []string {
	out := []string{"--model-binary=" + cfg.Binary, "--model-timeout=" + cfg.Timeout.String(), "--model-user=" + cfg.RunAsUser}
	for _, arg := range cfg.Args {
		out = append(out, "--model-arg="+arg)
	}
	for _, name := range cfg.EnvAllowlist {
		out = append(out, "--model-env="+name)
	}
	if cfg.CommandCodeModel != "" {
		out = append(out, "--model-commandcode="+cfg.CommandCodeModel)
	}
	if cfg.Profile != "" {
		out = append(out, "--model-profile="+cfg.Profile, "--model-name="+cfg.ProfileModel, "--model-profile-dir="+cfg.ProfileDir)
	}
	if cfg.AllowPaid {
		out = append(out, "--model-allow-paid")
	}
	if cfg.FallbackOf != "" {
		out = append(out, "--model-fallback-of="+cfg.FallbackOf)
	}
	if cfg.ZDR {
		out = append(out, "--model-zdr")
	}
	return out
}

// pass runs every stage once. A stage error is one stderr line and does not
// stop later stages; busy and a case that already moved on are skips. The pass
// fails if any stage failed.
func (p *pipeline) pass(ctx context.Context) error {
	failed := 0
	p.reportCapacity(ctx)
	report := func(name, caseID string, err error) {
		switch {
		case err == nil:
		case errors.Is(err, errBusy) || errors.Is(err, ghtriage.ErrCaseNotAtStage) || errors.Is(err, errCapacityGated):
			fmt.Fprintf(p.stderr, "stage=%s case=%s skipped=%s\n", name, caseID, oneLine(err))
		default:
			failed++
			fmt.Fprintf(p.stderr, "stage=%s case=%s err=%s\n", name, caseID, oneLine(err))
		}
	}
	run := func(s pipelineStage, caseID string, args []string) {
		if err := p.capacityGate(ctx, s.name); err != nil {
			report(s.name, caseID, err)
			return
		}
		routed, err := p.routeReviewer(ctx, s.name, args)
		if err != nil {
			report(s.name, caseID, err)
			return
		}
		stageCtx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		report(s.name, caseID, s.run(stageCtx, routed))
	}
	for _, s := range p.stages {
		run(s, "-", s.args)
	}
	selectCtx, cancel := context.WithTimeout(ctx, time.Minute)
	ids, err := p.implementCases(selectCtx)
	cancel()
	report("select", "-", err)
	for _, id := range ids {
		for _, s := range p.perCase {
			run(s, string(id), slices.Concat([]string{"--case", string(id)}, s.args))
		}
	}
	if failed > 0 {
		return fmt.Errorf("pipeline pass: %d stage(s) failed", failed)
	}
	return nil
}

func oneLine(err error) string { return strings.ReplaceAll(err.Error(), "\n", " ") }

// capacityGate holds a stage while every pool it can spend is exhausted. A held
// stage makes no model call, and its hold is recorded so the ledger shows it.
func (p *pipeline) capacityGate(ctx context.Context, stage string) error {
	if p.ledger == nil {
		return nil
	}
	now := time.Now()
	for _, g := range p.gates[stage] {
		if len(g.pools) == 0 {
			continue
		}
		available := false
		var blocked string
		var until time.Time
		for _, pool := range g.pools {
			if pool == "" {
				available = true
				break
			}
			state, err := p.ledger.PoolState(ctx, pool, now)
			if err != nil {
				return fmt.Errorf("capacity ledger: %w", err)
			}
			if !state.Exhausted {
				available = true
				break
			}
			if blocked == "" {
				blocked, until = pool, state.ResetAt
			}
		}
		if available {
			continue
		}
		if err := p.ledger.RecordCall(ctx, capacity.Call{Pool: blocked, Stage: stage, StartedAt: now, Outcome: capacity.OutcomeGated}); err != nil {
			fmt.Fprintf(p.stderr, "capacity ledger: %v\n", err)
		}
		return fmt.Errorf("%w: pool %s exhausted until %s (needed by %s)", errCapacityGated, blocked, until.UTC().Format(time.RFC3339), g.by)
	}
	return nil
}

// routeReviewer chooses the reviewer a review stage calls: the primary while its
// pool has room, otherwise the first approved replacement with room. Only plan
// review and code review take a replacement; a replacement's verdict is recorded
// as one, so it cannot open implementation.
func (p *pipeline) routeReviewer(ctx context.Context, stage string, args []string) ([]string, error) {
	if stage != "plan-review" && stage != "code-review" || p.ledger == nil || p.reviewerPool == "" || len(p.fallbacks) == 0 {
		return args, nil
	}
	state, err := p.ledger.PoolState(ctx, p.reviewerPool, time.Now())
	if err != nil {
		return nil, fmt.Errorf("capacity ledger: %w", err)
	}
	if !state.Exhausted {
		return args, nil
	}
	for _, fb := range p.fallbacks {
		pool, err := climodel.PoolOf(fb)
		if err != nil {
			return nil, err
		}
		if pool != "" {
			st, err := p.ledger.PoolState(ctx, pool, time.Now())
			if err != nil {
				return nil, fmt.Errorf("capacity ledger: %w", err)
			}
			if st.Exhausted {
				continue
			}
		}
		return spliceReviewer(args, p.reviewArgv, modelFlagArgv(fb))
	}
	return args, nil
}

// reportCapacity logs each pool once when it becomes exhausted and once when it
// is available again. It is the pass's capacity stage: it reads, it spends nothing.
func (p *pipeline) reportCapacity(ctx context.Context) {
	if p.ledger == nil {
		return
	}
	pools := map[string]bool{}
	for _, gates := range p.gates {
		for _, g := range gates {
			for _, pool := range g.pools {
				if pool != "" {
					pools[pool] = true
				}
			}
		}
	}
	now := time.Now()
	for _, pool := range slices.Sorted(maps.Keys(pools)) {
		state, err := p.ledger.PoolState(ctx, pool, now)
		if err != nil {
			fmt.Fprintf(p.stderr, "capacity: pool=%s err=%v\n", pool, err)
			continue
		}
		switch {
		case state.Exhausted && !p.exhausted[pool]:
			p.exhausted[pool] = true
			fmt.Fprintf(p.stderr, "capacity: pool=%s exhausted until=%s\n", pool, state.ResetAt.UTC().Format(time.RFC3339))
		case !state.Exhausted && p.exhausted[pool]:
			delete(p.exhausted, pool)
			fmt.Fprintf(p.stderr, "capacity: pool=%s available\n", pool)
		}
	}
}

// openCapacityLedger opens the usage ledger in the Collective's state database.
func openCapacityLedger(ctx context.Context) (*statesqlite.Store, error) {
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return nil, err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return nil, fmt.Errorf("load initialized Collective: %w", err)
	}
	return statesqlite.Open(ctx, cfg.DatabasePath)
}

// runGHPipeline runs one pass (--once) or a pass every --interval. In loop
// mode a failed pass is logged and the next one still runs; cancellation is a
// clean stop.
func runGHPipeline(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	p, interval, err := parsePipelineFlags(args)
	if err != nil {
		return err
	}
	ledger, err := openCapacityLedger(ctx)
	if err != nil {
		return err
	}
	defer ledger.DB().Close()
	p.ledger = ledger
	if interval == 0 {
		return p.pass(ctx)
	}
	for {
		if err := p.pass(ctx); err != nil && ctx.Err() == nil {
			fmt.Fprintln(p.stderr, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// implementationCases is every ACTIVE case of the mission at implementation work.
func implementationCases(ctx context.Context, cases *workflowcase.Service, mission domain.ID) ([]domain.ID, error) {
	active, err := cases.ListActive(ctx, mission)
	if err != nil {
		return nil, err
	}
	var ids []domain.ID
	for _, c := range active {
		if c.NextWork.Kind == ghtriage.ImplementationWorkKind {
			ids = append(ids, c.ID)
		}
	}
	return ids, nil
}

// recoverExpiredLeases is pipeline stage 1.
func recoverExpiredLeases(ctx context.Context, _ []string) error {
	box, err := openPipelineBox(ctx)
	if err != nil {
		return err
	}
	defer box.Close()
	report, err := box.Recover(ctx)
	if report.ExpiredLeases > 0 || report.ReconciledOperations > 0 {
		fmt.Fprintf(os.Stderr, "recovered %d expired task lease(s), reconciled %d operation(s)\n", report.ExpiredLeases, report.ReconciledOperations)
	}
	return err
}

// openPipelineBox opens the driver composition (no executors or providers)
// for the pipeline's own reads and lease recovery.
func openPipelineBox(ctx context.Context) (*summa42runtime.Box, error) {
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return nil, err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return nil, fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return openGHTriageDriverBox(ctx, summa42runtime.Config{
		StatePath: cfg.DatabasePath, EvidencePath: cfg.EvidencePath,
		CollectiveID: cfg.CollectiveID, OwnerPrincipalID: cfg.OwnerPrincipalID,
		PolicyEngine: material.policyEngine,
	})
}
