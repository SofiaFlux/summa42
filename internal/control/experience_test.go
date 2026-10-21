package control

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/experience"
)

type fakeExperienceLifecycle struct {
	proposalInput experience.ProposalInput
	outcomeInput  experience.VerifiedOutcome
	evaluatedID   domain.ID
	proposal      domain.ExperienceProposal
	rule          domain.ExperienceRule
}

func (f *fakeExperienceLifecycle) RequestGrant(context.Context, experience.GrantInput, domain.ID) (experience.GrantRequest, error) {
	return experience.GrantRequest{}, nil
}
func (f *fakeExperienceLifecycle) ActivateGrant(context.Context, domain.ID) (domain.AdaptationGrant, error) {
	return domain.AdaptationGrant{}, nil
}
func (f *fakeExperienceLifecycle) Propose(_ context.Context, input experience.ProposalInput) (domain.ExperienceProposal, error) {
	f.proposalInput = input
	return f.proposal, nil
}
func (f *fakeExperienceLifecycle) ObserveVerifiedOutcome(_ context.Context, input experience.VerifiedOutcome) error {
	f.outcomeInput = input
	return nil
}
func (f *fakeExperienceLifecycle) Evaluate(_ context.Context, id domain.ID) (domain.ExperienceRule, error) {
	f.evaluatedID = id
	return f.rule, nil
}
func (f *fakeExperienceLifecycle) Rules(context.Context) ([]domain.ExperienceRule, error) {
	return []domain.ExperienceRule{f.rule}, nil
}
func (f *fakeExperienceLifecycle) Rule(context.Context, domain.ID) (domain.ExperienceRule, error) {
	return f.rule, nil
}

func TestExperienceControlExposesGovernedLifecycleWithoutCallerScopeAuthority(t *testing.T) {
	server, _, _, _, _, _, _ := newTestServer(t)
	fake := &fakeExperienceLifecycle{
		proposal: domain.ExperienceProposal{
			ID: "proposal-1", GrantID: "grant-1", ScopeKey: "repo.review",
			GenericTaskClass: domain.GenericTaskReview, PreferredExecutor: "codex", State: domain.ExperienceCandidate,
		},
		rule: domain.ExperienceRule{
			ID: "rule-1", ProposalID: "proposal-1", GrantID: "grant-1", ScopeKey: "repo.review",
			GenericTaskClass: domain.GenericTaskReview, PreferredExecutor: "codex", State: domain.ExperienceShadow, VerifiedSamples: 1,
		},
	}
	server.deps.Experience = fake
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := doRequest(t, http.MethodPost, httpServer.URL+"/experience/proposals", "control-secret",
		bytes.NewBufferString(`{"grant_id":"grant-1","preferred_executor":"codex","evidence_observation_ids":["obs-1"]}`))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("proposal status=%d", response.StatusCode)
	}
	var proposal ExperienceProposalDTO
	decodeJSON(t, response, &proposal)
	if proposal.ID != "proposal-1" || fake.proposalInput.ScopeKey != "" || fake.proposalInput.GenericTaskClass != "" {
		t.Fatalf("proposal=%+v service input=%+v", proposal, fake.proposalInput)
	}

	response = doRequest(t, http.MethodPost, httpServer.URL+"/experience/outcomes", "control-secret",
		bytes.NewBufferString(`{"task_id":"task-1","accepted":true}`))
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("outcome status=%d", response.StatusCode)
	}
	if fake.outcomeInput.TaskID != "task-1" || fake.outcomeInput.ScopeKey != "" || fake.outcomeInput.ExecutorKind != "" || fake.outcomeInput.GenericTaskClass != "" {
		t.Fatalf("outcome service input=%+v", fake.outcomeInput)
	}

	response = doRequest(t, http.MethodPost, httpServer.URL+"/experience/proposals/proposal-1/evaluate", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("evaluate status=%d", response.StatusCode)
	}
	var rule ExperienceRuleDTO
	decodeJSON(t, response, &rule)
	if fake.evaluatedID != "proposal-1" || rule.ID != "rule-1" {
		t.Fatalf("evaluated=%s rule=%+v", fake.evaluatedID, rule)
	}

}

func TestControlRejectsCallerSuppliedMetrics(t *testing.T) {
	server, _, _, _, _, _, _ := newTestServer(t)
	fake := &fakeExperienceLifecycle{}
	server.deps.Experience = fake
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	for _, body := range []string{
		`{"task_id":"task-1","accepted":true,"cost_units":0}`,
		`{"task_id":"task-1","accepted":true,"latency_ms":1}`,
		`{"task_id":"task-1","accepted":true,"retry_count":0}`,
		`{"task_id":"task-1","accepted":true,"human_intervention":false}`,
	} {
		response := doRequest(t, http.MethodPost, httpServer.URL+"/experience/outcomes", "control-secret", bytes.NewBufferString(body))
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s status=%d", body, response.StatusCode)
		}
		if fake.outcomeInput.TaskID != "" {
			t.Fatalf("service called for %s", body)
		}
	}
}
