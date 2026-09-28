package runtime

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/audit"
	"github.com/SofiaFlux/summa42/internal/capabilities"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/experience"
	"github.com/SofiaFlux/summa42/internal/fieldfeedback"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/memory"
	"github.com/SofiaFlux/summa42/internal/observability"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	"github.com/SofiaFlux/summa42/internal/runmanifest"
	"github.com/SofiaFlux/summa42/internal/scheduler"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/teb"
	"github.com/SofiaFlux/summa42/internal/verification"
	"github.com/SofiaFlux/summa42/internal/wake"
)

const defaultLeaseDuration = 5 * time.Minute

type Config struct {
	StatePath           string
	EvidencePath        string
	Clock               clock.Clock
	CollectiveID        domain.ID
	OwnerPrincipalID    domain.ID
	FieldFeedback       localconfig.FieldFeedbackConfig
	FeedbackSink        fieldfeedback.Sink
	PolicyEngine        policy.PolicyEngine
	OperationProviders  []operations.Provider
	CapabilityProviders []capabilities.Provider
	Executors           map[string]executors.Executor
	ExecutorPreference  scheduler.ExecutorPreference
	TaskClassRouting    map[string]string
	TEBProfile          teb.Profile
	RuntimeVersion      string
	RuntimeCommit       string
	LeaseDuration       time.Duration
}

type Box struct {
	Store         *state.Store
	Clock         clock.Clock
	Purpose       *purpose.Service
	Execution     *execution.Service
	Evidence      *evidence.Store
	Verification  *verification.Service
	Resources     *resources.Service
	RunManifests  *runmanifest.Service
	Approvals     *approvals.Service
	FieldObserver *fieldfeedback.Observer
	Feedback      *fieldfeedback.Feedback
	Sanitizer     fieldfeedback.Sanitizer
	Experience    *experience.Service
	Operations    *operations.Service
	Capabilities  *capabilities.Registry
	Scheduler     *scheduler.Service
	Wake          *wake.Service
	Memory        *memory.Service
	Audit         *audit.Service
	Observability *observability.Bridge
	Policy        policy.PolicyEngine
	Executors     map[string]executors.Executor
	TEBProfile    teb.Profile
	CollectiveID  domain.ID

	shutdown     chan struct{}
	shutdownOnce sync.Once
	closeOnce    sync.Once
	closeErr     error
}

func Open(ctx context.Context, cfg Config) (*Box, error) {
	if ctx == nil {
		return nil, errors.New("runtime context is required")
	}
	cfg.StatePath = strings.TrimSpace(cfg.StatePath)
	cfg.EvidencePath = strings.TrimSpace(cfg.EvidencePath)
	cfg.CollectiveID = domain.ID(strings.TrimSpace(string(cfg.CollectiveID)))
	cfg.OwnerPrincipalID = domain.ID(strings.TrimSpace(string(cfg.OwnerPrincipalID)))
	if cfg.StatePath == "" || cfg.EvidencePath == "" {
		return nil, errors.New("state and evidence paths are required")
	}
	if cfg.CollectiveID == "" {
		return nil, errors.New("collective id is required")
	}
	if cfg.OwnerPrincipalID == "" {
		return nil, errors.New("Owner principal id is required")
	}
	if cfg.PolicyEngine == nil {
		return nil, errors.New("policy engine is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaultLeaseDuration
	}
	if cfg.TEBProfile.Name == "" {
		cfg.TEBProfile = teb.UnenforcedProfile("local-host")
	}
	if err := cfg.TEBProfile.Validate(); err != nil {
		return nil, fmt.Errorf("TEB profile: %w", err)
	}
	policySnapshot, err := capturePolicySnapshot(cfg.PolicyEngine)
	if err != nil {
		return nil, fmt.Errorf("capture policy metadata for Attempt manifests: %w", err)
	}
	buildMetadata := resolveBuildMetadata(cfg.RuntimeVersion, cfg.RuntimeCommit)

	store, err := state.Open(ctx, cfg.StatePath)
	if err != nil {
		return nil, err
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = store.DB().Close()
		}
	}()

	purposes := purpose.New(store, cfg.Clock)
	runManifestSvc := runmanifest.New(store, runmanifest.StaticContext{
		Build:      buildMetadata,
		Policy:     policySnapshot,
		TEBProfile: cfg.TEBProfile,
	})
	executionSvc := execution.New(store, cfg.Clock, purposes, runManifestSvc)
	evidenceStore, err := evidence.New(store, cfg.EvidencePath, cfg.Clock)
	if err != nil {
		return nil, err
	}
	verificationSvc := verification.New(store, cfg.Clock, executionSvc)
	resourceSvc := resources.New(store, cfg.Clock)
	approvalSvc := approvals.New(store, cfg.Clock)
	feedbackSvc := fieldfeedback.NewFeedback(store, cfg.Clock)
	if err := feedbackSvc.ConfigureEmission(executionSvc, cfg.FieldFeedback, cfg.OwnerPrincipalID); err != nil {
		return nil, fmt.Errorf("configure field feedback: %w", err)
	}
	operationProviders := append([]operations.Provider(nil), cfg.OperationProviders...)
	if cfg.FieldFeedback.Enabled && cfg.FieldFeedback.Mode != localconfig.FeedbackModeLocalOnly {
		if cfg.FeedbackSink != nil {
			provider, err := fieldfeedback.NewProvider(
				cfg.FieldFeedback.Provider,
				cfg.FieldFeedback.Destination,
				feedbackSvc,
				cfg.FeedbackSink,
			)
			if err != nil {
				return nil, fmt.Errorf("construct field feedback provider: %w", err)
			}
			operationProviders = append(operationProviders, provider)
		} else if !hasOperationProvider(operationProviders, cfg.FieldFeedback.Provider) {
			return nil, errors.New("field feedback export is enabled but no feedback sink/provider is configured")
		}
	}
	operationsSvc := operations.New(store, cfg.Clock, executionSvc, cfg.PolicyEngine, resourceSvc, approvalSvc, cfg.CollectiveID, operationProviders...)
	capabilityRegistry := capabilities.NewRegistry(store, cfg.Clock, executionSvc, cfg.CapabilityProviders...)
	var schedulerSvc *scheduler.Service
	var wakeSvc *wake.Service
	memorySvc := memory.New(store, cfg.Clock)
	auditSvc := audit.New(store, cfg.Clock)
	fieldObserver := fieldfeedback.NewObserver(store, cfg.Clock, cfg.CollectiveID)
	denyPatterns := make([]*regexp.Regexp, 0, len(cfg.FieldFeedback.DenyPatterns))
	for _, raw := range cfg.FieldFeedback.DenyPatterns {
		pattern, err := regexp.Compile(raw)
		if err != nil {
			return nil, fmt.Errorf("compile field feedback deny pattern: %w", err)
		}
		denyPatterns = append(denyPatterns, pattern)
	}
	sanitizer, err := fieldfeedback.NewDeterministicSanitizer(
		store, cfg.Clock, feedbackSvc,
		fieldfeedback.DeterministicSanitizerConfig{
			Version: "deterministic-v1", DenyPatterns: denyPatterns, AllowExecutorMetadata: false,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("construct field feedback sanitizer: %w", err)
	}
	experienceSvc := experience.New(store, cfg.Clock, approvalSvc, auditSvc, cfg.OwnerPrincipalID)
	preferences := make([]scheduler.ExecutorPreference, 0, 2)
	if len(cfg.TaskClassRouting) > 0 {
		preferences = append(preferences, scheduler.TaskClassRouting(cfg.TaskClassRouting))
	}
	if cfg.ExecutorPreference != nil {
		preferences = append(preferences, cfg.ExecutorPreference)
	} else {
		preferences = append(preferences, experienceSvc)
	}
	schedulerSvc = scheduler.New(store, cfg.Clock, purposes, executionSvc, resourceSvc, cfg.LeaseDuration, preferences...)
	wakeSvc = wake.New(store, cfg.Clock, schedulerSvc)

	executorSet := make(map[string]executors.Executor, len(cfg.Executors)+1)
	for name, executor := range cfg.Executors {
		name = strings.TrimSpace(name)
		if name != "" && executor != nil {
			executorSet[name] = executor
		}
	}
	if cfg.FieldFeedback.Enabled && cfg.FieldFeedback.Mode != localconfig.FeedbackModeLocalOnly {
		emitter, err := fieldfeedback.NewEmitExecutor(feedbackSvc, operationsSvc, cfg.FieldFeedback.Provider)
		if err != nil {
			return nil, fmt.Errorf("construct feedback emitter: %w", err)
		}
		if _, exists := executorSet["feedback-emitter"]; exists {
			return nil, errors.New("feedback-emitter executor kind is reserved")
		}
		executorSet["feedback-emitter"] = emitter
	}

	box := &Box{
		Store:         store,
		Clock:         cfg.Clock,
		Purpose:       purposes,
		Execution:     executionSvc,
		Evidence:      evidenceStore,
		Verification:  verificationSvc,
		Resources:     resourceSvc,
		RunManifests:  runManifestSvc,
		Approvals:     approvalSvc,
		FieldObserver: fieldObserver,
		Feedback:      feedbackSvc,
		Sanitizer:     sanitizer,
		Experience:    experienceSvc,
		Operations:    operationsSvc,
		Capabilities:  capabilityRegistry,
		Scheduler:     schedulerSvc,
		Wake:          wakeSvc,
		Memory:        memorySvc,
		Audit:         auditSvc,
		Observability: observability.New(nil),
		Policy:        cfg.PolicyEngine,
		Executors:     executorSet,
		TEBProfile:    cfg.TEBProfile,
		CollectiveID:  cfg.CollectiveID,
		shutdown:      make(chan struct{}),
	}
	closeStore = false
	return box, nil
}

func (b *Box) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		_ = b.RequestShutdown(context.Background())
		if b.Store != nil && b.Store.DB() != nil {
			b.closeErr = b.Store.DB().Close()
		}
	})
	return b.closeErr
}

func (b *Box) RequestShutdown(context.Context) error {
	if b == nil {
		return errors.New("runtime box is not configured")
	}
	b.shutdownOnce.Do(func() { close(b.shutdown) })
	return nil
}

func (b *Box) ShutdownRequested() <-chan struct{} {
	if b == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return b.shutdown
}

func hasOperationProvider(providers []operations.Provider, name string) bool {
	name = strings.TrimSpace(name)
	for _, provider := range providers {
		if provider != nil && provider.Name() == name {
			return true
		}
	}
	return false
}
