package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/SofiaFlux/summa42/internal/casereport"
	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
	"github.com/spf13/cobra"
)

func newCaseCommand(jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "case", Short: "Inspect one workflow case"}
	command.AddCommand(newCaseReportCommand(jsonOutput))
	return command
}

// newCaseReportCommand prints what one case cost: stages, model calls and
// tokens, wall time and Owner interventions. It only reads the local database.
func newCaseReportCommand(jsonOutput *bool) *cobra.Command {
	var home string
	command := &cobra.Command{
		Use:   "report <case-id>",
		Short: "Report one case: stages, evidence, model calls and tokens, wall time, Owner interventions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedHome, err := localconfig.ResolveHome(home)
			if err != nil {
				return err
			}
			cfg, err := localconfig.Load(resolvedHome)
			if err != nil {
				return fmt.Errorf("load local config: %w", err)
			}
			store, err := state.Open(cmd.Context(), cfg.DatabasePath)
			if err != nil {
				return err
			}
			defer store.DB().Close()
			cases := workflowcase.New(store, clock.System{}, purpose.New(store, clock.System{}))
			report, err := casereport.Build(cmd.Context(), store, cases, domain.ID(strings.TrimSpace(args[0])))
			if err != nil {
				return err
			}
			if *jsonOutput {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(report)
			}
			return writeCaseReport(cmd.OutOrStdout(), report)
		},
	}
	command.Flags().StringVar(&home, "home", "", "Collective home directory (default ~/.summa42)")
	return command
}

func writeCaseReport(out io.Writer, report casereport.Report) error {
	seconds := func(value float64) string {
		return (time.Duration(value*1000) * time.Millisecond).Round(time.Second).String()
	}
	fmt.Fprintf(out, "Case %s\n", report.CaseID)
	fmt.Fprintf(out, "  %s %s (revision %s)\n", report.Source, report.Object, report.Revision)
	fmt.Fprintf(out, "  state %s, steps %d/%d, wall time %s (created %s, updated %s)\n\n", report.State, report.CompletedSteps, report.MaxSteps,
		seconds(report.WallSeconds), report.CreatedAt.UTC().Format(time.RFC3339), report.UpdatedAt.UTC().Format(time.RFC3339))

	fmt.Fprintf(out, "Stages (%d)\n", len(report.Stages))
	for n, stage := range report.Stages {
		fmt.Fprintf(out, "  %d. %s -> %s after %s, work %s\n     %s\n", n+1, stage.Verdict, stage.Outcome, seconds(stage.Seconds), stage.WorkID, oneLine(stage.Reason, 300))
		for _, item := range stage.Evidence {
			fmt.Fprintf(out, "     evidence %s  %s  %s\n", item.ID, item.Kind, item.ContentHash)
		}
	}

	fmt.Fprintf(out, "\nModel calls (%d)\n", report.ModelTotal.Calls)
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "  STAGE\tCALLS\tOK\tFAILED\tRETRIES\tTIME\tINPUT TOKENS\tOUTPUT TOKENS\tNO TOKEN REPORT\tMODELS")
	row := func(name string, usage casereport.ModelUsage) {
		fmt.Fprintf(table, "  %s\t%d\t%d\t%d\t%d\t%s\t%d\t%d\t%d\t%s\n", name, usage.Calls, usage.OK, usage.Failed, usage.Retries,
			seconds(usage.DurationSeconds), usage.InputTokens, usage.OutputTokens, usage.CallsWithoutTokens, strings.Join(usage.Models, ","))
	}
	for _, usage := range report.ModelCalls {
		row(usage.Stage, usage)
	}
	row("total", report.ModelTotal)
	if err := table.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(out, "\nOwner interventions (%d)\n", report.Interventions.Total)
	fmt.Fprintf(out, "  approvals: %d\n  rejections: %d\n", report.Interventions.Approvals, report.Interventions.Rejections)
	kinds := make([]string, 0, len(report.Interventions.OwnerEvidence))
	for kind := range report.Interventions.OwnerEvidence {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(out, "  %s: %d\n", kind, report.Interventions.OwnerEvidence[kind])
	}

	fmt.Fprintln(out, "\nNotes")
	for _, note := range report.Notes {
		fmt.Fprintf(out, "  - %s\n", note)
	}
	return nil
}
