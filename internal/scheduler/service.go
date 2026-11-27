package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type CapabilityCapacity struct {
	Accessible  bool
	Enforcement domain.EnforcementLevel
}

type CapacitySnapshot struct {
	Capabilities map[string]CapabilityCapacity
	Executors    map[string]ExecutorCapacity
}

type TaskCandidate struct {
	Task              domain.Task
	AvailableBudget   int64
	DependencyUnlocks int
}

type ExecutorPreference interface {
	PreferredExecutor(context.Context, domain.Task, []string) (string, bool, error)
}

type Service struct {
	store         *state.Store
	clock         clock.Clock
	purpose       *purpose.Service
	execution     *execution.Service
	resources     *resources.Service
	leaseDuration time.Duration
	preferences   []ExecutorPreference

	mu          sync.Mutex
	lastProject domain.ID
}

func New(store *state.Store, clk clock.Clock, purposes *purpose.Service, executionSvc *execution.Service, resourceSvc *resources.Service, leaseDuration time.Duration, preferences ...ExecutorPreference) *Service {
	return &Service{
		store: store, clock: clk, purpose: purposes, execution: executionSvc,
		resources: resourceSvc, leaseDuration: leaseDuration, preferences: preferences,
	}
}

func (s *Service) Next(ctx context.Context, capacity CapacitySnapshot) (*TaskCandidate, error) {
	if err := s.configured(); err != nil {
		return nil, err
	}
	tasks, err := s.loadEligibleTasks(ctx)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now().UTC()
	candidates := make([]TaskCandidate, 0, len(tasks))
	for _, task := range tasks {
		if !task.EarliestStart.IsZero() && task.EarliestStart.After(now) {
			continue
		}
		if !task.Deadline.IsZero() && now.After(task.Deadline) {
			if _, err := s.execution.ExpireEligibleTask(ctx, task.ID); err != nil {
				return nil, err
			}
			continue
		}
		if err := s.purpose.ValidatePurpose(ctx, task.Purpose); err != nil {
			if errors.Is(err, domain.ErrInvalidPurpose) {
				continue
			}
			return nil, err
		}
		ready, err := s.dependenciesReady(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		if !ready || !capacityEligible(task, capacity) || !authorityEligible(task) {
			continue
		}
		if capacity.Executors != nil && len(eligibleKinds(task, capacity)) == 0 {
			continue
		}
		available, err := s.resources.Available(ctx, task.ResourceEnvelopeID)
		if err != nil {
			return nil, err
		}
		if available <= 0 {
			continue
		}
		unlocks, err := s.dependencyUnlockCount(ctx, task.ID)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, TaskCandidate{Task: task, AvailableBudget: available, DependencyUnlocks: unlocks})
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidateBefore(candidates[i], candidates[j]) })
	if idx := slices.IndexFunc(candidates, func(c TaskCandidate) bool {
		return c.Task.Purpose.Kind == domain.PurposeObligation
	}); idx >= 0 {
		return &candidates[idx], nil
	}
	chosen := s.selectRoundRobinCandidate(candidates)
	return &chosen, nil
}

func (s *Service) selectRoundRobinCandidate(candidates []TaskCandidate) TaskCandidate {
	seen := make(map[domain.ID]struct{}, len(candidates))
	projects := make([]domain.ID, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.Task.ProjectID]; !ok {
			seen[c.Task.ProjectID] = struct{}{}
			projects = append(projects, c.Task.ProjectID)
		}
	}
	if len(projects) <= 1 {
		s.mu.Lock()
		s.lastProject = candidates[0].Task.ProjectID
		s.mu.Unlock()
		return candidates[0]
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i] < projects[j] })

	s.mu.Lock()
	pick := projects[0]
	for _, p := range projects {
		if p > s.lastProject {
			pick = p
			break
		}
	}
	s.lastProject = pick
	s.mu.Unlock()

	for _, c := range candidates {
		if c.Task.ProjectID == pick {
			return c
		}
	}
	return candidates[0]
}

func (s *Service) ChooseExecutor(ctx context.Context, task domain.Task, eligibleExecutorKinds []string) (string, error) {
	if err := s.configured(); err != nil {
		return "", err
	}
	set := make(map[string]struct{}, len(eligibleExecutorKinds))
	eligible := make([]string, 0, len(eligibleExecutorKinds))
	for _, kind := range eligibleExecutorKinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		if _, exists := set[kind]; exists {
			continue
		}
		set[kind] = struct{}{}
		eligible = append(eligible, kind)
	}
	if len(eligible) == 0 {
		return "", errors.New("no eligible executor kinds")
	}
	sort.Strings(eligible)
	baseline := eligible[0]
	for _, preference := range s.preferences {
		if preference == nil {
			continue
		}
		preferred, found, err := preference.PreferredExecutor(ctx, task, append([]string(nil), eligible...))
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		preferred = strings.TrimSpace(preferred)
		if _, ok := set[preferred]; !ok {
			return "", fmt.Errorf("executor preference returned ineligible executor %q", preferred)
		}
		return preferred, nil
	}
	return baseline, nil
}

func (s *Service) Lease(ctx context.Context, taskID domain.ID, executorKind string) (domain.Attempt, error) {
	if err := s.configured(); err != nil {
		return domain.Attempt{}, err
	}
	return s.execution.StartAttempt(ctx, taskID, executorKind, s.leaseDuration)
}

func (s *Service) loadEligibleTasks(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.store.DB().QueryContext(ctx, `
		SELECT t.task_id, COALESCE(t.parent_task_id, ''),
		       COALESCE(NULLIF(t.project_id, ''), NULLIF(e.project_id, ''), ''),
		       t.purpose_kind, t.purpose_id, t.task_class,
		       t.objective, t.payload_json, COALESCE(t.idempotency_key, ''), t.state,
		       COALESCE(t.current_attempt_id, ''), t.current_fence, t.acceptance_criteria_json,
		       t.required_capabilities_json, t.required_enforcement, t.authority_ceiling_json,
		       t.resource_envelope_id, t.priority, t.earliest_start, t.deadline, t.created_at, t.updated_at
		FROM tasks t
		LEFT JOIN resource_envelopes e ON e.envelope_id = t.resource_envelope_id
		LEFT JOIN projects p ON p.project_id = COALESCE(NULLIF(t.project_id, ''), NULLIF(e.project_id, ''))
		WHERE t.state = ?
		  AND (COALESCE(NULLIF(t.project_id, ''), NULLIF(e.project_id, ''), '') = '' OR p.state = 'ACTIVE')`, domain.TaskEligible)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var task domain.Task
		var acceptanceJSON, capabilitiesJSON, authorityJSON, payloadJSON string
		var earliest, deadline sql.NullString
		var createdAt, updatedAt string
		if err := rows.Scan(
			&task.ID, &task.ParentTaskID, &task.ProjectID, &task.Purpose.Kind, &task.Purpose.ID, &task.TaskClass,
			&task.Objective, &payloadJSON, &task.IdempotencyKey, &task.State,
			&task.CurrentAttemptID, &task.CurrentFence, &acceptanceJSON, &capabilitiesJSON,
			&task.RequiredEnforcement, &authorityJSON, &task.ResourceEnvelopeID, &task.Priority,
			&earliest, &deadline, &createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		task.PayloadJSON = json.RawMessage(payloadJSON)
		if err := json.Unmarshal([]byte(acceptanceJSON), &task.AcceptanceCriteria); err != nil {
			return nil, fmt.Errorf("decode acceptance criteria for %s: %w", task.ID, err)
		}
		if err := json.Unmarshal([]byte(capabilitiesJSON), &task.RequiredCapabilities); err != nil {
			return nil, fmt.Errorf("decode required capabilities for %s: %w", task.ID, err)
		}
		if err := json.Unmarshal([]byte(authorityJSON), &task.AuthorityCeiling); err != nil {
			return nil, fmt.Errorf("decode authority ceiling for %s: %w", task.ID, err)
		}
		var err error
		if task.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parse created_at for %s: %w", task.ID, err)
		}
		if task.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, fmt.Errorf("parse updated_at for %s: %w", task.ID, err)
		}
		if earliest.Valid {
			if task.EarliestStart, err = parseTime(earliest.String); err != nil {
				return nil, fmt.Errorf("parse earliest_start for %s: %w", task.ID, err)
			}
		}
		if deadline.Valid {
			if task.Deadline, err = parseTime(deadline.String); err != nil {
				return nil, fmt.Errorf("parse deadline for %s: %w", task.ID, err)
			}
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *Service) dependenciesReady(ctx context.Context, taskID domain.ID) (bool, error) {
	var unmet int
	if err := s.store.DB().QueryRowContext(ctx, `
		SELECT count(*)
		FROM task_dependencies d
		JOIN tasks dependency ON dependency.task_id = d.depends_on_task_id
		WHERE d.task_id = ? AND dependency.state <> ?`, taskID, domain.TaskSucceeded,
	).Scan(&unmet); err != nil {
		return false, err
	}
	return unmet == 0, nil
}

func (s *Service) dependencyUnlockCount(ctx context.Context, taskID domain.ID) (int, error) {
	var count int
	if err := s.store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM task_dependencies WHERE depends_on_task_id = ?`, taskID,
	).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func capacityEligible(task domain.Task, capacity CapacitySnapshot) bool {
	for _, capability := range task.RequiredCapabilities {
		available, ok := capacity.Capabilities[capability]
		if !ok || !available.Accessible || enforcementRank(available.Enforcement) < enforcementRank(task.RequiredEnforcement) {
			return false
		}
	}
	return true
}

func authorityEligible(task domain.Task) bool {
	authorized := make(map[string]struct{}, len(task.AuthorityCeiling))
	for _, capability := range task.AuthorityCeiling {
		authorized[capability] = struct{}{}
	}
	for _, capability := range task.RequiredCapabilities {
		if _, ok := authorized[capability]; !ok {
			return false
		}
	}
	return true
}

func candidateBefore(a, b TaskCandidate) bool {
	// Compliance Obligations strictly precede discretionary goals (Anti-starvation / non-preemption guarantee)
	aIsObligation := a.Task.Purpose.Kind == domain.PurposeObligation
	bIsObligation := b.Task.Purpose.Kind == domain.PurposeObligation
	if aIsObligation != bIsObligation {
		return aIsObligation
	}

	aHasDeadline := !a.Task.Deadline.IsZero()
	bHasDeadline := !b.Task.Deadline.IsZero()
	if aHasDeadline != bHasDeadline {
		return aHasDeadline
	}
	if aHasDeadline && !a.Task.Deadline.Equal(b.Task.Deadline) {
		return a.Task.Deadline.Before(b.Task.Deadline)
	}
	if a.DependencyUnlocks != b.DependencyUnlocks {
		return a.DependencyUnlocks > b.DependencyUnlocks
	}
	if a.Task.Priority != b.Task.Priority {
		return a.Task.Priority > b.Task.Priority
	}
	if a.AvailableBudget != b.AvailableBudget {
		return a.AvailableBudget > b.AvailableBudget
	}
	if !a.Task.CreatedAt.Equal(b.Task.CreatedAt) {
		return a.Task.CreatedAt.Before(b.Task.CreatedAt)
	}
	return a.Task.ID < b.Task.ID
}

func enforcementRank(level domain.EnforcementLevel) int {
	switch level {
	case domain.EnforcementUnenforced:
		return 1
	case domain.EnforcementPartial:
		return 2
	case domain.EnforcementEnforced:
		return 3
	default:
		return 0
	}
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func (s *Service) configured() error {
	if s == nil || s.store == nil || s.clock == nil || s.purpose == nil || s.execution == nil || s.resources == nil || s.leaseDuration <= 0 {
		return errors.New("scheduler service is not configured")
	}
	return nil
}
