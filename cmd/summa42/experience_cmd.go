package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/spf13/cobra"
)

func newExperienceCommand(api control.API, jsonOutput *bool) *cobra.Command {
	root := &cobra.Command{Use: "experience", Short: "Inspect and authorize learned local experience"}
	root.AddCommand(
		newExperienceListCommand(api, jsonOutput),
		newExperienceInspectCommand(api, jsonOutput),
		newExperienceGrantCommand(api, jsonOutput),
		newExperienceProposeCommand(api, jsonOutput),
		newExperienceObserveOutcomeCommand(api, jsonOutput),
		newExperienceEvaluateCommand(api, jsonOutput),
	)
	return root
}

func newExperienceListCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List experience rules",
		RunE: func(cmd *cobra.Command, args []string) error {
			items, err := api.ExperienceRules(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
			}
			if len(items) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "No experience rules.")
				return err
			}
			for _, item := range items {
				if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\tsamples=%d\n", item.ID, item.State, item.ScopeKey, item.PreferredExecutor, item.VerifiedSamples); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newExperienceInspectCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "inspect <rule-id>", Short: "Inspect one experience rule", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			item, err := api.ExperienceRule(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(item)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Rule %s\nState: %s\nScope: %s\nPreferred executor: %s\nVerified samples: %d\n", item.ID, item.State, item.ScopeKey, item.PreferredExecutor, item.VerifiedSamples)
			return err
		},
	}
}

func newExperienceGrantCommand(api control.API, jsonOutput *bool) *cobra.Command {
	root := &cobra.Command{Use: "grant", Short: "Request or activate an AdaptationGrant"}
	root.AddCommand(newExperienceGrantRequestCommand(api, jsonOutput), newExperienceGrantActivateCommand(api, jsonOutput))
	return root
}

func newExperienceGrantRequestCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var scope string
	var executors []string
	var minSamples, acceptanceBps, costBps int
	var expiresIn time.Duration
	command := &cobra.Command{
		Use: "request", Short: "Request an Owner-authorized executor-preference grant",
		RunE: func(cmd *cobra.Command, args []string) error {
			scope = strings.TrimSpace(scope)
			if scope == "" || len(executors) == 0 {
				return fmt.Errorf("scope and at least one executor are required")
			}
			result, err := api.ExperienceGrantRequest(cmd.Context(), control.ExperienceGrantCreateRequest{
				ScopeKey: scope, AllowedExecutors: executors, MinVerifiedSamples: minSamples,
				MaxAcceptanceRegressionBps: acceptanceBps, MaxCostRegressionBps: costBps,
				ExpiresAt: time.Now().UTC().Add(expiresIn),
			})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Grant request %s created. Approval %s must be signed before activation.\n", result.RequestID, result.ApprovalID)
			return err
		},
	}
	command.Flags().StringVar(&scope, "scope", "", "local TaskClass/scope key")
	command.Flags().StringSliceVar(&executors, "executor", nil, "allowed executor kind; repeat or comma-separate")
	command.Flags().IntVar(&minSamples, "min-samples", 3, "minimum verified samples")
	command.Flags().IntVar(&acceptanceBps, "max-acceptance-regression-bps", 0, "maximum acceptance regression in basis points")
	command.Flags().IntVar(&costBps, "max-cost-regression-bps", 0, "maximum cost regression in basis points")
	command.Flags().DurationVar(&expiresIn, "expires-in", 7*24*time.Hour, "grant lifetime")
	return command
}

func newExperienceGrantActivateCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "activate <grant-request-id>", Short: "Activate an already Owner-approved AdaptationGrant", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := api.ExperienceGrantActivate(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "AdaptationGrant %s ACTIVE for scope %s.\n", result.ID, result.ScopeKey)
			return err
		},
	}
}

func newExperienceProposeCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var grantID, executor string
	var evidence []string
	command := &cobra.Command{
		Use: "propose", Short: "Propose an executor preference under an active AdaptationGrant",
		RunE: func(cmd *cobra.Command, args []string) error {
			grantID = strings.TrimSpace(grantID)
			executor = strings.TrimSpace(executor)
			ids := make([]domain.ID, 0, len(evidence))
			for _, raw := range evidence {
				raw = strings.TrimSpace(raw)
				if raw != "" {
					ids = append(ids, domain.ID(raw))
				}
			}
			if grantID == "" || executor == "" || len(ids) == 0 {
				return fmt.Errorf("grant, executor, and at least one evidence observation are required")
			}
			result, err := api.ExperienceProposalCreate(cmd.Context(), control.ExperienceProposalCreateRequest{
				GrantID: domain.ID(grantID), PreferredExecutor: executor, EvidenceObservationIDs: ids,
			})
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Experience proposal %s created for canonical scope %s (%s).\n", result.ID, result.ScopeKey, result.GenericTaskClass)
			return err
		},
	}
	command.Flags().StringVar(&grantID, "grant", "", "active AdaptationGrant id")
	command.Flags().StringVar(&executor, "executor", "", "preferred executor kind")
	command.Flags().StringSliceVar(&evidence, "evidence-observation", nil, "supporting field observation id; repeat or comma-separate")
	return command
}

func newExperienceObserveOutcomeCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var accepted bool
	command := &cobra.Command{
		Use: "observe-outcome <task-id>", Short: "Record a canonical verified Task outcome for local learning", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := control.ExperienceOutcomeCreateRequest{
				TaskID: domain.ID(strings.TrimSpace(args[0])), Accepted: accepted,
			}
			result, err := api.ExperienceOutcomeCreate(cmd.Context(), request)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Verified outcome recorded for Task %s. Scope, class, and executor were derived from canonical state.\n", result.TaskID)
			return err
		},
	}
	command.Flags().BoolVar(&accepted, "accepted", false, "record canonical accepted success; omit for challenged negative outcome")
	return command
}

func newExperienceEvaluateCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use: "evaluate <proposal-id>", Short: "Evaluate a proposal against canonical verified outcomes", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := api.ExperienceEvaluate(cmd.Context(), domain.ID(strings.TrimSpace(args[0])))
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Rule %s is %s for scope %s; verified samples=%d.\n", result.ID, result.State, result.ScopeKey, result.VerifiedSamples)
			return err
		},
	}
}
