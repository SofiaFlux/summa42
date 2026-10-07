package adoreview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/SofiaFlux/summa42/internal/adoeffects"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/evidence"
	"github.com/SofiaFlux/summa42/internal/executors"
	"github.com/SofiaFlux/summa42/internal/operations"
)

type PublishMode string

const (
	PublishNone     PublishMode = "none"
	PublishComments PublishMode = "comments"
	PublishAll      PublishMode = "all"

	commentProviderName = "ado-pr-comment"
	voteProviderName    = "ado-pr-vote"
)

type PublishOps interface {
	Prepare(ctx context.Context, request operations.PrepareRequest) (domain.ExternalOperation, error)
	Dispatch(ctx context.Context, operationID, attemptID domain.ID) (domain.ExternalOperation, error)
}

type PublishConfig struct {
	Mode           PublishMode
	Operations     PublishOps
	Evidence       *evidence.Store
	OwnerApprovals []domain.ID
	RiskComment    string
	RiskApprove    string
}

type PublishPayload struct {
	Decision string `json:"decision"`
	CaseID   string `json:"caseID"`
	WorkID   string `json:"workID"`
	Project  string `json:"project"`
	Repo     string `json:"repo"`
	PR       int64  `json:"pr"`
	Revision string `json:"revision"`
}

type Publisher struct {
	config PublishConfig
}

func (p *Publisher) ExecutionContract() executors.Contract {
	// All modes handle both decisions. Mode gates dispatch inside Start: none
	// records only; comments skips votes. A contract grants no effect authority.
	return executors.Contract{TaskClasses: []string{"publish-decision"}, Capabilities: []string{"ado-publish", "ado.pr.comment", "ado.pr.approve"}, Enforcement: domain.EnforcementEnforced}
}

type publishIntent struct {
	slot     string
	provider string
	intent   operations.IntentDescriptor
}

type publishEvidenceEntry struct {
	Slot         string                `json:"slot"`
	Operation    domain.ID             `json:"operation,omitempty"`
	State        domain.OperationState `json:"state,omitempty"`
	Reference    string                `json:"reference,omitempty"`
	Skipped      bool                  `json:"skipped,omitempty"`
	RecordedOnly bool                  `json:"recorded-only,omitempty"`
}

func NewPublisher(config PublishConfig) (*Publisher, error) {
	switch config.Mode {
	case PublishNone, PublishComments, PublishAll:
	default:
		return nil, fmt.Errorf("unknown publish mode %q", config.Mode)
	}
	if config.Operations == nil || config.Evidence == nil {
		return nil, errors.New("publisher requires operations and evidence services")
	}
	config.OwnerApprovals = append([]domain.ID(nil), config.OwnerApprovals...)
	config.RiskComment = strings.TrimSpace(config.RiskComment)
	if config.RiskComment == "" {
		config.RiskComment = "LOW"
	}
	config.RiskApprove = strings.TrimSpace(config.RiskApprove)
	if config.Mode == PublishAll && config.RiskApprove == "" {
		return nil, errors.New("publish-all mode requires an explicit approve risk")
	}
	return &Publisher{config: config}, nil
}

func (p *Publisher) Start(ctx context.Context, envelope executors.AttemptEnvelope) (executors.ExecutionResult, error) {
	if p == nil || p.config.Operations == nil || p.config.Evidence == nil {
		return executors.ExecutionResult{}, errors.New("publisher is not configured")
	}
	if strings.TrimSpace(envelope.Workspace) == "" {
		return executors.ExecutionResult{}, errors.New("attempt workspace is required")
	}
	if strings.TrimSpace(envelope.Objective) == "" {
		return executors.ExecutionResult{}, errors.New("attempt objective is required")
	}
	payload, err := decodePublishPayload(envelope.PayloadJSON)
	if err != nil {
		return executors.ExecutionResult{}, err
	}
	object, raw, err := p.config.Evidence.Get(ctx, domain.ID(payload.Decision))
	if err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("get review decision: %w", err)
	}
	if object.Kind != "ado.review.decision" {
		return executors.ExecutionResult{}, fmt.Errorf("evidence %q is not an ADO review decision", object.ID)
	}
	var decision ReviewDecision
	if err := json.Unmarshal(raw, &decision); err != nil {
		return executors.ExecutionResult{}, fmt.Errorf("decode review decision: %w", err)
	}
	if decision.Action == DecisionHoldAction {
		return publishExecutionResult([]publishEvidenceEntry{{Slot: "hold", RecordedOnly: true}})
	}
	if decision.Action != DecisionCommentAction && decision.Action != DecisionApproveAction {
		return executors.ExecutionResult{}, fmt.Errorf("invalid review decision action %q", decision.Action)
	}
	intents, err := buildPublishIntents(payload, decision)
	if err != nil {
		return executors.ExecutionResult{}, err
	}
	entries := make([]publishEvidenceEntry, 0, len(intents))
	for _, intent := range intents {
		if p.config.Mode == PublishNone {
			entries = append(entries, publishEvidenceEntry{Slot: intent.slot, RecordedOnly: true})
			continue
		}
		if intent.provider == voteProviderName && p.config.Mode == PublishComments {
			entries = append(entries, publishEvidenceEntry{Slot: intent.slot, Skipped: true})
			continue
		}
		risk := p.config.RiskComment
		if intent.provider == voteProviderName {
			risk = p.config.RiskApprove
		}
		op, err := p.config.Operations.Prepare(ctx, operations.PrepareRequest{
			AttemptID: envelope.AttemptID, Provider: intent.provider, TrustedSlotKey: intent.slot,
			Intent: intent.intent, Risk: risk,
			Attributes: map[string]any{
				"repo": payload.Repo, "pr": payload.PR, "revision": payload.Revision, "project": payload.Project,
			},
			RequiredApprovals: append([]domain.ID(nil), p.config.OwnerApprovals...),
		})
		if op.State == domain.OperationOutcomeUnknown {
			entries = append(entries, publishEvidenceEntry{
				Slot: intent.slot, Operation: op.ID, State: op.State, Reference: op.ProviderReference,
			})
			return publishExecutionResult(entries)
		}
		if err != nil {
			return publishExecutionFailure(entries, fmt.Errorf("prepare %s: %w", intent.slot, err))
		}
		settled, dispatchErr := p.config.Operations.Dispatch(ctx, op.ID, envelope.AttemptID)
		if settled.State == domain.OperationOutcomeUnknown {
			entries = append(entries, publishEvidenceEntry{
				Slot: intent.slot, Operation: op.ID, State: settled.State, Reference: settled.ProviderReference,
			})
			return publishExecutionResult(entries)
		}
		if dispatchErr != nil {
			return publishExecutionFailure(entries, fmt.Errorf("dispatch %s: %w", intent.slot, dispatchErr))
		}
		if settled.State != domain.OperationConfirmedEffect && settled.State != domain.OperationConfirmedNoEffect {
			return publishExecutionFailure(entries, fmt.Errorf("dispatch %s returned unsupported state %s", intent.slot, settled.State))
		}
		entries = append(entries, publishEvidenceEntry{
			Slot: intent.slot, Operation: op.ID, State: settled.State, Reference: settled.ProviderReference,
		})
	}
	return publishExecutionResult(entries)
}

func decodePublishPayload(raw json.RawMessage) (PublishPayload, error) {
	var payload PublishPayload
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return PublishPayload{}, fmt.Errorf("decode publish payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return PublishPayload{}, errors.New("decode publish payload trailer: multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return PublishPayload{}, fmt.Errorf("decode publish payload trailer: %w", err)
	}
	payload.Decision = strings.TrimSpace(payload.Decision)
	payload.CaseID = strings.TrimSpace(payload.CaseID)
	payload.WorkID = strings.TrimSpace(payload.WorkID)
	payload.Project = strings.TrimSpace(payload.Project)
	payload.Repo = strings.TrimSpace(payload.Repo)
	payload.Revision = strings.TrimSpace(payload.Revision)
	if payload.Decision == "" || payload.CaseID == "" || payload.WorkID == "" || payload.Project == "" ||
		payload.Repo == "" || payload.Revision == "" || payload.PR <= 0 {
		return PublishPayload{}, errors.New("publish payload requires decision, caseID, workID, project, repo, positive pr, and revision")
	}
	return payload, nil
}

func buildPublishIntents(payload PublishPayload, decision ReviewDecision) ([]publishIntent, error) {
	if err := validatePublishDecision(decision); err != nil {
		return nil, err
	}
	intents := make([]publishIntent, 0, len(decision.Comments)+1)
	for index, comment := range decision.Comments {
		path := strings.TrimSpace(comment.Path)
		body := strings.TrimSpace(comment.Body)
		if path == "" || body == "" || comment.Line < 0 {
			return nil, fmt.Errorf("review decision comment %d is invalid", index)
		}
		intents = append(intents, publishIntent{
			slot:     fmt.Sprintf("ado.pr.comment:%s/%s#%d:%s:%d", payload.Project, payload.Repo, payload.PR, payload.Revision, index),
			provider: commentProviderName,
			intent: adoeffects.CommentIntent{
				Project: payload.Project, Repository: payload.Repo, PR: payload.PR,
				Path: path, Line: comment.Line, Body: body,
				Marker: fmt.Sprintf("[summa42:%s:%s:%d]", payload.CaseID, payload.WorkID, index),
			},
		})
	}
	if decision.Action == DecisionApproveAction {
		intents = append(intents, publishIntent{
			slot:     fmt.Sprintf("ado.pr.approve:%s/%s#%d:%s", payload.Project, payload.Repo, payload.PR, payload.Revision),
			provider: voteProviderName,
			intent: adoeffects.VoteIntent{
				Project: payload.Project, Repository: payload.Repo, PR: payload.PR, Vote: 10,
			},
		})
	}
	if len(intents) == 0 {
		return nil, errors.New("review decision has no publish intents")
	}
	return intents, nil
}

func validatePublishDecision(decision ReviewDecision) error {
	switch decision.Action {
	case DecisionCommentAction:
		if len(decision.Comments) == 0 {
			return errors.New("comment review decision requires comments")
		}
		if decision.Vote != "" {
			return errors.New("comment review decision must not carry vote")
		}
	case DecisionApproveAction:
		if decision.Vote != "approve" {
			return errors.New("approve review decision requires approve vote")
		}
		if len(decision.Comments) != 0 {
			return errors.New("approve review decision must not carry comments")
		}
	default:
		return fmt.Errorf("invalid review decision action %q", decision.Action)
	}
	return nil
}

func publishExecutionFailure(entries []publishEvidenceEntry, err error) (executors.ExecutionResult, error) {
	result, evidenceErr := publishExecutionResult(entries)
	if evidenceErr != nil {
		return result, errors.Join(err, evidenceErr)
	}
	return result, err
}

func publishExecutionResult(entries []publishEvidenceEntry) (executors.ExecutionResult, error) {
	var content strings.Builder
	for index, entry := range entries {
		raw, err := json.Marshal(entry)
		if err != nil {
			return executors.ExecutionResult{}, fmt.Errorf("encode publish evidence: %w", err)
		}
		if index > 0 {
			content.WriteByte('\n')
		}
		content.Write(raw)
	}
	return executors.ExecutionResult{Evidence: []executors.Evidence{{Kind: executors.EvidenceAgentMessage, Content: content.String()}}}, nil
}
