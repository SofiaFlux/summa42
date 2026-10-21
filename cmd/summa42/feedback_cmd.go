package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/spf13/cobra"
)

func newFeedbackCommand(api control.API, jsonOutput *bool) *cobra.Command {
	root := &cobra.Command{Use: "feedback", Short: "Inspect and manage local field feedback"}
	root.AddCommand(
		newFeedbackListCommand(api, jsonOutput),
		newFeedbackCandidateCommand(api, jsonOutput),
		newFeedbackSanitizeCommand(api, jsonOutput),
		newFeedbackInspectCommand(api, jsonOutput),
		newFeedbackEmitCommand(api, jsonOutput),
		newFeedbackObserveCommand(api, jsonOutput),
		newFeedbackScanCommand(api, jsonOutput),
	)
	return root
}

func newFeedbackListCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List local feedback candidates",
		RunE: func(cmd *cobra.Command, args []string) error {
			items, err := api.Feedback(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
			}
			if len(items) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No feedback candidates.")
				return err
			}
			for _, item := range items {
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", item.ID, item.State, item.GenericTaskClass, item.Category); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newFeedbackInspectCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "inspect <candidate-id>", Short: "Inspect local candidate and sanitized export artifact", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := api.FeedbackInspect(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			if result.LocalCandidate != nil {
				if _, err = fmt.Fprintln(cmd.OutOrStdout(), "LOCAL — DO NOT EXPORT"); err != nil {
					return err
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nState: %s\nCategory: %s\nExpected: %s\nObserved: %s\n",
					result.LocalCandidate.ID, result.LocalCandidate.State, result.LocalCandidate.Category,
					result.LocalCandidate.ExpectedBehavior, result.LocalCandidate.ObservedBehavior); err != nil {
					return err
				}
			}
			if result.ExportArtifact != nil {
				if _, err = fmt.Fprintln(cmd.OutOrStdout(), "\nSANITIZED EXPORT ARTIFACT"); err != nil {
					return err
				}
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "ID: %s\nFingerprint: %s\nContent: %s\n",
					result.ExportArtifact.ID, result.ExportArtifact.Fingerprint, result.ExportArtifact.ContentJSON); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newFeedbackEmitCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "emit <candidate-id>", Short: "Schedule sanitized feedback as governed work", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := api.FeedbackEmit(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Scheduled governed work %s for sanitized artifact %s. No issue was sent directly.\n", result.TaskID, result.ArtifactID)
			return err
		},
	}
}

func newFeedbackObserveCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var category, summary, taskID, attemptID, operationID, enforcement string
	command := &cobra.Command{
		Use: "observe", Short: "Record a local-only operator observation",
		RunE: func(cmd *cobra.Command, args []string) error {
			category = strings.TrimSpace(category)
			summary = strings.TrimSpace(summary)
			if category == "" || summary == "" {
				return errors.New("category and summary are required")
			}
			level := domain.EnforcementLevel(strings.TrimSpace(enforcement))
			if level == "" {
				level = domain.EnforcementEnforced
			}
			result, err := api.FeedbackObserve(cmd.Context(), control.FeedbackObserveRequest{
				TaskID: domain.ID(strings.TrimSpace(taskID)), AttemptID: domain.ID(strings.TrimSpace(attemptID)),
				OperationID: domain.ID(strings.TrimSpace(operationID)), Category: category, SummaryLocal: summary, Enforcement: level,
			})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "LOCAL — DO NOT EXPORT\nObservation %s (%s) recorded locally.\n", result.ID, result.Category)
			return err
		},
	}
	command.Flags().StringVar(&category, "category", "", "privacy-safe categorical observation type")
	command.Flags().StringVar(&summary, "summary", "", "local-sensitive observation summary")
	command.Flags().StringVar(&taskID, "task", "", "related task id")
	command.Flags().StringVar(&attemptID, "attempt", "", "related attempt id")
	command.Flags().StringVar(&operationID, "operation", "", "related operation id")
	command.Flags().StringVar(&enforcement, "enforcement", string(domain.EnforcementEnforced), "enforcement level")
	return command
}

func newFeedbackScanCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "scan", Short: "Scan canonical runtime state for deterministic observations",
		RunE: func(cmd *cobra.Command, args []string) error {
			items, err := api.FeedbackScan(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
			}
			if len(items) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No new or existing detector observations.")
				return err
			}
			for _, item := range items {
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.ID, item.Category); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newFeedbackCandidateCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var observations []string
	var genericClass, category, expected, observed, enforcement string
	var transitions []string
	var human bool
	command := &cobra.Command{
		Use: "candidate", Short: "Create a local feedback candidate from observations",
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := make([]domain.ID, 0, len(observations))
			for _, raw := range observations {
				raw = strings.TrimSpace(raw)
				if raw != "" {
					ids = append(ids, domain.ID(raw))
				}
			}
			if len(ids) == 0 || strings.TrimSpace(genericClass) == "" || strings.TrimSpace(category) == "" || strings.TrimSpace(expected) == "" || strings.TrimSpace(observed) == "" {
				return errors.New("observation, generic-task-class, category, expected, and observed are required")
			}
			result, err := api.FeedbackCandidateCreate(cmd.Context(), control.FeedbackCandidateCreateRequest{
				ObservationIDs: ids, GenericTaskClass: domain.GenericTaskClass(strings.TrimSpace(genericClass)),
				Category: strings.TrimSpace(category), ExpectedBehavior: expected, ObservedBehavior: observed,
				StateTransitions: append([]string(nil), transitions...), HumanIntervention: human,
				Enforcement: domain.EnforcementLevel(strings.TrimSpace(enforcement)),
			})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "LOCAL — DO NOT EXPORT\nCandidate %s created from %d observation(s).\n", result.ID, len(ids))
			return err
		},
	}
	command.Flags().StringSliceVar(&observations, "observation", nil, "observation id; repeat or comma-separate")
	command.Flags().StringVar(&genericClass, "generic-task-class", "", "privacy-safe generic task class")
	command.Flags().StringVar(&category, "category", "", "privacy-safe categorical feedback type")
	command.Flags().StringVar(&expected, "expected", "", "local expected behavior")
	command.Flags().StringVar(&observed, "observed", "", "local observed behavior")
	command.Flags().StringSliceVar(&transitions, "transition", nil, "privacy-safe state transition token")
	command.Flags().BoolVar(&human, "human-intervention", false, "human intervention was required")
	command.Flags().StringVar(&enforcement, "enforcement", string(domain.EnforcementEnforced), "enforcement level")
	return command
}

func newFeedbackSanitizeCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "sanitize <candidate-id>", Short: "Run the outbound privacy gate for a local candidate", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := api.FeedbackSanitize(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			if result.Artifact == nil {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Candidate %s sanitization: %s (%s)\n", result.CandidateID, result.Outcome, strings.Join(result.ReasonCodes, ","))
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Candidate %s sanitization: PASS. Immutable artifact %s created; follow-up emission remains governed work.\n", result.CandidateID, result.Artifact.ID)
			return err
		},
	}
}
