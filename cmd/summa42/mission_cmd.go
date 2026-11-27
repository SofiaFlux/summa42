package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/spf13/cobra"
)

func newMissionCommand(api control.API, jsonOutput *bool) *cobra.Command {
	command := &cobra.Command{Use: "mission", Short: "Manage Collective Missions"}
	var ownerKey, switchKey string
	var standby bool
	create := &cobra.Command{
		Use:   "create <statement>",
		Short: "Create the active (or standby) Collective Mission with an Owner signature",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			statement := strings.TrimSpace(args[0])
			if statement == "" {
				return fmt.Errorf("Mission statement is required")
			}
			signer, err := loadOwnerSigner(ownerKey)
			if err != nil {
				return err
			}
			var mission control.MissionDTO
			if standby {
				mission, err = api.CreateStandbyMission(cmd.Context(), statement, signer)
			} else {
				mission, err = api.CreateMission(cmd.Context(), statement, signer)
			}
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(mission)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created Mission %s\n", mission.ID)
			return err
		},
	}
	create.Flags().BoolVar(&standby, "standby", false, "create as standby Mission without deactivating the active Mission")
	create.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the active Mission",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mission, err := api.ActiveMission(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(mission)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Mission %s: %s\n", mission.ID, mission.Statement)
			return err
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List stored Collective Missions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			missions, err := api.Missions(cmd.Context())
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(missions)
			}
			table := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(table, "MISSION_ID\tACTIVE\tSTATEMENT")
			for _, m := range missions {
				fmt.Fprintf(table, "%s\t%t\t%s\n", m.ID, m.Active, m.Statement)
			}
			return table.Flush()
		},
	}
	switchCmd := &cobra.Command{
		Use:   "switch <mission-id>",
		Short: "Switch the active Collective Mission with an Owner signature",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signer, err := loadOwnerSigner(switchKey)
			if err != nil {
				return err
			}
			mission, err := api.SwitchMission(cmd.Context(), domain.ID(strings.TrimSpace(args[0])), signer)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(mission)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Switched active Mission to %s: %s\n", mission.ID, mission.Statement)
			return err
		},
	}
	switchCmd.Flags().StringVar(&switchKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	command.AddCommand(create, show, list, switchCmd)
	return command
}
