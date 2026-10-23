package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/localconfig"
	summa42runtime "github.com/SofiaFlux/summa42/internal/runtime"
	"github.com/SofiaFlux/summa42/internal/workflowcase"
)

// KindCaseBlock is the evidence an Owner's manual block cites.
const KindCaseBlock = "summa42.owner.case_block"

func parseBlockCasesFlags(args []string) (reason string, ids []domain.ID, err error) {
	flags := flag.NewFlagSet("block-cases", flag.ContinueOnError)
	flags.StringVar(&reason, "reason", "", "why the cases are held (recorded in the assessment and evidence)")
	if err = flags.Parse(args); err != nil {
		return
	}
	for _, id := range flags.Args() {
		if strings.TrimSpace(id) != "" {
			ids = append(ids, domain.ID(strings.TrimSpace(id)))
		}
	}
	if strings.TrimSpace(reason) == "" || len(ids) == 0 {
		err = errors.New("block-cases requires --reason and at least one case ID")
	}
	return
}

// runBlockCases holds ACTIVE cases through Assess (the Owner's manual stop).
// One evidence object records the reason and the case list; each case is
// blocked in its own transaction and reported as one JSON line.
func runBlockCases(ctx context.Context, args []string) error {
	reason, ids, err := parseBlockCasesFlags(args)
	if err != nil {
		return err
	}
	home, err := localconfig.ResolveHome("")
	if err != nil {
		return err
	}
	cfg, err := localconfig.Load(home)
	if err != nil {
		return err
	}
	material, err := loadStartupMaterial(ctx, cfg)
	if err != nil {
		return err
	}
	box, err := summa42runtime.Open(ctx, summa42runtime.Config{StatePath: cfg.DatabasePath, EvidencePath: cfg.EvidencePath, CollectiveID: cfg.CollectiveID, OwnerPrincipalID: cfg.OwnerPrincipalID, PolicyEngine: material.policyEngine})
	if err != nil {
		return err
	}
	defer box.Close()
	record, err := json.Marshal(struct {
		Reason  string      `json:"reason"`
		CaseIDs []domain.ID `json:"case_ids"`
	}{reason, ids})
	if err != nil {
		return err
	}
	object, err := box.Evidence.Put(ctx, bytes.NewReader(record), evidence.Metadata{MediaType: "application/json", Kind: KindCaseBlock})
	if err != nil {
		return err
	}
	cases := workflowcase.New(box.Store, box.Clock, box.Purpose)
	out := json.NewEncoder(os.Stdout)
	failed := 0
	for _, id := range ids {
		line := map[string]string{"case_id": string(id), "evidence_id": string(object.ID)}
		if c, err := cases.Block(ctx, id, reason, object.ID); err != nil {
			failed++
			line["error"] = err.Error()
		} else {
			line["state"] = string(c.State)
		}
		if err := out.Encode(line); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d cases not blocked", failed, len(ids))
	}
	return nil
}
