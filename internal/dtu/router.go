package dtu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var (
	ErrProviderNotFound  = errors.New("environment provider not found for service")
	ErrModeUnavailable   = errors.New("requested environment mode unavailable for service")
	ErrInvalidMode       = errors.New("invalid environment mode")
	ErrInvalidProvider   = errors.New("invalid environment provider")
	ErrProviderDuplicate = errors.New("duplicate environment provider registration")
)

// RouterConfig defines routing policies for the EnvironmentRouter.
type RouterConfig struct {
	DefaultMode EnvironmentMode
}

// EnvironmentRouter directs execution invocations to appropriate environment backends.
// It is thread-safe and safe for concurrent routing decisions.
type EnvironmentRouter struct {
	mu        sync.RWMutex
	config    RouterConfig
	providers map[string]map[EnvironmentMode]EnvironmentProvider
}

// NewEnvironmentRouter constructs a new EnvironmentRouter.
func NewEnvironmentRouter(config RouterConfig) *EnvironmentRouter {
	if config.DefaultMode == "" {
		config.DefaultMode = EnvModeTwin
	}
	return &EnvironmentRouter{
		config:    config,
		providers: make(map[string]map[EnvironmentMode]EnvironmentProvider),
	}
}

// RegisterProvider registers an EnvironmentProvider for its declared ServiceID and Mode.
// It validates fidelity and metadata, failing closed if invalid or duplicate.
func (r *EnvironmentRouter) RegisterProvider(p EnvironmentProvider) error {
	if p == nil {
		return fmt.Errorf("%w: nil provider", ErrInvalidProvider)
	}

	meta := p.Metadata()
	serviceID := strings.TrimSpace(meta.ServiceID)
	if serviceID == "" {
		return fmt.Errorf("%w: empty service_id", ErrInvalidProvider)
	}
	if strings.TrimSpace(meta.ProviderID) == "" {
		return fmt.Errorf("%w: empty provider_id", ErrInvalidProvider)
	}
	if !meta.Fidelity.Valid() {
		return fmt.Errorf("%w: invalid fidelity %q", ErrInvalidProvider, meta.Fidelity)
	}
	if !p.Mode().Valid() {
		return fmt.Errorf("%w: invalid mode %q", ErrInvalidProvider, p.Mode())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.providers[serviceID]; !ok {
		r.providers[serviceID] = make(map[EnvironmentMode]EnvironmentProvider)
	}
	if _, exists := r.providers[serviceID][p.Mode()]; exists {
		return fmt.Errorf("%w: service %q mode %s", ErrProviderDuplicate, serviceID, p.Mode())
	}

	r.providers[serviceID][p.Mode()] = p
	return nil
}

// Route directs an invocation to the provider matching serviceID and preferredMode.
// It fails closed if the requested service or mode is unavailable, preventing
// unintentional fallback or silent mock success.
func (r *EnvironmentRouter) Route(ctx context.Context, serviceID string, preferredMode EnvironmentMode) (EnvironmentProvider, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if preferredMode == "" {
		preferredMode = r.config.DefaultMode
	}
	if !preferredMode.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidMode, preferredMode)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	modes, ok := r.providers[serviceID]
	if !ok || len(modes) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrProviderNotFound, serviceID)
	}

	provider, hasMode := modes[preferredMode]
	if !hasMode {
		return nil, fmt.Errorf("%w: mode %s for service %q", ErrModeUnavailable, preferredMode, serviceID)
	}

	return provider, nil
}
