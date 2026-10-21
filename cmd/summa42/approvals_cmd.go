package main

import (
	"encoding/json"
	"fmt"

	"github.com/SofiaFlux/summa42/internal/control"
	"github.com/spf13/cobra"
)

func newApprovalsCommand(api control.API, jsonOutput *bool) *cobra.Command {
	return &cobra.Command{
		Use:"approvals",Short:"List pending durable approvals",
		RunE:func(cmd *cobra.Command,args []string) error{
			items,err:=api.Approvals(cmd.Context());if err!=nil{return err}
			if *jsonOutput{return json.NewEncoder(cmd.OutOrStdout()).Encode(items)}
			if len(items)==0{_,err=fmt.Fprintln(cmd.OutOrStdout(),"No pending approvals.");return err}
			for _,item:=range items{
				if _,err=fmt.Fprintf(cmd.OutOrStdout(),"%s\t%s\t%s/%s\texpires %s\n",
					item.ApprovalID,item.Status,item.SubjectKind,item.SubjectID,item.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"));err!=nil{return err}
				describeApproval(cmd.OutOrStdout(),item)
			}
			return nil
		},
	}
}
