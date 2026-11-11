package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"github.com/spf13/cobra"
)

type caseRow struct {
	CaseID               string `json:"case_id"`
	Source               string `json:"source"`
	Object               string `json:"object"`
	Revision             string `json:"revision"`
	State                string `json:"state"`
	Phase                string `json:"phase,omitempty"`
	CurrentWorkID        string `json:"current_work_id"`
	CurrentWorkKind      string `json:"current_work_kind"`
	CompletedSteps       int    `json:"completed_steps"`
	MaxSteps             int    `json:"max_steps"`
	LastAssessmentReason string `json:"last_assessment_reason"`
	UpdatedAt            string `json:"updated_at"`
}

// newCasesCommand lists workflow cases. It only reads, and like the emergency
// commands it reads the store directly, so it works while only run-* workers
// are running and no control daemon is.
func newCasesCommand(jsonOutput *bool) *cobra.Command {
	var home, stateFilter string
	command := &cobra.Command{
		Use:   "cases",
		Short: "List workflow cases: state, current work, steps and the last assessment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			wanted, err := parseCaseState(stateFilter)
			if err != nil {
				return err
			}
			rows, err := listCases(cmd.Context(), home, wanted)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if *jsonOutput {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(out, "No cases.")
				return nil
			}
			table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(table, "CASE\tSOURCE\tOBJECT\tSTATE\tCURRENT WORK\tSTEPS\tUPDATED\tLAST ASSESSMENT")
			for _, row := range rows {
				work := "-"
				if row.CurrentWorkID != "" {
					work = row.CurrentWorkKind + " " + row.CurrentWorkID
				}
				reason := row.LastAssessmentReason
				if reason == "" {
					reason = "-"
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%s\t%s\n",
					row.CaseID, row.Source, row.Object, row.State, work, row.CompletedSteps, row.MaxSteps, row.UpdatedAt, oneLine(reason, 160))
			}
			return table.Flush()
		},
	}
	command.Flags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	command.Flags().StringVar(&stateFilter, "state", "", "only cases in this state: ACTIVE, BLOCKED, READY_FOR_VERIFICATION or CLOSED")
	return command
}

func parseCaseState(text string) (workflowcase.State, error) {
	wanted := workflowcase.State(strings.ToUpper(strings.TrimSpace(text)))
	switch wanted {
	case "", workflowcase.Active, workflowcase.Blocked, workflowcase.ReadyForVerification, workflowcase.Closed:
		return wanted, nil
	}
	return "", fmt.Errorf("unknown case state %q: use ACTIVE, BLOCKED, READY_FOR_VERIFICATION or CLOSED", text)
}

func listCases(ctx context.Context, home string, wanted workflowcase.State) ([]caseRow, error) {
	resolvedHome, err := localconfig.ResolveHome(home)
	if err != nil {
		return nil, err
	}
	cfg, err := localconfig.Load(resolvedHome)
	if err != nil {
		return nil, fmt.Errorf("load local config: %w", err)
	}
	store, err := state.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	defer store.DB().Close()
	cases := workflowcase.New(store, clock.System{}, purpose.New(store, clock.System{}))
	summaries, err := cases.ListSummaries(ctx, wanted)
	if err != nil {
		return nil, err
	}
	rows := make([]caseRow, 0, len(summaries))
	for _, summary := range summaries {
		c := summary.Case
		row := caseRow{
			CaseID: string(c.ID), Source: c.Source, Object: c.ObjectID, Revision: c.RevisionID,
			State: string(c.State), Phase: string(c.Phase), CurrentWorkID: string(c.CurrentWorkID),
			CompletedSteps: c.CompletedSteps, MaxSteps: c.MaxSteps,
			LastAssessmentReason: summary.LastAssessmentReason,
			UpdatedAt:            summary.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
		if c.CurrentWorkID != "" {
			row.CurrentWorkKind = c.NextWork.Kind
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// oneLine keeps a table cell on one line and within a width.
func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}
