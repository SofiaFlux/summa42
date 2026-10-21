package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/spf13/cobra"
)

// stdinIsTTY reports whether stdin is a terminal; tests replace it.
var stdinIsTTY = func() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// describeApproval writes what the Owner is about to sign: the bound operation's provider, slot, intent and attributes.
func describeApproval(w io.Writer, a control.ApprovalDTO) {
	fmt.Fprintf(w, "Approval %s (%s %s) digest %s expires %s\n", a.ApprovalID, a.SubjectKind, a.SubjectID, a.RequestDigest, a.ExpiresAt.Format(time.RFC3339))
	if a.Provider != "" || a.EffectSlotID != "" {
		fmt.Fprintf(w, "Provider: %s\nDescriptor: %s\nEffect slot: %s\nFingerprint: %s (revision %d)\n", a.Provider, a.DescriptorType, a.EffectSlotID, a.IntentFingerprint, a.IntentRevision)
	}
	if len(a.CanonicalIntent) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, a.CanonicalIntent, "", "  ") != nil {
			pretty.Reset()
			pretty.Write(a.CanonicalIntent)
		}
		fmt.Fprintf(w, "Intent:\n%s\n", pretty.String())
	}
	if len(a.Attributes) > 0 {
		attributes, _ := json.MarshalIndent(a.Attributes, "", "  ")
		fmt.Fprintf(w, "Attributes (informational, not signed):\n%s\n", attributes)
	}
}

func newApproveCommand(api control.API, jsonOutput *bool) *cobra.Command {
	var ownerKey string
	var assumeYes bool
	command := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a pending action with an Owner signature",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ownerKey = strings.TrimSpace(ownerKey)
			if ownerKey == "" {
				home, err := localconfig.ResolveHome("")
				if err != nil {
					return err
				}
				ownerKey = filepath.Join(home, "keys", "owner.key")
			}
			if _, err := os.Stat(ownerKey); err != nil {
				return fmt.Errorf("Owner key must already exist: %w", err)
			}
			signer, err := identity.NewLocalEd25519(ownerKey, "owner")
			if err != nil {
				return fmt.Errorf("load Owner key: %w", err)
			}
			tty := stdinIsTTY()
			if assumeYes && (tty || os.Getenv("SUMMA42_APPROVE_NONINTERACTIVE") != "1") {
				return errors.New("--yes is accepted only when stdin is not a TTY and SUMMA42_APPROVE_NONINTERACTIVE=1")
			}
			if !assumeYes && !tty {
				return errors.New("approval needs confirmation on a TTY (non-interactive: --yes with SUMMA42_APPROVE_NONINTERACTIVE=1)")
			}
			pending, err := api.Approval(cmd.Context(), domain.ID(args[0]))
			if err != nil {
				return err
			}
			if pending.SubjectKind == "EXTERNAL_OPERATION" && len(pending.CanonicalIntent) == 0 {
				return errors.New("refusing to approve: the server did not return the operation's canonical intent")
			}
			if pending.SubjectKind == "EXTERNAL_OPERATION" {
				recomputed, err := operations.OperationApprovalDigest(pending.Provider, pending.DescriptorType, pending.EffectSlotID,
					pending.IntentFingerprint, pending.IntentRevision, pending.CanonicalIntent)
				if err != nil || recomputed != pending.RequestDigest {
					return fmt.Errorf("refusing to approve: displayed intent does not match request digest (%v)", err)
				}
			}
			describeApproval(cmd.ErrOrStderr(), pending)
			if !assumeYes {
				fmt.Fprint(cmd.ErrOrStderr(), "Type 'yes' to approve: ")
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("approval not confirmed")
				}
			}
			approval, err := api.Approve(cmd.Context(), domain.ID(args[0]), signer, pending.RequestDigest)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(approval)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Approval %s: %s by %s\n", approval.ApprovalID, approval.Status, approval.ApprovedBy)
			return err
		},
	}
	command.Flags().BoolVar(&assumeYes, "yes", false, "skip confirmation (only when stdin is not a TTY and SUMMA42_APPROVE_NONINTERACTIVE=1)")
	command.Flags().StringVar(&ownerKey, "owner-key", "", "path to an existing Owner Ed25519 private key (default: SUMMA42_HOME/keys/owner.key)")
	return command
}
