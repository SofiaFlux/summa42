package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/project"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

// extractProjectFlag finds --project <slug> or --project=<slug> in args and
// returns the slug plus the remaining arguments in order.
func extractProjectFlag(args []string) (slug string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var val string
		matched := false
		switch {
		case arg == "--project":
			if i+1 >= len(args) {
				return "", nil, errors.New("--project requires a slug value")
			}
			i++
			val = args[i]
			matched = true
		case strings.HasPrefix(arg, "--project="):
			val = strings.TrimPrefix(arg, "--project=")
			matched = true
		}
		if matched {
			val = strings.TrimSpace(val)
			if val == "" {
				return "", nil, errors.New("--project value cannot be blank")
			}
			if slug != "" {
				return "", nil, errors.New("--project may be passed only once")
			}
			slug = val
			continue
		}
		rest = append(rest, arg)
	}
	return slug, rest, nil
}

// manifestArgsForProject verifies slug's signed manifest against disk and DB,
// then builds equivalent run-gh-intake, run-gh-pipeline, and run-worker argv.
func manifestArgsForProject(ctx context.Context, home string, cfg localconfig.Config, slug string, once bool) (intake, pipeline, worker []string, err error) {
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, nil, nil, err
	}
	defer store.DB().Close()
	eng, _, err := project.LoadActivePolicyEngine(ctx, store)
	if err != nil {
		return nil, nil, nil, err
	}
	proj, m, _, err := project.New(store, clock.System{}).VerifyActiveManifest(ctx, home, slug, eng)
	if err != nil {
		return nil, nil, nil, err
	}
	missionID, _, err := purpose.New(store, clock.System{}).ActiveMission(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read active Mission: %w", err)
	}
	return buildManifestArgs(proj, m, missionID, once)
}

// resolveManifestFlagArgs expands --project <slug> into the argv for kind
// ("intake", "pipeline", or "worker"), rejecting conflicting manual flags.
func resolveManifestFlagArgs(ctx context.Context, slug string, rest []string, kind string) ([]string, error) {
	once := false
	var interval string
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--once":
			once = true
		case strings.HasPrefix(a, "--interval="), strings.HasPrefix(a, "--poll-interval="):
			_, val, _ := strings.Cut(a, "=")
			interval = strings.TrimSpace(val)
		case a == "--interval", a == "--poll-interval":
			if i+1 >= len(rest) {
				return nil, fmt.Errorf("%s requires a duration value", a)
			}
			i++
			interval = strings.TrimSpace(rest[i])
		default:
			return nil, fmt.Errorf("--project cannot be combined with flag %q", a)
		}
	}
	if once && interval != "" {
		return nil, errors.New("--project: --once cannot be combined with an explicit interval")
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return nil, err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return nil, fmt.Errorf("load initialized Collective: %w", err)
	}
	intake, pipeline, worker, err := manifestArgsForProject(ctx, home, cfg, slug, once)
	if err != nil {
		return nil, err
	}
	var argv []string
	intervalFlag := "--poll-interval"
	switch kind {
	case "intake":
		argv = intake
	case "pipeline":
		argv = pipeline
		intervalFlag = "--interval"
	case "worker":
		argv = worker
	default:
		return nil, fmt.Errorf("unsupported project manifest loop kind %q", kind)
	}
	if interval != "" {
		argv = setFlagValue(argv, intervalFlag, interval)
	}
	return argv, nil
}

func setFlagValue(argv []string, flagName, value string) []string {
	prefix := flagName + "="
	out := make([]string, 0, len(argv)+1)
	replaced := false
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			out = append(out, prefix+value)
			replaced = true
			continue
		}
		out = append(out, a)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func buildManifestArgs(proj project.Project, m project.Manifest, missionID domain.ID, once bool) (intake, pipeline, worker []string, err error) {
	// M2-T3 maps the primary GitHub repository entry to single-repo run-* flags;
	// ADO worker wiring (M2-T4) and multi-repo serve loops (M2-T5) extend this.
	var gh *project.RepositorySpec
	for i := range m.Repositories {
		if m.Repositories[i].Host == project.HostGitHub {
			gh = &m.Repositories[i]
			break
		}
	}
	if gh == nil {
		return nil, nil, nil, fmt.Errorf("project %s manifest has no GitHub repository entry", m.Project)
	}
	modeFlag := func(poll string) []string {
		if once {
			return []string{"--once"}
		}
		return []string{"--interval=" + poll}
	}
	poll := strings.TrimSpace(gh.Intake.PollInterval)
	if poll == "" {
		poll = "30s"
	}
	intake = []string{
		"--mission=" + string(missionID),
		"--envelope=" + string(proj.EnvelopeID),
		"--repo=" + gh.Repository,
	}
	for _, login := range gh.Intake.Maintainers {
		intake = append(intake, "--maintainer="+login)
	}
	for _, c := range gh.CaseGrants.Capabilities {
		intake = append(intake, "--grant-capability="+c)
	}
	for _, a := range gh.CaseGrants.Actions {
		intake = append(intake, "--grant-action="+a)
	}
	for _, wc := range gh.CaseGrants.WorkCapabilities {
		intake = append(intake, "--work-capability="+wc)
	}
	if gh.Intake.BotLogin != "" {
		intake = append(intake, "--bot-login="+gh.Intake.BotLogin)
	}
	if gh.Intake.AutoGrantLabel != "" {
		intake = append(intake, "--auto-grant-label="+gh.Intake.AutoGrantLabel)
	}
	intake = append(intake,
		"--credential-file="+gh.CredentialFile,
		fmt.Sprintf("--max-steps=%d", m.Limits.MaxSteps),
		fmt.Sprintf("--remaining-budget=%d", m.Limits.RemainingBudget),
		"--poll-interval="+poll,
	)
	if once {
		intake = append(intake, "--once")
	}

	prodArgs := manifestModelArgv("model", m.Models.Producer)
	revArgs := manifestModelArgv("review-model", m.Models.Reviewer)
	pipeline = slices.Concat([]string{
		"--mission=" + string(missionID),
		"--source-repo=" + gh.LocalPath,
		"--source-repository=" + gh.Repository,
		"--repository=" + gh.Repository,
		"--base-branch=" + gh.BaseBranch,
		"--workspace-root=" + m.WorkspaceRoot,
		"--credential-file=" + gh.CredentialFile,
		fmt.Sprintf("--max-repairs=%d", m.Limits.MaxRepairs),
	}, prodArgs, revArgs, modeFlag(poll))
	for _, p := range gh.AllowedPrefixes {
		pipeline = append(pipeline, "--allowed-prefix="+p)
	}
	for _, p := range gh.AllowedPaths {
		pipeline = append(pipeline, "--allowed-path="+p)
	}
	for _, cf := range gh.ContextFiles {
		pipeline = append(pipeline, "--context-file="+cf)
	}
	if gh.ContextMaxBytes > 0 {
		pipeline = append(pipeline, fmt.Sprintf("--context-max-bytes=%d", gh.ContextMaxBytes))
	}
	if gh.PlanReadMode {
		pipeline = append(pipeline, "--plan-read-mode")
	}
	for _, cmdArgv := range gh.Validation.Commands {
		rawCmd, _ := json.Marshal(cmdArgv)
		pipeline = append(pipeline, "--validation-command-json="+string(rawCmd))
	}
	if gh.Validation.User != "" {
		pipeline = append(pipeline, "--validation-user="+gh.Validation.User)
	}
	if gh.Validation.Timeout != "" {
		pipeline = append(pipeline, "--validation-timeout="+gh.Validation.Timeout)
	}
	if m.Models.FallbackReviewersFile != "" {
		pipeline = append(pipeline, "--fallback-reviewers="+m.Models.FallbackReviewersFile)
	}

	worker = slices.Concat([]string{
		"--workspace-root=" + m.WorkspaceRoot,
		"--poll-interval=" + poll,
		"--credential-file=" + gh.CredentialFile,
	}, prodArgs)
	if m.Limits.LeaseDuration != "" {
		worker = append(worker, "--lease-duration="+m.Limits.LeaseDuration)
	}
	if m.Limits.HeartbeatInterval != "" {
		worker = append(worker, "--heartbeat-interval="+m.Limits.HeartbeatInterval)
	}
	if m.Limits.MaxWallTime != "" {
		worker = append(worker, "--max-wall-time="+m.Limits.MaxWallTime)
	}
	if once {
		worker = append(worker, "--once")
	}
	return intake, pipeline, worker, nil
}

func manifestModelArgv(prefix string, m project.ModelSpec) []string {
	var out []string
	if m.Binary != "" {
		out = append(out, fmt.Sprintf("--%s-binary=%s", prefix, m.Binary))
	}
	if m.Timeout != "" {
		out = append(out, fmt.Sprintf("--%s-timeout=%s", prefix, m.Timeout))
	}
	if m.User != "" {
		out = append(out, fmt.Sprintf("--%s-user=%s", prefix, m.User))
	}
	for _, a := range m.Args {
		out = append(out, fmt.Sprintf("--%s-arg=%s", prefix, a))
	}
	for _, e := range m.EnvAllowlist {
		out = append(out, fmt.Sprintf("--%s-env=%s", prefix, e))
	}
	if m.CommandCodeModel != "" {
		out = append(out, fmt.Sprintf("--%s-commandcode=%s", prefix, m.CommandCodeModel))
	}
	if m.Profile != "" {
		out = append(out, fmt.Sprintf("--%s-profile=%s", prefix, m.Profile))
	}
	if m.Model != "" {
		out = append(out, fmt.Sprintf("--%s-name=%s", prefix, m.Model))
	}
	if m.ProfileDir != "" {
		out = append(out, fmt.Sprintf("--%s-profile-dir=%s", prefix, m.ProfileDir))
	}
	if m.AllowPaid {
		out = append(out, fmt.Sprintf("--%s-allow-paid", prefix))
	}
	if m.ZDR {
		out = append(out, fmt.Sprintf("--%s-zdr", prefix))
	}
	return out
}
