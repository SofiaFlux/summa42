package scheduler

import (
	"context"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
)

// TaskClassRouting maps a task class to the executor kind that must handle it.
// It exists because ChooseExecutor does not inspect the task class: without an
// explicit mapping the alphabetical baseline would hand a triage Task to
// whichever registered kind happens to sort first.
type TaskClassRouting map[string]string

func (r TaskClassRouting) PreferredExecutor(ctx context.Context, task domain.Task, eligible []string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	kind, mapped := r[strings.TrimSpace(task.TaskClass)]
	if !mapped {
		return "", false, nil
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", false, fmt.Errorf("task class %q has a blank required executor", task.TaskClass)
	}
	for _, candidate := range eligible {
		if strings.TrimSpace(candidate) == kind {
			return kind, true, nil
		}
	}
	return "", false, fmt.Errorf("task class %q requires unavailable executor %q", task.TaskClass, kind)
}
