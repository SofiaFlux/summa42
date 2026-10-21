package experience

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
)

func TestVerifiedOutcomesPromoteThenRollbackWithoutInventingMissingCost(t *testing.T) {
	svc,ap,clk:=newExpHarness(t)
	ctx:=context.Background()
	obsID:=insertObservation(t,svc,"obs-1")
	req,err:=svc.RequestGrant(ctx,GrantInput{
		Kind:AdaptationExecutorPreference,ScopeKey:"repo.review",AllowedExecutors:[]string{"codex","claude"},
		MinVerifiedSamples:3,MaxAcceptanceRegressionBps:0,MaxCostRegressionBps:0,ExpiresAt:clk.Now().Add(24*time.Hour),
	},"requester");if err!=nil{t.Fatal(err)}
	if err:=ap.Approve(ctx,req.ApprovalID,"owner-1",req.Digest);err!=nil{t.Fatal(err)}
	grant,err:=svc.ActivateGrant(ctx,req.ID);if err!=nil{t.Fatal(err)}
	proposal,err:=svc.Propose(ctx,ProposalInput{
		GrantID:grant.ID,GenericTaskClass:domain.GenericTaskReview,ScopeKey:"repo.review",
		PreferredExecutor:"codex",EvidenceObservationIDs:[]domain.ID{obsID},
	});if err!=nil{t.Fatal(err)}

	for i:=0;i<2;i++{
		taskID:=insertVerifiedTask(t,svc,fmt.Sprintf("accepted-%d",i),"codex",true)
		if err:=svc.ObserveVerifiedOutcome(ctx,VerifiedOutcome{
			TaskID:taskID,GenericTaskClass:domain.GenericTaskReview,ScopeKey:"repo.review",ExecutorKind:"codex",
			Accepted:true,RetryCount:0,CostUnits:nil,LatencyMs:nil,
		});err!=nil{t.Fatal(err)}
	}
	rule,err:=svc.Evaluate(ctx,proposal.ID);if err!=nil{t.Fatal(err)}
	if rule.State!=domain.ExperienceShadow||rule.VerifiedSamples!=2{t.Fatalf("rule=%+v",rule)}

	taskID:=insertVerifiedTask(t,svc,"accepted-2","codex",true)
	if err:=svc.ObserveVerifiedOutcome(ctx,VerifiedOutcome{
		TaskID:taskID,GenericTaskClass:domain.GenericTaskReview,ScopeKey:"repo.review",ExecutorKind:"codex",Accepted:true,
	});err!=nil{t.Fatal(err)}
	rule,err=svc.Evaluate(ctx,proposal.ID);if err!=nil{t.Fatal(err)}
	if rule.State!=domain.ExperienceActive||rule.VerifiedSamples!=3{t.Fatalf("active rule=%+v",rule)}
	pref,found,err:=svc.Preference(ctx,PreferenceQuery{ScopeKey:"repo.review",GenericTaskClass:domain.GenericTaskReview})
	if err!=nil||!found||pref.ExecutorKind!="codex"{t.Fatalf("preference=%+v found=%v err=%v",pref,found,err)}

	badTask:=insertVerifiedTask(t,svc,"challenged-1","codex",false)
	if err:=svc.ObserveVerifiedOutcome(ctx,VerifiedOutcome{
		TaskID:badTask,GenericTaskClass:domain.GenericTaskReview,ScopeKey:"repo.review",ExecutorKind:"codex",Accepted:false,
	});err!=nil{t.Fatal(err)}
	rule,err=svc.Evaluate(ctx,proposal.ID);if err!=nil{t.Fatal(err)}
	if rule.State!=domain.ExperienceRolledBack{t.Fatalf("rollback rule=%+v",rule)}
	if _,found,err:=svc.Preference(ctx,PreferenceQuery{ScopeKey:"repo.review"});err!=nil||found{
		t.Fatalf("rolled-back preference found=%v err=%v",found,err)
	}
}

func TestUnverifiedOutcomeIsRejected(t *testing.T) {
	svc,_,_:=newExpHarness(t)
	taskID:=insertBareTask(t,svc,"unverified","codex")
	err:=svc.ObserveVerifiedOutcome(context.Background(),VerifiedOutcome{
		TaskID:taskID,GenericTaskClass:domain.GenericTaskReview,ScopeKey:"repo.review",ExecutorKind:"codex",Accepted:true,
	})
	if err==nil{t.Fatal("unverified accepted outcome counted")}
}

func insertObservation(t *testing.T,svc *Service,id domain.ID) domain.ID {
	t.Helper();now:=formatTime(svc.clock.Now().UTC())
	if _,err:=svc.store.DB().Exec(
		"INSERT INTO field_observations(observation_id, collective_id, category, basis_class, source_kind, summary_local, metrics_json, runtime_version, executor_kind, executor_version, enforcement, created_at) VALUES (?, 'collective', 'TEST', 'TEST', 'TEST', 'local', '{}', '', '', '', 'ENFORCED', ?)",
		id,now);err!=nil{t.Fatal(err)}
	return id
}

func insertBareTask(t *testing.T,svc *Service,name,executor string) domain.ID {
	t.Helper();ctx:=context.Background();now:=formatTime(svc.clock.Now().UTC())
	taskID:=domain.ID("task-"+name);attemptID:=domain.ID("attempt-"+name)
	if _,err:=svc.store.DB().ExecContext(ctx,
		"INSERT INTO tasks(task_id,purpose_kind,purpose_id,state,current_attempt_id,current_fence,acceptance_criteria_json,required_capabilities_json,required_enforcement,authority_ceiling_json,resource_envelope_id,priority,created_at,updated_at,task_class) VALUES (?, 'OWNER_DIRECTIVE', ?, 'SUCCEEDED', ?, 1, '[]', '[]', 'ENFORCED', '[]', 'env', 0, ?, ?, 'repo.review')",
		taskID,name,attemptID,now,now);err!=nil{t.Fatal(err)}
	if _,err:=svc.store.DB().ExecContext(ctx,
		"INSERT INTO attempts(attempt_id,task_id,state,fence_generation,lease_state,lease_expires_at,started_at,completed_at,executor_kind) VALUES (?, ?, 'COMPLETED', 1, 'REVOKED', ?, ?, ?, ?)",
		attemptID,taskID,formatTime(svc.clock.Now().Add(time.Hour)),now,now,executor);err!=nil{t.Fatal(err)}
	return taskID
}

func insertVerifiedTask(t *testing.T,svc *Service,name,executor string,accepted bool) domain.ID {
	t.Helper();ctx:=context.Background();taskID:=insertBareTask(t,svc,name,executor);now:=formatTime(svc.clock.Now().UTC())
	var attemptID domain.ID
	if err:=svc.store.DB().QueryRowContext(ctx,"SELECT current_attempt_id FROM tasks WHERE task_id = ?",taskID).Scan(&attemptID);err!=nil{t.Fatal(err)}
	if accepted{
		if _,err:=svc.store.DB().ExecContext(ctx,
			"INSERT INTO acceptance_records(acceptance_id,task_id,attempt_id,verifier_id,verifier_type,criteria_result_json,evidence_ids_json,created_at) VALUES (?, ?, ?, 'verifier', 'TEST', '{\"met\":true}', '[]', ?)",
			domain.NewID("acceptance"),taskID,attemptID,now);err!=nil{t.Fatal(err)}
	}else{
		if _,err:=svc.store.DB().ExecContext(ctx,
			"UPDATE tasks SET state = 'CHALLENGED', updated_at = ? WHERE task_id = ?",
			now,taskID);err!=nil{t.Fatal(err)}
		if _,err:=svc.store.DB().ExecContext(ctx,
			"INSERT INTO task_challenges(challenge_id,task_id,scope,reason,evidence_ids_json,created_at) VALUES (?, ?, 'TASK', 'verified regression', '[]', ?)",
			domain.NewID("challenge"),taskID,now);err!=nil{t.Fatal(err)}
		if _,err:=svc.store.DB().ExecContext(ctx,
			"INSERT INTO execution_events(event_id,task_id,attempt_id,event_type,details_json,created_at) VALUES (?, ?, ?, 'TASK_CHALLENGED', '{}', ?)",
			domain.NewID("event"),taskID,attemptID,now);err!=nil{t.Fatal(err)}
	}
	return taskID
}

func TestAuthorityPromotionKindRejected(t *testing.T) {
	svc, _, clk := newExpHarness(t)
	ctx := context.Background()
	if _, err := svc.RequestGrant(ctx, GrantInput{
		Kind: "AUTHORITY_PROMOTION", ScopeKey: "github.pr.review", AllowedExecutors: []string{"copilot"},
		MinVerifiedSamples: 2, ExpiresAt: clk.Now().Add(24 * time.Hour),
	}, "requester-1"); err == nil {
		t.Fatal("AUTHORITY_PROMOTION grant accepted")
	}
	// The DB trigger rejects it even when the Go check is bypassed.
	_, err := svc.store.DB().Exec(`INSERT INTO adaptation_grants(grant_id, grant_request_id, adaptation_kind, scope_key, allowed_executors_json, min_verified_samples, max_acceptance_regression_bps, max_cost_regression_bps, owner_principal_id, expires_at, activated_at)
		VALUES ('g-auth','r-auth','AUTHORITY_PROMOTION','s','["x"]',1,0,0,'owner','2030-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err == nil || !strings.Contains(err.Error(), "only EXECUTOR_PREFERENCE adaptation is permitted") {
		t.Fatalf("grant trigger err=%v", err)
	}
	_, err = svc.store.DB().Exec(`INSERT INTO experience_rules(rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, created_at, updated_at)
		VALUES ('rule-auth','p','g',1,'AUTHORITY_PROMOTION','s','REVIEW','x','ACTIVE',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	if err == nil || !strings.Contains(err.Error(), "only EXECUTOR_PREFERENCE adaptation is permitted") {
		t.Fatalf("rule trigger err=%v", err)
	}
}

func TestAmbiguousPreferenceFallsBackToBaseline(t *testing.T) {
	svc, ap, clk := newExpHarness(t)
	ctx := context.Background()
	req, err := svc.RequestGrant(ctx, GrantInput{
		Kind: AdaptationExecutorPreference, ScopeKey: "repo.review", AllowedExecutors: []string{"codex", "claude"},
		MinVerifiedSamples: 1, ExpiresAt: clk.Now().Add(24 * time.Hour),
	}, "requester")
	if err != nil {
		t.Fatal(err)
	}
	if err := ap.Approve(ctx, req.ApprovalID, "owner-1", req.Digest); err != nil {
		t.Fatal(err)
	}
	grant, err := svc.ActivateGrant(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := formatTime(clk.Now().UTC())
	for i, exec := range []string{"codex", "claude"} {
		pid := fmt.Sprintf("p%d", i)
		if _, err := svc.store.DB().Exec(`INSERT INTO experience_proposals(proposal_id, grant_id, generic_task_class, scope_key, preferred_executor, state, created_at, updated_at) VALUES (?, ?, 'REVIEW', 'repo.review', ?, 'ACTIVE', ?, ?)`, pid, grant.ID, exec, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.store.DB().Exec(`INSERT INTO experience_rules(rule_id, proposal_id, grant_id, version, adaptation_kind, scope_key, generic_task_class, preferred_executor, state, verified_samples, activated_at, created_at, updated_at) VALUES (?, ?, ?, 1, 'EXECUTOR_PREFERENCE', 'repo.review', 'REVIEW', ?, 'ACTIVE', 1, ?, ?, ?)`, "rule"+pid, pid, grant.ID, exec, now, now, now); err != nil {
			t.Fatal(err)
		}
	}
	_, found, err := svc.Preference(ctx, PreferenceQuery{ScopeKey: "repo.review"})
	if err != nil || found {
		t.Fatalf("ambiguous preference found=%v err=%v", found, err)
	}
	if _, found, err := svc.PreferredExecutor(ctx, domain.Task{TaskClass: "repo.review"}, []string{"codex", "claude"}); err != nil || found {
		t.Fatalf("PreferredExecutor found=%v err=%v", found, err)
	}
	var n int
	if err := svc.store.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE kind = 'EXPERIENCE_PREFERENCE_AMBIGUOUS'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("ambiguity audit events=%d err=%v", n, err)
	}
}
