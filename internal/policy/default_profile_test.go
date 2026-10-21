package policy

import (
	"context"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestDefaultProfileCompilesAndAllowsOnlyAuthorizedLowRisk(t *testing.T) {
	profile, err := DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	engine := NewOPAEngine(OPAConfig{
		ModuleName:    profile.ModuleName,
		Module:        profile.Module,
		PolicySetID:   profile.PolicySetID,
		PolicySetHash: profile.PolicyHash,
	})
	metadata, err := engine.Metadata()
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PolicySetHash != profile.PolicyHash || metadata.PolicyCapabilitiesHash != profile.CapabilitiesHash {
		t.Fatalf("metadata=%+v profile=%+v", metadata, profile)
	}
	now := time.Now().UTC()
	allowed, err := engine.Evaluate(context.Background(), PolicyInput{Risk: "LOW", AuthorityValid: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Outcome != domain.PolicyAllow {
		t.Fatalf("authorized LOW risk outcome=%s", allowed.Outcome)
	}
	denied, err := engine.Evaluate(context.Background(), PolicyInput{Risk: "HIGH", AuthorityValid: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if denied.Outcome != domain.PolicyDeny {
		t.Fatalf("HIGH risk outcome=%s", denied.Outcome)
	}
}

func TestPolicyV2RequiresOwnerForMedium(t *testing.T) {
	profile, err := DefaultProfile()
	if err != nil {
		t.Fatal(err)
	}
	if profile.PolicySetID != "policy_mvc_default_v2" {
		t.Fatalf("default policy set = %s, want policy_mvc_default_v2", profile.PolicySetID)
	}
	engine := NewOPAEngine(OPAConfig{
		ModuleName: profile.ModuleName, Module: profile.Module,
		PolicySetID: profile.PolicySetID, PolicySetHash: profile.PolicyHash,
	})
	now := time.Now().UTC()
	owner := domain.ID("principal_owner")
	cases := []struct {
		name      string
		in        PolicyInput
		outcome   domain.PolicyOutcome
		approvers []domain.ID
	}{
		{"low authorized", PolicyInput{Risk: "LOW", AuthorityValid: true, OwnerPrincipalID: owner}, domain.PolicyAllow, nil},
		{"medium authorized", PolicyInput{Risk: "MEDIUM", AuthorityValid: true, OwnerPrincipalID: owner}, domain.PolicyRequireApproval, []domain.ID{owner}},
		{"medium without owner", PolicyInput{Risk: "MEDIUM", AuthorityValid: true}, domain.PolicyDeny, nil},
		{"medium unauthorized", PolicyInput{Risk: "MEDIUM", OwnerPrincipalID: owner}, domain.PolicyDeny, nil},
		{"low unauthorized", PolicyInput{Risk: "LOW", OwnerPrincipalID: owner}, domain.PolicyDeny, nil},
		{"high authorized", PolicyInput{Risk: "HIGH", AuthorityValid: true, OwnerPrincipalID: owner}, domain.PolicyDeny, nil},
		{"caller owner attribute ignored", PolicyInput{Risk: "MEDIUM", AuthorityValid: true, Attributes: map[string]any{"caller": map[string]any{"owner_principal_id": "intruder"}}}, domain.PolicyDeny, nil},
	}
	for _, tc := range cases {
		tc.in.Now = now
		decision, err := engine.Evaluate(context.Background(), tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if decision.Outcome != tc.outcome {
			t.Fatalf("%s: outcome=%s want %s", tc.name, decision.Outcome, tc.outcome)
		}
		if len(decision.RequiredApprovals) != len(tc.approvers) || (len(tc.approvers) == 1 && decision.RequiredApprovals[0] != tc.approvers[0]) {
			t.Fatalf("%s: required approvals=%v want %v", tc.name, decision.RequiredApprovals, tc.approvers)
		}
	}
}
