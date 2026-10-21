package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
)

type API interface {
	Status(context.Context) (StatusDTO, error)
	CreateTask(context.Context, CreateTaskRequest) (TaskDTO, error)
	CreateMission(context.Context, string, identity.Signer) (MissionDTO, error)
	ActiveMission(context.Context) (MissionDTO, error)
	Task(context.Context, domain.ID) (TaskDTO, error)
	Approvals(context.Context) ([]ApprovalDTO, error)
	Approval(context.Context, domain.ID) (ApprovalDTO, error)
	Feedback(context.Context) ([]FeedbackCandidateDTO, error)
	FeedbackCandidateCreate(context.Context, FeedbackCandidateCreateRequest) (FeedbackCandidateDTO, error)
	FeedbackSanitize(context.Context, domain.ID) (FeedbackSanitizeDTO, error)
	FeedbackInspect(context.Context, domain.ID) (FeedbackInspectDTO, error)
	FeedbackEmit(context.Context, domain.ID) (FeedbackEmitDTO, error)
	FeedbackObserve(context.Context, FeedbackObserveRequest) (FeedbackObservationDTO, error)
	FeedbackScan(context.Context) ([]FeedbackObservationDTO, error)
	ExperienceGrantRequest(context.Context, ExperienceGrantCreateRequest) (ExperienceGrantRequestDTO, error)
	ExperienceGrantActivate(context.Context, domain.ID) (AdaptationGrantDTO, error)
	ExperienceProposalCreate(context.Context, ExperienceProposalCreateRequest) (ExperienceProposalDTO, error)
	ExperienceOutcomeCreate(context.Context, ExperienceOutcomeCreateRequest) (ExperienceOutcomeDTO, error)
	ExperienceEvaluate(context.Context, domain.ID) (ExperienceRuleDTO, error)
	ExperienceRules(context.Context) ([]ExperienceRuleDTO, error)
	ExperienceRule(context.Context, domain.ID) (ExperienceRuleDTO, error)
	Approve(ctx context.Context, id domain.ID, signer identity.Signer, expectedDigest string) (ApprovalDTO, error)
	Reject(context.Context, domain.ID, identity.Signer) (ApprovalDTO, error)
	Attempt(context.Context, domain.ID) (AttemptDTO, error)
	Operation(context.Context, domain.ID) (OperationDTO, error)
	Shutdown(context.Context) error
}

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL, token string, client *http.Client) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://summa42.local"
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: strings.TrimSpace(token), http: client}
}

func NewLocalClient(endpoint, token string) *Client {
	return NewClient("http://summa42.local", token, localHTTPClient(endpoint))
}

func (c *Client) Status(ctx context.Context) (StatusDTO, error) {
	var result StatusDTO
	err := c.doJSON(ctx, http.MethodGet, "/status", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) CreateTask(ctx context.Context, request CreateTaskRequest) (TaskDTO, error) {
	var result TaskDTO
	err := c.doJSON(ctx, http.MethodPost, "/tasks", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) CreateMission(ctx context.Context, statement string, signer identity.Signer) (MissionDTO, error) {
	if signer == nil {
		return MissionDTO{}, errors.New("Owner signer is required")
	}
	statement = strings.TrimSpace(statement)
	var challenge MissionChallengeDTO
	if err := c.doJSON(ctx, http.MethodPost, "/missions/challenge", CreateMissionRequest{Statement: statement}, &challenge, http.StatusOK); err != nil {
		return MissionDTO{}, err
	}
	if challenge.RequestDigest != missionDigest(statement) || challenge.Challenge == "" {
		return MissionDTO{}, errors.New("Mission challenge does not match statement")
	}
	signature, err := signer.Sign(MissionSigningMessage(challenge.Challenge, challenge.RequestDigest))
	if err != nil {
		return MissionDTO{}, fmt.Errorf("sign Mission challenge: %w", err)
	}
	request := CreateMissionRequest{Statement: statement, Challenge: challenge.Challenge, Signature: base64.StdEncoding.EncodeToString(signature)}
	var result MissionDTO
	err = c.doJSON(ctx, http.MethodPost, "/missions", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) ActiveMission(ctx context.Context) (MissionDTO, error) {
	var result MissionDTO
	err := c.doJSON(ctx, http.MethodGet, "/missions/active", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Task(ctx context.Context, id domain.ID) (TaskDTO, error) {
	var result TaskDTO
	err := c.doJSON(ctx, http.MethodGet, "/tasks/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Approvals(ctx context.Context) ([]ApprovalDTO, error) {
	var result []ApprovalDTO
	err := c.doJSON(ctx, http.MethodGet, "/approvals", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Approval(ctx context.Context, id domain.ID) (ApprovalDTO, error) {
	var result ApprovalDTO
	err := c.doJSON(ctx, http.MethodGet, "/approvals/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Feedback(ctx context.Context) ([]FeedbackCandidateDTO, error) {
	var result []FeedbackCandidateDTO
	err := c.doJSON(ctx, http.MethodGet, "/feedback", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) FeedbackCandidateCreate(ctx context.Context, request FeedbackCandidateCreateRequest) (FeedbackCandidateDTO, error) {
	var result FeedbackCandidateDTO
	err := c.doJSON(ctx, http.MethodPost, "/feedback/candidates", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) FeedbackSanitize(ctx context.Context, id domain.ID) (FeedbackSanitizeDTO, error) {
	var result FeedbackSanitizeDTO
	err := c.doJSON(ctx, http.MethodPost, "/feedback/"+string(id)+"/sanitize", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) FeedbackInspect(ctx context.Context, id domain.ID) (FeedbackInspectDTO, error) {
	var result FeedbackInspectDTO
	err := c.doJSON(ctx, http.MethodGet, "/feedback/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) FeedbackEmit(ctx context.Context, id domain.ID) (FeedbackEmitDTO, error) {
	var result FeedbackEmitDTO
	err := c.doJSON(ctx, http.MethodPost, "/feedback/"+string(id)+"/emit", nil, &result, http.StatusAccepted)
	return result, err
}

func (c *Client) FeedbackObserve(ctx context.Context, request FeedbackObserveRequest) (FeedbackObservationDTO, error) {
	var result FeedbackObservationDTO
	err := c.doJSON(ctx, http.MethodPost, "/feedback/observations", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) FeedbackScan(ctx context.Context) ([]FeedbackObservationDTO, error) {
	var result []FeedbackObservationDTO
	err := c.doJSON(ctx, http.MethodPost, "/feedback/scan", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) ExperienceGrantRequest(ctx context.Context, request ExperienceGrantCreateRequest) (ExperienceGrantRequestDTO, error) {
	var result ExperienceGrantRequestDTO
	err := c.doJSON(ctx, http.MethodPost, "/experience/grants/requests", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) ExperienceGrantActivate(ctx context.Context, id domain.ID) (AdaptationGrantDTO, error) {
	var result AdaptationGrantDTO
	err := c.doJSON(ctx, http.MethodPost, "/experience/grants/"+string(id)+"/activate", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) ExperienceProposalCreate(ctx context.Context, request ExperienceProposalCreateRequest) (ExperienceProposalDTO, error) {
	var result ExperienceProposalDTO
	err := c.doJSON(ctx, http.MethodPost, "/experience/proposals", request, &result, http.StatusCreated)
	return result, err
}

func (c *Client) ExperienceOutcomeCreate(ctx context.Context, request ExperienceOutcomeCreateRequest) (ExperienceOutcomeDTO, error) {
	var result ExperienceOutcomeDTO
	err := c.doJSON(ctx, http.MethodPost, "/experience/outcomes", request, &result, http.StatusAccepted)
	return result, err
}

func (c *Client) ExperienceEvaluate(ctx context.Context, id domain.ID) (ExperienceRuleDTO, error) {
	var result ExperienceRuleDTO
	err := c.doJSON(ctx, http.MethodPost, "/experience/proposals/"+string(id)+"/evaluate", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) ExperienceRules(ctx context.Context) ([]ExperienceRuleDTO, error) {
	var result []ExperienceRuleDTO
	err := c.doJSON(ctx, http.MethodGet, "/experience", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) ExperienceRule(ctx context.Context, id domain.ID) (ExperienceRuleDTO, error) {
	var result ExperienceRuleDTO
	err := c.doJSON(ctx, http.MethodGet, "/experience/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

// Approve signs only a challenge whose digest equals expectedDigest, the digest the Owner was shown.
func (c *Client) Approve(ctx context.Context, id domain.ID, signer identity.Signer, expectedDigest string) (ApprovalDTO, error) {
	return c.decideApproval(ctx, id, signer, "APPROVE", expectedDigest)
}

func (c *Client) Reject(ctx context.Context, id domain.ID, signer identity.Signer) (ApprovalDTO, error) {
	return c.decideApproval(ctx, id, signer, "REJECT", "")
}

func (c *Client) decideApproval(ctx context.Context, id domain.ID, signer identity.Signer, action, expectedDigest string) (ApprovalDTO, error) {
	if signer == nil {
		return ApprovalDTO{}, errors.New("Owner signer is required")
	}
	var challenge ApprovalChallengeDTO
	path := "/approvals/" + string(id) + "/challenge?action=" + action
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &challenge, http.StatusOK); err != nil {
		return ApprovalDTO{}, err
	}
	if challenge.Action != action {
		return ApprovalDTO{}, errors.New("approval challenge action mismatch")
	}
	if expectedDigest != "" && challenge.RequestDigest != expectedDigest {
		return ApprovalDTO{}, errors.New("approval challenge digest differs from the displayed digest; refusing to sign")
	}
	signature, err := signer.Sign(ApprovalSigningMessage(challenge.Challenge, challenge.RequestDigest, challenge.Action))
	if err != nil {
		return ApprovalDTO{}, fmt.Errorf("sign approval challenge: %w", err)
	}
	request := ApprovalRequest{Challenge: challenge.Challenge, Signature: base64.StdEncoding.EncodeToString(signature)}
	var result ApprovalDTO
	endpoint := "/approvals/" + string(id) + "/" + strings.ToLower(action)
	err = c.doJSON(ctx, http.MethodPost, endpoint, request, &result, http.StatusOK)
	return result, err
}

func (c *Client) Attempt(ctx context.Context, id domain.ID) (AttemptDTO, error) {
	var result AttemptDTO
	err := c.doJSON(ctx, http.MethodGet, "/inspect/attempts/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Operation(ctx context.Context, id domain.ID) (OperationDTO, error) {
	var result OperationDTO
	err := c.doJSON(ctx, http.MethodGet, "/inspect/operations/"+string(id), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Shutdown(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/shutdown", nil, nil, http.StatusAccepted)
}

func (c *Client) doJSON(ctx context.Context, method, path string, requestBody, responseBody any, wantStatus int) error {
	if c == nil || c.http == nil || strings.TrimSpace(c.baseURL) == "" {
		return errors.New("control client is not configured")
	}
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode control request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("control request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		var apiError struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&apiError)
		if strings.TrimSpace(apiError.Error) == "" {
			apiError.Error = response.Status
		}
		return fmt.Errorf("control API %s %s: %s", method, path, apiError.Error)
	}
	if responseBody == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(responseBody); err != nil {
		return fmt.Errorf("decode control response: %w", err)
	}
	return nil
}
