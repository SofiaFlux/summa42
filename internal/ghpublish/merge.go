package ghpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/repoworkspace"
	"github.com/SofiaFlux/summa42/internal/resources"
)

const MergeProviderName = "github-candidate-merge"

type MergeIntent struct {
	Repository    string `json:"repository"`
	PullNumber    int    `json:"pull_number"`
	HeadSHA       string `json:"head_sha"`
	BaseBranch    string `json:"base_branch"`
	MergeMethod   string `json:"merge_method"` // "merge", "squash", or "rebase"
	CommitTitle   string `json:"commit_title,omitempty"`
	CommitMessage string `json:"commit_message,omitempty"`
}

func (MergeIntent) DescriptorType() string { return "github.candidate.merge.v1" }

type MergeProvider struct {
	c *client
}

func NewMergeProvider(c Config) (*MergeProvider, error) {
	cl, err := newClient(c)
	if err != nil {
		return nil, err
	}
	return &MergeProvider{c: cl}, nil
}

func (*MergeProvider) Name() string                              { return MergeProviderName }
func (*MergeProvider) Capability() string                        { return "github.pr.merge" }
func (*MergeProvider) EnforcementLevel() domain.EnforcementLevel { return domain.EnforcementEnforced }
func (*MergeProvider) AdapterVersion() string                    { return "1" }
func (*MergeProvider) AdapterVersionSemanticallyRelevant() bool  { return true }

func (p *MergeProvider) validate(v MergeIntent) error {
	if v.Repository != p.c.repo || v.PullNumber <= 0 || !validRef(v.BaseBranch) || !repoworkspace.FullCommit(v.HeadSHA) {
		return errors.New("invalid merge intent")
	}
	switch v.MergeMethod {
	case "merge", "squash", "rebase":
	default:
		return errors.New("invalid merge method")
	}
	return nil
}

func (p *MergeProvider) CanonicalIntent(d operations.IntentDescriptor) ([]byte, error) {
	v, ok := d.(MergeIntent)
	if !ok {
		return nil, errors.New("merge descriptor required")
	}
	if err := p.validate(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func (p *MergeProvider) CostProfile(d operations.IntentDescriptor) (operations.CostProfile, error) {
	_, e := p.CanonicalIntent(d)
	return operations.CostProfile{MaxExposure: 1, Enforceability: resources.Enforceability{CostControl: resources.CostTechnicallyCapped, RequireHardCap: true, Source: "bounded GitHub write-request units"}}, e
}

type pullMergeState struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func (p *MergeProvider) readPR(ctx context.Context, pullNumber int) (*pullMergeState, error) {
	var pr pullMergeState
	path := fmt.Sprintf("/pulls/%d", pullNumber)
	if _, err := p.c.request(ctx, "GET", path, nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (p *MergeProvider) Dispatch(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v MergeIntent
	if err := decode(r.CanonicalIntent, &v); err != nil {
		return unknown(), err
	}
	if err := p.validate(v); err != nil {
		return unknown(), err
	}

	pr, err := p.readPR(ctx, v.PullNumber)
	if err != nil {
		return unknown(), err
	}
	if pr.Merged {
		return operations.ProviderOutcome{
			State:             domain.OperationConfirmedEffect,
			ProviderReference: pr.MergeCommitSHA,
			ActualCost:        1,
		}, nil
	}

	if pr.Head.SHA != v.HeadSHA {
		return unknown(), fmt.Errorf("remote PR head SHA %s does not match expected candidate %s", pr.Head.SHA, v.HeadSHA)
	}
	if pr.Base.Ref != v.BaseBranch {
		return unknown(), fmt.Errorf("remote PR base %s does not match expected %s", pr.Base.Ref, v.BaseBranch)
	}

	reqBody := map[string]any{
		"sha":          v.HeadSHA,
		"merge_method": v.MergeMethod,
	}
	if v.CommitTitle != "" {
		reqBody["commit_title"] = v.CommitTitle
	}
	if v.CommitMessage != "" {
		reqBody["commit_message"] = v.CommitMessage
	}

	var mergeResp struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}

	path := fmt.Sprintf("/pulls/%d/merge", v.PullNumber)
	_, mergeErr := p.c.request(ctx, "PUT", path, reqBody, &mergeResp)
	if mergeErr != nil {
		return p.LookupOutcome(ctx, r)
	}

	if mergeResp.Merged && mergeResp.SHA != "" {
		return operations.ProviderOutcome{
			State:             domain.OperationConfirmedEffect,
			ProviderReference: mergeResp.SHA,
			ActualCost:        1,
		}, nil
	}

	return p.LookupOutcome(ctx, r)
}

func (p *MergeProvider) LookupOutcome(ctx context.Context, r operations.ProviderDispatchRequest) (operations.ProviderOutcome, error) {
	var v MergeIntent
	if err := decode(r.CanonicalIntent, &v); err != nil {
		return unknown(), err
	}
	if err := p.validate(v); err != nil {
		return unknown(), err
	}

	pr, err := p.readPR(ctx, v.PullNumber)
	if err != nil {
		return unknown(), nil
	}
	if pr.Merged && pr.MergeCommitSHA != "" {
		return operations.ProviderOutcome{
			State:             domain.OperationConfirmedEffect,
			ProviderReference: pr.MergeCommitSHA,
			ActualCost:        1,
		}, nil
	}
	return unknown(), nil
}
