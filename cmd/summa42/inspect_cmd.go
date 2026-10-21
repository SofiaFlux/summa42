package main

import (
	"encoding/json"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/spf13/cobra"
)

func newInspectCommand(api control.API, jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "inspect", Short: "Inspect execution state"}
	command.AddCommand(newInspectAttemptCommand(api, jsonOutput), newInspectOperationCommand(api, jsonOutput))
	return command
}

func newInspectAttemptCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "attempt <attempt-id>",
		Short: "Inspect an Attempt",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			attempt, err := api.Attempt(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(attempt)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Attempt %s for Task %s: %s (lease %s)\n", attempt.ID, attempt.TaskID, attempt.State, attempt.LeaseState)
			return err
		},
	}
}

func newInspectOperationCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:   "operation <operation-id>",
		Short: "Inspect an External Operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operation, err := api.Operation(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(operation)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Operation %s for Task %s: %s\n", operation.ID, operation.TaskID, operation.State)
			if err != nil {
				return err
			}
			if len(operation.CanonicalIntent) > 0 {
				pretty, _ := json.MarshalIndent(operation.CanonicalIntent, "", "  ")
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\nIntent: %s\n", operation.Provider, pretty)
			}
			return err
		},
	}
}
