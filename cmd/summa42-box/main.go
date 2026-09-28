package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/SofiaFlux/summa42/internal/adoeffects"
	"github.com/SofiaFlux/summa42/internal/adomcp"
	"github.com/SofiaFlux/summa42/internal/adoreview"
	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/feedbackgithub"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/ghissue"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/ghtriage/climodel"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflow"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

const controlShutdownTimeout = 5 * time.Second

const (
	publishExecutorKind      = "ado-publish"
	publishCommentCapability = "ado.pr.comment"
	publishApproveCapability = "ado.pr.approve"
)

type controlLifecycle interface {
	Serve(net.Listener) error
	Close(context.Context) error
}

type startupMaterial struct {
	policyEngine   *policy.OPAEngine
	ownerPublicKey ed25519.PublicKey
}

type boxStatusProvider struct {
	box *summa42runtime.Box
}

func (p boxStatusProvider) Status(ctx context.Context) (control.StatusDTO, error) {
	if p.box == nil || p.box.Store == nil {
		return control.StatusDTO{}, errors.New("Box status provider is not configured")
	}
	var activeTasks, activeAttempts int
	if err := p.box.Store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM tasks WHERE state NOT IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'EXPIRED')",
	).Scan(&activeTasks); err != nil {
		return control.StatusDTO{}, err
	}
	if err := p.box.Store.DB().QueryRowContext(ctx,
		"SELECT count(*) FROM attempts WHERE lease_state = 'ACTIVE'",
	).Scan(&activeAttempts); err != nil {
		return control.StatusDTO{}, err
	}
	stateName := "DORMANT"
	if activeTasks != 0 || activeAttempts != 0 {
		stateName = "ACTIVE"
	}
	return control.StatusDTO{
		CollectiveID:   p.box.CollectiveID,
		State:          stateName,
		ActiveTasks:    activeTasks,
		ActiveAttempts: activeAttempts,
	}, nil
}

func serveControl(ctx context.Context, listener net.Listener, server controlLifecycle) error {
	if ctx == nil {
		return errors.New("control context is required")
	}
	if listener == nil {
		return errors.New("control listener is required")
	}
	if server == nil {
		return errors.New("control server is required")
	}
	defer listener.Close()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), controlShutdownTimeout)
		defer cancel()
		if err := server.Close(shutdownCtx); err != nil {
			return err
		}
		select {
		case err := <-serveErr:
			return err
		case <-shutdownCtx.Done():
			return shutdownCtx.Err()
		}
	}
}

func run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	feedbackSink, err := buildFeedbackSink(cfg)
	if err != nil {
		return err
	}
	adoProvider, err := buildADOProviderFromEnv()
	if err != nil {
		return err
	}
	var capabilityProviders []capabilities.Provider
	if adoProvider != nil {
		capabilityProviders = append(capabilityProviders, adoProvider)
	}

	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:           cfg.DatabasePath,
		EvidencePath:        cfg.EvidencePath,
		CollectiveID:        cfg.CollectiveID,
		OwnerPrincipalID:    cfg.OwnerPrincipalID,
		FieldFeedback:       cfg.FieldFeedback,
		FeedbackSink:        feedbackSink,
		PolicyEngine:        material.policyEngine,
		CapabilityProviders: capabilityProviders,
	})
	if err != nil {
		return fmt.Errorf("open Box runtime: %w", err)
	}
	defer box.Close()
	if err := assessConfiguredProviders(ctx, box.Capabilities, adoProvider); err != nil {
		return fmt.Errorf("assess configured ADO capability provider: %w", err)
	}

	server, err := control.NewServer(control.ServerConfig{
		AuthToken:        cfg.ControlToken,
		OwnerPrincipalID: cfg.OwnerPrincipalID,
		OwnerPublicKey:   material.ownerPublicKey,
		ChallengeTTL:     2 * time.Minute,
	}, control.Dependencies{
		Status:        boxStatusProvider{box: box},
		Tasks:         box.Execution,
		Missions:      box.Purpose,
		Approvals:     box.Approvals,
		Feedback:      box.Feedback,
		Sanitizer:     box.Sanitizer,
		FieldObserver: box.FieldObserver,
		Experience:    box.Experience,
		Attempts:      box.Execution,
		Operations:    box.Operations,
		Shutdown:      box,
	})
	if err != nil {
		return fmt.Errorf("create control server: %w", err)
	}

	endpoint := strings.TrimSpace(os.Getenv("SUMMA42_CONTROL_ENDPOINT"))
	listener, err := control.ListenLocal(endpoint)
	if err != nil {
		return fmt.Errorf("listen on local control endpoint: %w", err)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-box.ShutdownRequested():
			cancel()
		case <-serveCtx.Done():
		}
	}()
	return serveControl(serveCtx, listener, server)
}

func buildADOProviderFromEnv() (*adomcp.Provider, error) {
	command := strings.TrimSpace(os.Getenv("SUMMA42_ADO_MCP_COMMAND"))
	organization := strings.TrimSpace(os.Getenv("SUMMA42_ADO_ORGANIZATION"))
	if command == "" && organization == "" {
		return nil, nil
	}
	if command == "" || organization == "" {
		return nil, errors.New("ADO MCP requires both SUMMA42_ADO_MCP_COMMAND and SUMMA42_ADO_ORGANIZATION")
	}
	return adomcp.New(adomcp.Config{Command: command, Organization: organization})
}

type publishSettings struct {
	mode        adoreview.PublishMode
	approvers   []domain.ID
	riskComment string
	riskApprove string
}

func publishSettingsFromEnv() (publishSettings, error) {
	mode := strings.TrimSpace(os.Getenv("SUMMA42_PUBLISH_MODE"))
	if mode == "" {
		mode = string(adoreview.PublishNone)
	}
	settings := publishSettings{mode: adoreview.PublishMode(mode)}
	switch settings.mode {
	case adoreview.PublishNone, adoreview.PublishComments, adoreview.PublishAll:
	default:
		return publishSettings{}, fmt.Errorf("unknown publish mode %q", mode)
	}
	for _, approver := range splitCSV(os.Getenv("SUMMA42_PUBLISH_APPROVERS")) {
		settings.approvers = append(settings.approvers, domain.ID(approver))
	}
	settings.riskComment = strings.TrimSpace(os.Getenv("SUMMA42_PUBLISH_RISK_COMMENT"))
	if settings.riskComment == "" {
		settings.riskComment = "LOW"
	}
	settings.riskApprove = strings.TrimSpace(os.Getenv("SUMMA42_PUBLISH_RISK_APPROVE"))
	if settings.mode == adoreview.PublishAll && settings.riskApprove == "" {
		return publishSettings{}, errors.New("publish-all mode requires an explicit approve risk")
	}
	return settings, nil
}

func buildAdoEffectProviders(adoProvider *adomcp.Provider) ([]operations.Provider, error) {
	if adoProvider == nil {
		return nil, nil
	}
	config := adoeffects.Config{
		Command:      strings.TrimSpace(os.Getenv("SUMMA42_ADO_MCP_COMMAND")),
		Organization: strings.TrimSpace(os.Getenv("SUMMA42_ADO_ORGANIZATION")),
	}
	read := adoeffects.ReadFunc(adoProvider.Call)
	commentProvider, err := adoeffects.NewCommentProvider(config, read)
	if err != nil {
		return nil, fmt.Errorf("construct ADO comment provider: %w", err)
	}
	voteProvider, err := adoeffects.NewVoteProvider(config, read)
	if err != nil {
		return nil, fmt.Errorf("construct ADO vote provider: %w", err)
	}
	return []operations.Provider{commentProvider, voteProvider}, nil
}

func buildPublishFromEnv(adoProvider *adomcp.Provider) (publishSettings, []operations.Provider, error) {
	settings, err := publishSettingsFromEnv()
	if err != nil {
		return publishSettings{}, nil, err
	}
	if adoProvider == nil {
		return publishSettings{}, nil, nil
	}
	providers, err := buildAdoEffectProviders(adoProvider)
	if err != nil {
		return publishSettings{}, nil, err
	}
	return settings, providers, nil
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func buildCopilotExecutorFromEnv() (map[string]executors.Executor, error) {
	path := strings.TrimSpace(os.Getenv("SUMMA42_COPILOT_PATH"))
	if path == "" {
		return nil, nil
	}
	server := strings.TrimSpace(os.Getenv("SUMMA42_COPILOT_MCP_SERVER"))
	tools := splitCSV(os.Getenv("SUMMA42_COPILOT_TOOLS"))
	if len(tools) == 0 {
		tools = []string{"repo_pull_request", "repo_pull_request_org", "repo_pull_request_thread", "repo_file", "pipelines_build", "core_list_projects", "wit_work_item"}
	}
	timeout := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("SUMMA42_COPILOT_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid SUMMA42_COPILOT_TIMEOUT %q", raw)
		}
		timeout = parsed
	}
	executor, err := executors.NewCopilotExecutor(executors.CopilotConfig{
		Path: path, Model: strings.TrimSpace(os.Getenv("SUMMA42_COPILOT_MODEL")),
		MCPServer: server, AllowedTools: tools, Timeout: timeout,
		Environment: copilotEnvironmentFromOS(),
	})
	if err != nil {
		return nil, err
	}
	return map[string]executors.Executor{"copilot": executor}, nil
}

// copilotEnvironmentFromOS snapshots the allowlisted OS vars into the child
// environment at build time. Absent vars are omitted, never empty-string injected.
func copilotEnvironmentFromOS() map[string]string {
	allowlisted := []string{
		"PATH", "COPILOT_MODEL", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "TMPDIR", "TMP", "TEMP",
	}
	env := make(map[string]string, len(allowlisted))
	for _, key := range allowlisted {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}

type capabilityAssessor interface {
	AssessProvider(context.Context, string) ([]capabilities.Assessment, error)
}

func assessConfiguredProviders(ctx context.Context, assessor capabilityAssessor, provider *adomcp.Provider) error {
	if provider == nil {
		return nil
	}
	if assessor == nil {
		return errors.New("capability assessor is required")
	}
	_, err := assessor.AssessProvider(ctx, provider.Name())
	return err
}

func buildFeedbackSink(cfg localconfig.Config) (fieldfeedback.Sink, error) {
	if !cfg.FieldFeedback.Enabled || cfg.FieldFeedback.Mode == localconfig.FeedbackModeLocalOnly {
		return nil, nil
	}
	switch cfg.FieldFeedback.Provider {
	case "github":
		tokenFile := strings.TrimSpace(os.Getenv("SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE"))
		if tokenFile == "" {
			return nil, errors.New("GitHub feedback export requires SUMMA42_FEEDBACK_GITHUB_TOKEN_FILE")
		}
		apiBaseURL := strings.TrimSpace(os.Getenv("SUMMA42_FEEDBACK_GITHUB_API_BASE_URL"))
		sink, err := feedbackgithub.NewSink(feedbackgithub.Config{
			APIBaseURL:       apiBaseURL,
			Repository:       cfg.FieldFeedback.Destination,
			CredentialSource: feedbackgithub.FileCredentialSource{Path: tokenFile},
		})
		if err != nil {
			return nil, fmt.Errorf("configure GitHub feedback sink: %w", err)
		}
		return sink, nil
	default:
		return nil, fmt.Errorf("unsupported field feedback provider %q", cfg.FieldFeedback.Provider)
	}
}

func loadStartupMaterial(ctx context.Context, cfg localconfig.Config) (startupMaterial, error) {
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return startupMaterial{}, fmt.Errorf("open canonical state for startup: %w", err)
	}
	defer store.DB().Close()

	var (
		policySetID      domain.ID
		moduleName       string
		module           []byte
		policyHash       string
		capabilitiesHash string
	)
	if err := store.DB().QueryRowContext(ctx,
		"SELECT policy_set_id, module_name, module, policy_hash, capabilities_hash FROM policy_sets WHERE active = 1",
	).Scan(&policySetID, &moduleName, &module, &policyHash, &capabilitiesHash); err != nil {
		return startupMaterial{}, fmt.Errorf("load active policy set: %w", err)
	}
	if policySetID != cfg.ActivePolicySetID {
		return startupMaterial{}, fmt.Errorf("active policy set %s does not match config %s", policySetID, cfg.ActivePolicySetID)
	}
	engine := policy.NewOPAEngine(policy.OPAConfig{
		ModuleName:    moduleName,
		Module:        string(module),
		PolicySetID:   policySetID,
		PolicySetHash: policyHash,
	})
	metadata, err := engine.Metadata()
	if err != nil {
		return startupMaterial{}, fmt.Errorf("validate active policy set: %w", err)
	}
	if metadata.PolicyCapabilitiesHash != capabilitiesHash {
		return startupMaterial{}, errors.New("active policy capability profile does not match the runtime safe profile")
	}

	var publicKey []byte
	if err := store.DB().QueryRowContext(ctx,
		"SELECT public_key FROM principals WHERE principal_id = ? AND principal_kind = 'OWNER'",
		cfg.OwnerPrincipalID,
	).Scan(&publicKey); err != nil {
		return startupMaterial{}, fmt.Errorf("load Owner public key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return startupMaterial{}, fmt.Errorf("Owner public key has invalid size %d", len(publicKey))
	}
	return startupMaterial{
		policyEngine:   engine,
		ownerPublicKey: append(ed25519.PublicKey(nil), publicKey...),
	}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "run-worker" {
		if err := runWorker(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "run-observer" {
		if err := runObserver(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "run-gh-intake" {
		if err := runGHIntake(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "run-driver" {
		if err := runDriver(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "run-final-verifier" {
		if err := runFinalVerifier(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func parseWorkerFlags(args []string) (pollInterval time.Duration, leaseDuration time.Duration, modelConfig climodel.Config, err error) {
	flags := flag.NewFlagSet("run-worker", flag.ContinueOnError)
	flags.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "interval between scheduler polls")
	flags.DurationVar(&leaseDuration, "lease-duration", 0, "attempt lease duration (0 uses Box default)")
	flags.String("model-binary", "codex", "model binary classifying a triage issue")
	flags.String("model-timeout", "60s", "per-invocation model timeout")
	if err := flags.Parse(args); err != nil {
		return 0, 0, climodel.Config{}, err
	}
	if pollInterval <= 0 {
		return 0, 0, climodel.Config{}, errors.New("run-worker requires a positive --poll-interval")
	}
	if leaseDuration < 0 {
		return 0, 0, climodel.Config{}, errors.New("run-worker requires a non-negative --lease-duration")
	}
	modelConfig, err = triageModelConfig(flags)
	if err != nil {
		return 0, 0, climodel.Config{}, err
	}
	return pollInterval, leaseDuration, modelConfig, nil
}

// triageModelConfig reads the model flags off any flag set, so run-worker,
// run-gh-triage-driver and run-gh-triage-review agree on the model; the flag
// set must have registered both of them. A flag value that cannot be honoured
// is a configuration mistake and stays a startup error: the degradation in
// triageModelAdapter covers a model binary that is not on PATH, never a flag
// that cannot be parsed and never a value that was never given.
func triageModelConfig(flags *flag.FlagSet) (climodel.Config, error) {
	binaryFlag := flags.Lookup("model-binary")
	if binaryFlag == nil {
		return climodel.Config{}, errors.New("model flags are not registered: --model-binary is missing from the flag set")
	}
	timeoutFlag := flags.Lookup("model-timeout")
	if timeoutFlag == nil {
		return climodel.Config{}, errors.New("model flags are not registered: --model-timeout is missing from the flag set")
	}
	binary := binaryFlag.Value.String()
	timeout := timeoutFlag.Value.String()
	if strings.TrimSpace(binary) == "" {
		return climodel.Config{}, fmt.Errorf("run-worker requires a non-empty --model-binary, got %q", binary)
	}
	duration, err := time.ParseDuration(timeout)
	if err != nil {
		return climodel.Config{}, fmt.Errorf("parse --model-timeout: %w", err)
	}
	if duration <= 0 {
		return climodel.Config{}, fmt.Errorf("run-worker requires a positive --model-timeout, got %q", timeout)
	}
	return climodel.Config{Binary: binary, Timeout: duration}, nil
}

// triageModelAdapter builds the classifier the triage executor needs and reports
// whether that executor can run here at all. A model binary that is not on PATH
// is a missing optional dependency, not a misconfiguration: the box keeps
// starting and keeps serving every other task, exactly as it does when
// SUMMA42_COPILOT_PATH is unset. The caller registers no executor and no routing
// entry in that case, so github.issue.read is never advertised and triage Tasks
// stay unclaimed rather than being claimed by an executor that cannot run.
func triageModelAdapter(modelConfig climodel.Config, stderr io.Writer) (*climodel.Adapter, bool) {
	adapter, err := climodel.New(modelConfig)
	if err != nil {
		fmt.Fprintf(stderr, "executor kind %s is not registered: %v\n", ghtriage.ExecutorKind, err)
		return nil, false
	}
	return adapter, true
}

// installTriageExecutor puts the triage executor in registry when the model
// binary can actually be invoked, and reports whether it did. A model that
// cannot be invoked leaves the registry untouched: no executor means no
// github.issue.read in the capacity derived from that registry, so triage Tasks
// stay unclaimed rather than being claimed by an executor whose classifier is
// nil and fails every attempt. The evidence store is the Box's, so this runs
// after the runtime is open; the model verdict that gates the routing runs
// before it and hands this call io.Discard, so a degraded box still prints
// exactly one notice.
func installTriageExecutor(registry map[string]executors.Executor, evidenceStore *evidence.Store, modelConfig climodel.Config, stderr io.Writer) bool {
	adapter, usable := triageModelAdapter(modelConfig, stderr)
	if !usable {
		return false
	}
	registry[ghtriage.ExecutorKind] = ghtriage.NewExecutor(evidenceStore, adapter)
	return true
}

func splitWorkspaceRootArg(args []string) (workspaceRoot string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--workspace-root" {
			if i+1 >= len(args) {
				return "", nil, errors.New("run-worker requires a value for --workspace-root")
			}
			workspaceRoot = args[i+1]
			i++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--workspace-root="); ok {
			workspaceRoot = value
			continue
		}
		rest = append(rest, arg)
	}
	return workspaceRoot, rest, nil
}

func workerCapacity(box *summa42runtime.Box) (scheduler.CapacitySnapshot, error) {
	caps := make(map[string]scheduler.CapabilityCapacity, len(box.Executors)+2)
	for kind := range box.Executors {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		caps[kind] = scheduler.CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
	}
	if _, registered := caps[publishExecutorKind]; registered {
		for _, capability := range []string{publishCommentCapability, publishApproveCapability} {
			caps[capability] = scheduler.CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
		}
	}
	if _, registered := caps[ghtriage.ExecutorKind]; registered {
		caps[ghtriage.RequiredCapability] = scheduler.CapabilityCapacity{Accessible: true, Enforcement: domain.EnforcementEnforced}
	}
	if len(caps) == 0 {
		return scheduler.CapacitySnapshot{}, errors.New("run-worker has no schedulable capabilities: the Box executor registry is empty, so there is no capability source to advertise")
	}
	return scheduler.CapacitySnapshot{Capabilities: caps}, nil
}

func runWorker(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	workspaceRoot, rest, err := splitWorkspaceRootArg(args)
	if err != nil {
		return err
	}
	pollInterval, leaseDuration, modelConfig, err := parseWorkerFlags(rest)
	if err != nil {
		return err
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return errors.New("run-worker requires --workspace-root")
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	feedbackSink, err := buildFeedbackSink(cfg)
	if err != nil {
		return err
	}
	adoProvider, err := buildADOProviderFromEnv()
	if err != nil {
		return err
	}
	var capabilityProviders []capabilities.Provider
	if adoProvider != nil {
		capabilityProviders = append(capabilityProviders, adoProvider)
	}
	publishCfg, operationProviders, err := buildPublishFromEnv(adoProvider)
	if err != nil {
		return err
	}
	if adoProvider == nil {
		fmt.Fprintln(os.Stderr, "executor kind ado-publish is not registered: SUMMA42_ADO_MCP_COMMAND and SUMMA42_ADO_ORGANIZATION are not set")
	}

	runtimeCfg := summa42runtime.Config{
		StatePath:           cfg.DatabasePath,
		EvidencePath:        cfg.EvidencePath,
		CollectiveID:        cfg.CollectiveID,
		OwnerPrincipalID:    cfg.OwnerPrincipalID,
		FieldFeedback:       cfg.FieldFeedback,
		FeedbackSink:        feedbackSink,
		PolicyEngine:        material.policyEngine,
		OperationProviders:  operationProviders,
		CapabilityProviders: capabilityProviders,
	}
	copilotExecutors, err := buildCopilotExecutorFromEnv()
	if err != nil {
		return err
	}
	if copilotExecutors == nil {
		fmt.Fprintln(os.Stderr, "executor kind copilot is not registered: SUMMA42_COPILOT_PATH is not set")
	} else {
		if runtimeCfg.Executors == nil {
			runtimeCfg.Executors = make(map[string]executors.Executor, len(copilotExecutors))
		}
		for kind, executor := range copilotExecutors {
			runtimeCfg.Executors[kind] = executor
		}
	}
	if leaseDuration > 0 {
		runtimeCfg.LeaseDuration = leaseDuration
	}
	_, triageUsable := triageModelAdapter(modelConfig, os.Stderr)
	if triageUsable {
		runtimeCfg.TaskClassRouting = map[string]string{ghtriage.TaskClass: ghtriage.ExecutorKind}
	}
	box, err := summa42runtime.Open(ctx, runtimeCfg)
	if err != nil {
		return fmt.Errorf("open Box runtime: %w", err)
	}
	defer box.Close()
	// The verdict above already printed the degradation notice and decided the
	// routing, so the registration pass must not print it a second time.
	installTriageExecutor(box.Executors, box.Evidence, modelConfig, io.Discard)
	if adoProvider != nil {
		publisher, err := adoreview.NewPublisher(adoreview.PublishConfig{
			Mode:           publishCfg.mode,
			Operations:     box.Operations,
			Evidence:       box.Evidence,
			OwnerApprovals: publishCfg.approvers,
			RiskComment:    publishCfg.riskComment,
			RiskApprove:    publishCfg.riskApprove,
		})
		if err != nil {
			return fmt.Errorf("construct ado-publish executor: %w", err)
		}
		box.Executors[publishExecutorKind] = publisher
	}
	if err := assessConfiguredProviders(ctx, box.Capabilities, adoProvider); err != nil {
		return fmt.Errorf("assess configured ADO capability provider: %w", err)
	}
	capacity, err := workerCapacity(box)
	if err != nil {
		return err
	}
	worker, err := scheduler.NewWorker(box.Scheduler, box.Execution, box.Evidence, box.Verification, box.Executors, box.Clock, workspaceRoot)
	if err != nil {
		return err
	}
	return worker.Run(ctx, capacity, pollInterval)
}

type stringSlice []string

func (s *stringSlice) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func parseObserverFlags(args []string) (adoreview.Config, time.Duration, error) {
	var cfg adoreview.Config
	var mission, reviewer, envelope string
	var grantCaps, grantActions, workCaps stringSlice
	var maxSteps int
	var remainingBudget int64
	var pollInterval time.Duration
	flags := flag.NewFlagSet("run-observer", flag.ContinueOnError)
	flags.StringVar(&mission, "mission", "", "mission ID to attach observed review cases to")
	flags.StringVar(&reviewer, "reviewer-id", "", "ADO reviewer identity to observe")
	flags.Var(&grantCaps, "grant-capability", "capability granted to review work (repeatable)")
	flags.Var(&grantActions, "grant-action", "action granted to review work (repeatable)")
	flags.Var(&workCaps, "work-capability", "required work capability (repeatable, defaults to grant capabilities)")
	flags.StringVar(&envelope, "envelope", "", "resource envelope ID for materialized review tasks")
	flags.IntVar(&maxSteps, "max-steps", 0, "maximum steps for observed review cases")
	flags.Int64Var(&remainingBudget, "remaining-budget", 0, "remaining budget for observed review cases")
	flags.StringVar(&cfg.Project, "project", "", "ADO project scope (empty uses org-active scope)")
	flags.StringVar(&cfg.Repository, "repository", "", "ADO repository scope")
	flags.DurationVar(&pollInterval, "poll-interval", 5*time.Minute, "interval between observer polls")
	if err := flags.Parse(args); err != nil {
		return adoreview.Config{}, 0, err
	}
	if strings.TrimSpace(mission) == "" {
		return adoreview.Config{}, 0, errors.New("run-observer requires --mission")
	}
	if strings.TrimSpace(reviewer) == "" {
		return adoreview.Config{}, 0, errors.New("run-observer requires --reviewer-id")
	}
	if len(grantCaps) == 0 {
		return adoreview.Config{}, 0, errors.New("run-observer requires at least one --grant-capability (refusing to observe with an empty grant)")
	}
	if strings.TrimSpace(envelope) == "" {
		return adoreview.Config{}, 0, errors.New("run-observer requires --envelope")
	}
	if maxSteps <= 0 {
		return adoreview.Config{}, 0, errors.New("run-observer requires a positive --max-steps")
	}
	if remainingBudget <= 0 {
		return adoreview.Config{}, 0, errors.New("run-observer requires a positive --remaining-budget")
	}
	if pollInterval <= 0 {
		return adoreview.Config{}, 0, errors.New("run-observer requires a positive --poll-interval")
	}
	if (strings.TrimSpace(cfg.Project) == "") != (strings.TrimSpace(cfg.Repository) == "") {
		return adoreview.Config{}, 0, errors.New("run-observer requires --project and --repository together (optional pair)")
	}
	cfg.MissionID = domain.ID(mission)
	cfg.ReviewerID = reviewer
	cfg.Grant = workflow.Grant{Capabilities: []string(grantCaps), Actions: []string(grantActions)}
	cfg.WorkCapabilities = []string(workCaps)
	cfg.ResourceEnvelopeID = domain.ID(envelope)
	cfg.MaxSteps = maxSteps
	cfg.RemainingBudget = remainingBudget
	return cfg, pollInterval, nil
}

func runObserver(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	observerCfg, pollInterval, err := parseObserverFlags(args)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	feedbackSink, err := buildFeedbackSink(cfg)
	if err != nil {
		return err
	}
	adoProvider, err := buildADOProviderFromEnv()
	if err != nil {
		return err
	}
	if adoProvider == nil {
		return errors.New("run-observer requires ADO MCP configuration (SUMMA42_ADO_MCP_COMMAND and SUMMA42_ADO_ORGANIZATION)")
	}
	var capabilityProviders []capabilities.Provider
	capabilityProviders = append(capabilityProviders, adoProvider)

	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:           cfg.DatabasePath,
		EvidencePath:        cfg.EvidencePath,
		CollectiveID:        cfg.CollectiveID,
		OwnerPrincipalID:    cfg.OwnerPrincipalID,
		FieldFeedback:       cfg.FieldFeedback,
		FeedbackSink:        feedbackSink,
		PolicyEngine:        material.policyEngine,
		CapabilityProviders: capabilityProviders,
	})
	if err != nil {
		return fmt.Errorf("open Box runtime: %w", err)
	}
	defer box.Close()
	if err := assessConfiguredProviders(ctx, box.Capabilities, adoProvider); err != nil {
		return fmt.Errorf("assess configured ADO capability provider: %w", err)
	}
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	return adoreview.Run(ctx, adoProvider, cases, box.Execution, box.Evidence, observerCfg, pollInterval)
}

func parseGHIntakeFlags(args []string) (ghissue.ObserveConfig, time.Duration, error) {
	var cfg ghissue.ObserveConfig
	var mission, envelope, repository string
	var maintainers, grantCaps, workCaps stringSlice
	var maxSteps int
	var remainingBudget int64
	var pollInterval time.Duration

	flags := flag.NewFlagSet("run-gh-intake", flag.ContinueOnError)
	flags.StringVar(&mission, "mission", "", "mission ID for GitHub issue cases")
	flags.StringVar(&repository, "repo", "", "GitHub repository as owner/name")
	flags.Var(&maintainers, "maintainer", "maintainer login eligible to open issues (repeatable)")
	flags.Var(&grantCaps, "grant-capability", "capability granted to issue cases (repeatable; must include github.issue.read)")
	flags.Var(&workCaps, "work-capability", "additional work capability (repeatable)")
	flags.StringVar(&envelope, "envelope", "", "resource envelope ID for triage tasks")
	flags.IntVar(&maxSteps, "max-steps", 3, "maximum workflow steps per issue case")
	flags.Int64Var(&remainingBudget, "remaining-budget", 10, "remaining workflow budget per issue case")
	flags.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "interval between intake polls")
	if err := flags.Parse(args); err != nil {
		return ghissue.ObserveConfig{}, 0, err
	}
	if strings.TrimSpace(mission) == "" {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires --mission")
	}
	if strings.TrimSpace(repository) == "" {
		repository = strings.TrimSpace(os.Getenv("SUMMA42_GITHUB_REPOSITORY"))
	}
	if strings.TrimSpace(repository) == "" {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires --repo or SUMMA42_GITHUB_REPOSITORY")
	}
	if flags.NArg() != 0 {
		return ghissue.ObserveConfig{}, 0, fmt.Errorf("run-gh-intake takes no positional arguments, got %q", flags.Args())
	}
	logins := make([]string, 0, len(maintainers))
	for _, login := range maintainers {
		trimmed := strings.TrimSpace(login)
		if trimmed == "" {
			return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake --maintainer must not be blank")
		}
		logins = append(logins, trimmed)
	}
	if len(logins) == 0 {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires at least one --maintainer")
	}
	if strings.ContainsAny(repository, " \t") || !strings.Contains(repository, "/") {
		return ghissue.ObserveConfig{}, 0, fmt.Errorf("run-gh-intake repository %q must be owner/name", repository)
	}
	if strings.TrimSpace(envelope) == "" {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires --envelope")
	}
	if len(grantCaps) == 0 {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires --grant-capability github.issue.read")
	}
	capabilities := make([]string, 0, len(grantCaps))
	granted := make(map[string]struct{}, len(grantCaps))
	for _, capability := range grantCaps {
		if trimmed := strings.TrimSpace(capability); trimmed != "" {
			capabilities = append(capabilities, trimmed)
			granted[trimmed] = struct{}{}
		}
	}
	if _, ok := granted["github.issue.read"]; !ok {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake grant must include github.issue.read")
	}
	work := make([]string, 0, len(workCaps))
	for _, capability := range workCaps {
		trimmed := strings.TrimSpace(capability)
		if _, ok := granted[trimmed]; !ok {
			return ghissue.ObserveConfig{}, 0, fmt.Errorf("work capability %q is not listed in the grant", trimmed)
		}
		work = append(work, trimmed)
	}
	if maxSteps <= 0 {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires a positive --max-steps")
	}
	if remainingBudget <= 0 {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires a positive --remaining-budget")
	}
	if pollInterval <= 0 {
		return ghissue.ObserveConfig{}, 0, errors.New("run-gh-intake requires a positive --poll-interval")
	}
	cfg.MissionID = domain.ID(strings.TrimSpace(mission))
	cfg.Repository = strings.TrimSpace(repository)
	cfg.Maintainers = logins
	cfg.Grant = workflow.Grant{Capabilities: capabilities}
	cfg.WorkCapabilities = work
	cfg.ResourceEnvelopeID = domain.ID(strings.TrimSpace(envelope))
	cfg.MaxSteps = maxSteps
	cfg.RemainingBudget = remainingBudget
	return cfg, pollInterval, nil
}

// openGHIntakeBox opens the runtime with every writable component removed:
// intake only reads GitHub, so no executor, capability provider, feedback sink,
// or operation provider may exist in this composition.
func openGHIntakeBox(ctx context.Context, cfg summa42runtime.Config) (*summa42runtime.Box, error) {
	if ctx == nil {
		return nil, errors.New("Box context is required")
	}
	cfg.FieldFeedback = localconfig.FieldFeedbackConfig{Enabled: false, Mode: localconfig.FeedbackModeLocalOnly}
	cfg.FeedbackSink = nil
	cfg.OperationProviders = nil
	cfg.CapabilityProviders = nil
	cfg.Executors = nil
	return summa42runtime.Open(ctx, cfg)
}

// ghIntakeClientConfig builds the read-only client configuration from the
// resolved repository and the environment. BaseURL stays unset so the client
// applies its api.github.com default: the loopback override is a test seam the
// CLI cannot reach.
func ghIntakeClientConfig(repository string) ghissue.Config {
	return ghissue.Config{
		Repository: repository,
		TokenFile:  strings.TrimSpace(os.Getenv("SUMMA42_GITHUB_TOKEN_FILE")),
	}
}

// ghIntakeTickReporter writes one summary line per tick. A tick that failed
// nothing still prints, so an idle intake and a durable-write failure are
// distinguishable; stdout stays clean for machine consumption.
func ghIntakeTickReporter(out io.Writer) func(ghissue.ObserveResult, error) {
	return func(result ghissue.ObserveResult, err error) {
		fmt.Fprintln(out, formatGHIntakeTick(result, err))
	}
}

// formatGHIntakeTick renders one tick as a single line: the ensured and
// materialized counts, every exclusion with its reason token, issue identity
// and retained cause, every failure with its error text, the skipped pull
// requests and the tick error when there is one. The bracketed detail is
// omitted while its bucket is empty, and a tick error is printed only when the
// failure bucket is empty, so no per-issue failure is stated twice.
func formatGHIntakeTick(result ghissue.ObserveResult, err error) string {
	line := fmt.Sprintf("gh-intake tick ensured=%d materialized=%d excluded=%d",
		len(result.Ensured), len(result.Materialized), len(result.Excluded))
	if len(result.Excluded) > 0 {
		exclusions := make([]string, 0, len(result.Excluded))
		for _, excluded := range result.Excluded {
			exclusions = append(exclusions, formatGHIntakeExclusion(excluded))
		}
		line += " [" + strings.Join(exclusions, ", ") + "]"
	}
	line += fmt.Sprintf(" failed=%d", len(result.Failed))
	if len(result.Failed) > 0 {
		failures := make([]string, 0, len(result.Failed))
		for _, failed := range result.Failed {
			failures = append(failures, failed.Issue.ObjectID()+": "+failed.Err)
		}
		line += " [" + strings.Join(failures, ", ") + "]"
	}
	line += fmt.Sprintf(" pull_requests_skipped=%d", result.PullRequestsSkipped)
	if err != nil && len(result.Failed) == 0 {
		line += " error=" + err.Error()
	}
	return line
}

// formatGHIntakeExclusion renders one exclusion as its reason token, the issue
// identity when the row carries one, and the retained cause. An unparseable
// item never parsed, so its row has no identity to report.
func formatGHIntakeExclusion(excluded ghissue.ExcludedIssue) string {
	entry := excluded.Reason
	if excluded.Issue.Repository != "" || excluded.Issue.Number != 0 {
		entry += " " + excluded.Issue.ObjectID()
	}
	if excluded.Detail != "" {
		entry += ": " + excluded.Detail
	}
	return entry
}

func runGHIntake(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	observerCfg, pollInterval, err := parseGHIntakeFlags(args)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	client, err := ghissue.New(ghIntakeClientConfig(observerCfg.Repository))
	if err != nil {
		return fmt.Errorf("construct read-only GitHub issues client: %w", err)
	}
	box, err := openGHIntakeBox(ctx, summa42runtime.Config{
		StatePath:        cfg.DatabasePath,
		EvidencePath:     cfg.EvidencePath,
		CollectiveID:     cfg.CollectiveID,
		OwnerPrincipalID: cfg.OwnerPrincipalID,
		PolicyEngine:     material.policyEngine,
	})
	if err != nil {
		return fmt.Errorf("open read-only Box runtime: %w", err)
	}
	defer box.Close()
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	return ghissue.RunReporting(ctx, client, cases, box.Execution, box.Evidence, observerCfg, pollInterval,
		ghIntakeTickReporter(os.Stderr))
}

func parseDriverFlags(args []string) (adoreview.DriverConfig, time.Duration, error) {
	var cfg adoreview.DriverConfig
	var mission, envelope string
	var pollInterval time.Duration
	flags := flag.NewFlagSet("run-driver", flag.ContinueOnError)
	flags.StringVar(&mission, "mission", "", "mission ID for driver cases")
	flags.StringVar(&envelope, "envelope", "", "resource envelope ID for materialized publish tasks")
	flags.StringVar(&cfg.Project, "project", "", "ADO project fallback for PR project resolution")
	flags.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "interval between driver polls")
	if err := flags.Parse(args); err != nil {
		return adoreview.DriverConfig{}, 0, err
	}
	if strings.TrimSpace(mission) == "" {
		return adoreview.DriverConfig{}, 0, errors.New("run-driver requires --mission")
	}
	if strings.TrimSpace(envelope) == "" {
		return adoreview.DriverConfig{}, 0, errors.New("run-driver requires --envelope")
	}
	if pollInterval <= 0 {
		return adoreview.DriverConfig{}, 0, errors.New("run-driver requires a positive --poll-interval")
	}
	cfg.MissionID = domain.ID(strings.TrimSpace(mission))
	cfg.ResourceEnvelopeID = domain.ID(strings.TrimSpace(envelope))
	cfg.Project = strings.TrimSpace(cfg.Project)
	return cfg, pollInterval, nil
}

func runDriver(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	driverCfg, pollInterval, err := parseDriverFlags(args)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	feedbackSink, err := buildFeedbackSink(cfg)
	if err != nil {
		return err
	}
	adoProvider, err := buildADOProviderFromEnv()
	if err != nil {
		return err
	}
	if adoProvider == nil {
		return errors.New("run-driver requires ADO MCP configuration (SUMMA42_ADO_MCP_COMMAND and SUMMA42_ADO_ORGANIZATION)")
	}
	capabilityProviders := []capabilities.Provider{adoProvider}
	operationProviders, err := buildAdoEffectProviders(adoProvider)
	if err != nil {
		return err
	}
	if len(operationProviders) != 2 {
		return errors.New("run-driver requires ADO comment and vote providers")
	}

	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:           cfg.DatabasePath,
		EvidencePath:        cfg.EvidencePath,
		CollectiveID:        cfg.CollectiveID,
		OwnerPrincipalID:    cfg.OwnerPrincipalID,
		FieldFeedback:       cfg.FieldFeedback,
		FeedbackSink:        feedbackSink,
		PolicyEngine:        material.policyEngine,
		OperationProviders:  operationProviders,
		CapabilityProviders: capabilityProviders,
	})
	if err != nil {
		return fmt.Errorf("open Box runtime: %w", err)
	}
	defer box.Close()
	if err := assessConfiguredProviders(ctx, box.Capabilities, adoProvider); err != nil {
		return fmt.Errorf("assess configured ADO capability provider: %w", err)
	}
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	driver, err := adoreview.NewDriver(cases, box.Execution, box.Evidence, box.RunManifests, adoreview.DriverConfig{
		MissionID:          driverCfg.MissionID,
		ResourceEnvelopeID: driverCfg.ResourceEnvelopeID,
		Comment:            operationProviders[0],
		Vote:               operationProviders[1],
		Caller:             adoProvider,
		Project:            driverCfg.Project,
	})
	if err != nil {
		return fmt.Errorf("construct ADO workflow driver: %w", err)
	}
	return driver.Run(ctx, pollInterval)
}

const (
	defaultFinalVerifierID   = "final-verifier"
	defaultFinalVerifierType = "AUTOMATED"
)

// The default CLI identity is final-verifier/AUTOMATED; deployments can override both values.
type finalVerifierFlags struct {
	MissionID    domain.ID
	Project      string
	VerifierID   domain.ID
	VerifierType string
}

func parseFinalVerifierFlags(args []string) (finalVerifierFlags, time.Duration, error) {
	var mission, project, verifierID, verifierType string
	var pollInterval time.Duration
	flags := flag.NewFlagSet("run-final-verifier", flag.ContinueOnError)
	flags.StringVar(&mission, "mission", "", "mission ID for final verification cases")
	flags.StringVar(&project, "project", "", "ADO project scope (empty uses stored payload scope)")
	flags.StringVar(&verifierID, "verifier-id", defaultFinalVerifierID, "independent final verifier identity")
	flags.StringVar(&verifierType, "verifier-type", defaultFinalVerifierType, "independent final verifier type")
	flags.DurationVar(&pollInterval, "poll-interval", 30*time.Second, "interval between final verifier polls")
	if err := flags.Parse(args); err != nil {
		return finalVerifierFlags{}, 0, err
	}
	mission = strings.TrimSpace(mission)
	project = strings.TrimSpace(project)
	verifierID = strings.TrimSpace(verifierID)
	verifierType = strings.TrimSpace(verifierType)
	if mission == "" {
		return finalVerifierFlags{}, 0, errors.New("run-final-verifier requires --mission")
	}
	if verifierID == "" {
		return finalVerifierFlags{}, 0, errors.New("run-final-verifier requires a non-empty --verifier-id")
	}
	if verifierType == "" {
		return finalVerifierFlags{}, 0, errors.New("run-final-verifier requires a non-empty --verifier-type")
	}
	if pollInterval <= 0 {
		return finalVerifierFlags{}, 0, errors.New("run-final-verifier requires a positive --poll-interval")
	}
	return finalVerifierFlags{
		MissionID:    domain.ID(mission),
		Project:      project,
		VerifierID:   domain.ID(verifierID),
		VerifierType: verifierType,
	}, pollInterval, nil
}

func runFinalVerifier(ctx context.Context, args []string) error {
	if ctx == nil {
		return errors.New("Box context is required")
	}
	verifierCfg, pollInterval, err := parseFinalVerifierFlags(args)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return fmt.Errorf("load initialized Collective: %w", err)
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	feedbackSink, err := buildFeedbackSink(cfg)
	if err != nil {
		return err
	}
	adoProvider, err := buildADOProviderFromEnv()
	if err != nil {
		return err
	}
	if adoProvider == nil {
		return errors.New("run-final-verifier requires ADO MCP configuration (SUMMA42_ADO_MCP_COMMAND and SUMMA42_ADO_ORGANIZATION)")
	}
	capabilityProviders := []capabilities.Provider{adoProvider}
	operationProviders, err := buildAdoEffectProviders(adoProvider)
	if err != nil {
		return err
	}
	if len(operationProviders) != 2 {
		return errors.New("run-final-verifier requires ADO comment and vote providers")
	}

	box, err := summa42runtime.Open(ctx, summa42runtime.Config{
		StatePath:           cfg.DatabasePath,
		EvidencePath:        cfg.EvidencePath,
		CollectiveID:        cfg.CollectiveID,
		OwnerPrincipalID:    cfg.OwnerPrincipalID,
		FieldFeedback:       cfg.FieldFeedback,
		FeedbackSink:        feedbackSink,
		PolicyEngine:        material.policyEngine,
		OperationProviders:  operationProviders,
		CapabilityProviders: capabilityProviders,
	})
	if err != nil {
		return fmt.Errorf("open Box runtime: %w", err)
	}
	defer box.Close()
	if err := assessConfiguredProviders(ctx, box.Capabilities, adoProvider); err != nil {
		return fmt.Errorf("assess configured ADO capability provider: %w", err)
	}
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	verifier, err := adoreview.NewFinalVerifier(cases, box.Execution, box.Evidence, box.Verification, adoreview.FinalVerifierConfig{
		MissionID:    verifierCfg.MissionID,
		Project:      verifierCfg.Project,
		Comment:      operationProviders[0],
		Vote:         operationProviders[1],
		VerifierID:   verifierCfg.VerifierID,
		VerifierType: verifierCfg.VerifierType,
	})
	if err != nil {
		return fmt.Errorf("construct ADO final verifier: %w", err)
	}
	return verifier.Run(ctx, pollInterval)
}
