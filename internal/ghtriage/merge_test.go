package ghtriage_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SofiaFlux/summa42/internal/approvals"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/ghpublish"
	"github.com/SofiaFlux/summa42/internal/ghtriage"
	"github.com/SofiaFlux/summa42/internal/operations"
	"github.com/SofiaFlux/summa42/internal/policy"
	"github.com/SofiaFlux/summa42/internal/resources"
)

type tokenSource struct{}

func (tokenSource) Token(context.Context) (string, error) { return "test-token", nil }

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }

func response(status int, v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
}

type mergePolicy struct{ required []domain.ID }

func (p mergePolicy) Evaluate(_ context.Context, in policy.PolicyInput) (domain.PolicyDecision, error) {
	outcome := domain.PolicyAllow
	if len(p.required) > 0 {
		outcome = domain.PolicyRequireApproval
	}
	return domain.PolicyDecision{
		RequiredApprovals:      p.required,
		ID:                     domain.NewID("decision"),
		Outcome:                outcome,
		PolicySetID:            "merge_policy",
		PolicySetHash:          "merge-v1",
		PolicyCapabilitiesHash: "merge-caps",
		InputDigest:            "fixture",
		EvaluatedAt:            in.Now,
	}, nil
}

func mergeOps(t *testing.T, f *driverFixture) (*operations.Service, *publicationProvider) {
	t.Helper()
	_, _ = f.store.DB().ExecContext(f.ctx, `INSERT INTO policy_sets(policy_set_id,version,module_name,module,policy_hash,capabilities_hash,active,created_at) VALUES ('merge_policy',1,'fixture','package fixture','merge-v1','merge-caps',1,?)`, f.clock.Now().Format("2006-01-02T15:04:05Z07:00"))

	b := &publicationProvider{name: ghpublish.BranchProviderName, cap: "github.repo.publish"}
	p := &publicationProvider{name: ghpublish.PRProviderName, cap: "github.pr.create"}
	m := &publicationProvider{name: ghpublish.MergeProviderName, cap: "github.pr.merge"}

	return operations.New(f.store, f.clock, f.execSvc, mergePolicy{}, resources.New(f.store, f.clock), approvals.New(f.store, f.clock), "merge_collective", b, p, m), m
}

func grantMerge(t *testing.T, f *driverFixture, id domain.ID) {
	t.Helper()
	grantPublication(t, f, id)
	c, err := f.cases.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	c.Grant.Capabilities = append(c.Grant.Capabilities, "github.pr.merge")
	c.Grant.Actions = append(c.Grant.Actions, "github.pr.merge")
	raw, _ := json.Marshal(c.Grant)
	if _, err = f.store.DB().ExecContext(f.ctx, `UPDATE workflow_cases SET grant_json=? WHERE case_id=?`, string(raw), id); err != nil {
		t.Fatal(err)
	}
}

func publishedFixture(t *testing.T) (*driverFixture, ghtriage.GroundingConfig, domain.ID, *operations.Service, *ghtriage.PublicationRecord) {
	t.Helper()
	f, cfg, id, _ := implementedFixture(t)
	r, err := ghtriage.NewCodeReviewer(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, &codeReviewModel{verdict: "ACCEPT"}).Review(f.ctx, id, cfg.Source.LocalPath)
	if err != nil || !r.Accepted {
		t.Fatal(err)
	}
	grantMerge(t, f, id)
	ops, m := mergeOps(t, f)
	_ = m
	pub := ghtriage.NewPublisher(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops)
	pubResult, err := pub.Publish(f.ctx, id, cfg.Source.LocalPath, "main")
	if err != nil || pubResult.Record == nil {
		t.Fatalf("publish failed: %v", err)
	}
	return f, cfg, id, ops, pubResult.Record
}

func TestMergerSuccessAndReplay(t *testing.T) {
	f, cfg, id, ops, pubRecord := publishedFixture(t)
	candidateSHA := pubRecord.CandidateSHA

	httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		if strings.Contains(path, "/check-runs") {
			return response(200, map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"name": "test", "status": "completed", "conclusion": "success", "head_sha": candidateSHA},
				},
			}), nil
		}
		if strings.Contains(path, "/reviews") {
			return response(200, []map[string]any{
				{"user": map[string]string{"login": "reviewer"}, "state": "APPROVED", "commit_id": candidateSHA},
			}), nil
		}
		return response(404, nil), nil
	})}

	obs, err := ghpublish.NewObserver(ghpublish.Config{
		Repository:       "owner/repo",
		CredentialSource: tokenSource{},
		HTTPClient:       httpClient,
	})
	if err != nil {
		t.Fatal(err)
	}

	merger := ghtriage.NewMerger(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops, obs)
	sourcePath := cfg.Source.LocalPath
	res, err := merger.Merge(f.ctx, id, sourcePath, "main", "merge", []string{"test"})
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if res.Record == nil || res.Record.MergeCommitSHA == "" {
		t.Fatalf("expected merge record with commit sha, got %+v", res)
	}

	// Replay returns cached record
	replay, err := merger.Merge(f.ctx, id, sourcePath, "main", "merge", []string{"test"})
	if err != nil || !replay.Reused || replay.Record.MergeCommitSHA != res.Record.MergeCommitSHA {
		t.Fatalf("expected replay of merge record, got %+v, err: %v", replay, err)
	}
}

func TestMergerPendingCIBlocksMerge(t *testing.T) {
	f, cfg, id, ops, pubRecord := publishedFixture(t)
	candidateSHA := pubRecord.CandidateSHA

	httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/check-runs") {
			return response(200, map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"name": "test", "status": "in_progress", "conclusion": nil, "head_sha": candidateSHA},
				},
			}), nil
		}
		return response(200, []any{}), nil
	})}

	obs, _ := ghpublish.NewObserver(ghpublish.Config{
		Repository:       "owner/repo",
		CredentialSource: tokenSource{},
		HTTPClient:       httpClient,
	})

	merger := ghtriage.NewMerger(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops, obs)
	sourcePath := cfg.Source.LocalPath
	res, err := merger.Merge(f.ctx, id, sourcePath, "main", "merge", []string{"test"})
	if err != nil {
		t.Fatalf("expected no error on pending CI, got %v", err)
	}
	if !res.Pending {
		t.Fatalf("expected res.Pending == true when CI is pending, got %+v", res)
	}
}

func TestMergerFailedCIRejectsMerge(t *testing.T) {
	f, cfg, id, ops, pubRecord := publishedFixture(t)
	candidateSHA := pubRecord.CandidateSHA

	httpClient := &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/check-runs") {
			return response(200, map[string]any{
				"total_count": 1,
				"check_runs": []map[string]any{
					{"name": "test", "status": "completed", "conclusion": "failure", "head_sha": candidateSHA},
				},
			}), nil
		}
		return response(200, []any{}), nil
	})}

	obs, _ := ghpublish.NewObserver(ghpublish.Config{
		Repository:       "owner/repo",
		CredentialSource: tokenSource{},
		HTTPClient:       httpClient,
	})

	merger := ghtriage.NewMerger(f.cases, f.execSvc, f.verifSvc, f.evidenceStore, ops, obs)
	sourcePath := cfg.Source.LocalPath
	_, err := merger.Merge(f.ctx, id, sourcePath, "main", "merge", []string{"test"})
	if err == nil || !strings.Contains(err.Error(), "CI failed") {
		t.Fatalf("expected error indicating CI failed, got %v", err)
	}
}
