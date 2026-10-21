package fieldfeedback

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestCanonicalEventDetectorsAndCostOutlier(t *testing.T) {
	observer, taskID, attemptID, _, clk := newObserverHarness(t)
	ctx := context.Background()
	now := clk.Now().UTC()

	for i := 0; i < 2; i++ {
		if _, err := observer.store.DB().ExecContext(ctx,
			"INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at) VALUES (?, ?, ?, 'OPERATION_CANCELLED_BEFORE_DISPATCH', '{}', ?)",
			domain.NewID("event"), taskID, attemptID, now.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := observer.store.DB().ExecContext(ctx,
		"INSERT INTO execution_events(event_id, task_id, attempt_id, event_type, details_json, created_at) VALUES (?, ?, ?, 'HUMAN_INTERVENTION', '{}', ?)",
		domain.NewID("event"), taskID, attemptID, now.Add(3*time.Second).Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 6; i++ {
		outcomeTaskID := taskID
		if i > 0 {
			outcomeTaskID = domain.NewID("outlier-task")
			if _, err := observer.store.DB().ExecContext(ctx, `
				INSERT INTO tasks(task_id, purpose_kind, purpose_id, task_class, state, current_fence,
					acceptance_criteria_json, required_capabilities_json, required_enforcement,
					authority_ceiling_json, resource_envelope_id, created_at, updated_at)
				SELECT ?, purpose_kind, purpose_id, task_class, state, current_fence,
					acceptance_criteria_json, required_capabilities_json, required_enforcement,
					authority_ceiling_json, resource_envelope_id, created_at, updated_at
				FROM tasks WHERE task_id = ?`, outcomeTaskID, taskID); err != nil {
				t.Fatal(err)
			}
		}
		cost := int64(10)
		latency := int64(100)
		if i == 5 {
			cost = 100
			latency = 1000
		}
		if _, err := observer.store.DB().ExecContext(ctx, `
			INSERT INTO experience_outcomes(
				outcome_id, task_id, generic_task_class, scope_key, executor_kind,
				accepted, human_intervention, retry_count, cost_units, latency_ms, recorded_at
			) VALUES (?, ?, 'DEBUGGING', 'repo', 'test-executor', 1, 0, 0, ?, ?, ?)`,
			domain.NewID("outcome"), outcomeTaskID, cost, latency,
			now.Add(time.Duration(10+i)*time.Second).Format(time.RFC3339Nano),
		); err != nil {
			t.Fatal(err)
		}
	}

	observations, err := observer.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	categories := map[string]bool{}
	for _, observation := range observations {
		categories[observation.Category] = true
	}
	for _, want := range []string{"POLICY_AUTHORITY_FRICTION", "HUMAN_INTERVENTION", "COST_LATENCY_OUTLIER"} {
		if !categories[want] {
			t.Fatalf("missing detector category %s in %v", want, categories)
		}
	}
}

func TestDetectSkipsMalformedRecordedAt(t *testing.T) {
	observer, taskID, _, _, clk := newObserverHarness(t)
	ctx := context.Background()
	db := observer.store.DB()
	// Control row (valid) plus two malformed rows, each on its own task.
	base := clk.Now().UTC()
	recorded := []string{"2026-10-19 14:41:15", "2026-10-19 14:41:16"}
	for i := 0; i < 6; i++ { // valid control rows: 5 baseline + 1 outlier
		recorded = append(recorded, base.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano))
	}
	for i, ts := range recorded {
		outcomeTask := taskID
		if i > 0 {
			outcomeTask = domain.NewID("cloned-task")
			if _, err := db.ExecContext(ctx, `
				INSERT INTO tasks(task_id, purpose_kind, purpose_id, task_class, state, current_fence,
					acceptance_criteria_json, required_capabilities_json, required_enforcement,
					authority_ceiling_json, resource_envelope_id, created_at, updated_at)
				SELECT ?, purpose_kind, purpose_id, task_class, state, current_fence,
					acceptance_criteria_json, required_capabilities_json, required_enforcement,
					authority_ceiling_json, resource_envelope_id, created_at, updated_at
				FROM tasks WHERE task_id = ?`, outcomeTask, taskID); err != nil {
				t.Fatal(err)
			}
		}
		id := domain.NewID("outcome")
		cost := 10
		if i == len(recorded)-1 {
			cost = 1000
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO experience_outcomes(
				outcome_id, task_id, generic_task_class, scope_key, executor_kind,
				accepted, human_intervention, retry_count, cost_units, latency_ms, recorded_at
			) VALUES (?, ?, 'DEBUGGING', 'repo', 'test-executor', 1, 0, 0, ?, 1, ?)`,
			id, outcomeTask, cost, ts); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
	}
	observations, err := observer.Detect(ctx, DetectorInput{})
	if err != nil {
		t.Fatalf("Detect aborted on malformed row: %v", err)
	}
	malformedCount, outliers := 0, 0
	for _, o := range observations {
		switch o.Category {
		case "MALFORMED_CANONICAL_ROW":
			malformedCount++
			if !strings.Contains(o.MetricsJSON, `"malformed_rows":2`) {
				t.Fatalf("metrics = %s, want malformed_rows 2", o.MetricsJSON)
			}
		case "COST_LATENCY_OUTLIER":
			outliers++
		}
	}
	if malformedCount != 1 || outliers != 1 {
		t.Fatalf("malformed=%d outliers=%d, want 1 and 1 (valid rows still evaluated)", malformedCount, outliers)
	}
	if _, ok := safeExportCategories["MALFORMED_CANONICAL_ROW"]; !ok {
		t.Fatal("category missing from export allowlist")
	}
}
