package main

import (
	"os"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	endpoint := os.Getenv("SUMMA42_CONTROL_ENDPOINT")
	token := os.Getenv("SUMMA42_CONTROL_TOKEN")
	if token == "" {
		if cfg, err := localconfig.Load(""); err == nil {
			token = cfg.ControlToken
		}
	}
	return newRootCommandWithClient(control.NewLocalClient(endpoint, token))
}

func newRootCommandWithClient(api control.API) *cobra.Command {
	command := &cobra.Command{
		Use:   "summa42",
		Short: "Control a Summa42",
	}
	var jsonOutput bool
	command.PersistentFlags().BoolVar(&jsonOutput, "json", false, "emit stable JSON DTO output")
	command.AddCommand(
		NewInitCommand(),
		newPolicyCommand(),
		newStatusCommand(api, &jsonOutput),
		newMissionCommand(api, &jsonOutput),
		newTaskCommand(api, &jsonOutput),
		newApproveCommand(api, &jsonOutput),
		newRejectCommand(api, &jsonOutput),
		newApprovalsCommand(api, &jsonOutput),
		newFeedbackCommand(api, &jsonOutput),
		newExperienceCommand(api, &jsonOutput),
		newInspectCommand(api, &jsonOutput),
	)
	return command
}

func Execute() error {
	return NewRootCommand().Execute()
}
