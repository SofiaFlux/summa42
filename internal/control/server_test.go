package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SofiaFlux/summa42/internal/clock"
	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/purpose"
	state "github.com/SofiaFlux/summa42/internal/state/sqlite"
)

type fakeStatusProvider struct {
	calls int
	value StatusDTO
}

func (f *fakeStatusProvider) Status(context.Context) (StatusDTO, error) {
	f.calls++
	return f.value, nil
}

type fakeTaskService struct {
	created int
	last    execution.TaskRequest
	task    domain.Task
}

type fakeMissionService struct {
	statement string
	calls     int
}

func (f *fakeMissionService) CreateMission(_ context.Context, statement string) (domain.ID, error) {
	f.calls++
	f.statement = statement
	return "mission-1", nil
}
func (f *fakeMissionService) ActiveMission(context.Context) (domain.ID, string, error) {
	return "mission-1", f.statement, nil
}

func (f *fakeTaskService) CreateTask(_ context.Context, request execution.TaskRequest) (domain.Task, error) {
	f.created++
	f.last = request
	return f.task, nil
}

func (f *fakeTaskService) Task(context.Context, domain.ID) (domain.Task, error) {
	return f.task, nil
}

type fakeApprovalService struct {
	calls       int
	rejectCalls int
	id          domain.ID
	actor       domain.ID
	digest      string
	record      domain.ApprovalRequestRecord
}

func (f *fakeApprovalService) Get(_ context.Context, id domain.ID) (domain.ApprovalRequestRecord, error) {
	record := f.record
	record.ID = id
	return record, nil
}

func (f *fakeApprovalService) Pending(context.Context) ([]domain.ApprovalRequestRecord, error) {
	if f.record.ID == "" {
		return nil, nil
	}
	return []domain.ApprovalRequestRecord{f.record}, nil
}

func (f *fakeApprovalService) Approve(_ context.Context, id, actor domain.ID, digest string) error {
	f.calls++
	f.id = id
	f.actor = actor
	f.digest = digest
	f.record.ID = id
	f.record.State = domain.ApprovalApproved
	f.record.ApproverID = actor
	return nil
}

func (f *fakeApprovalService) Reject(_ context.Context, id, actor domain.ID, digest string) error {
	f.rejectCalls++
	f.id = id
	f.actor = actor
	f.digest = digest
	f.record.ID = id
	f.record.State = domain.ApprovalRejected
	f.record.ApproverID = actor
	return nil
}

type fakeAttemptReader struct{ attempt domain.Attempt }

func (f *fakeAttemptReader) Attempt(context.Context, domain.ID) (domain.Attempt, error) {
	return f.attempt, nil
}

type fakeOperationReader struct{ operation domain.ExternalOperation }

func (f *fakeOperationReader) Operation(context.Context, domain.ID) (domain.ExternalOperation, error) {
	return f.operation, nil
}

type fakeShutdownService struct{ calls int }

func (f *fakeShutdownService) RequestShutdown(context.Context) error {
	f.calls++
	return nil
}

func TestControlAPIRequiresBearerAuthAndStatusIsReadOnly(t *testing.T) {
	server, status, _, _, _, _, _ := newTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response, err := http.Get(httpServer.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if status.calls != 0 {
		t.Fatalf("status provider called before authentication: %d", status.calls)
	}

	response = doRequest(t, http.MethodGet, httpServer.URL+"/status", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", response.StatusCode)
	}
	var got StatusDTO
	decodeJSON(t, response, &got)
	if got.State != "DORMANT" || got.CollectiveID != "collective-test" {
		t.Fatalf("status = %+v", got)
	}

	response = doRequest(t, http.MethodPost, httpServer.URL+"/status", "control-secret", bytes.NewBufferString("{}"))
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /status = %d, want 405", response.StatusCode)
	}
	if status.calls != 1 {
		t.Fatalf("status provider calls = %d, want exactly one read", status.calls)
	}
}

func TestMissionCreationRequiresOwnerSignatureBoundToStatement(t *testing.T) {
	server, _, _, _, _, _, _ := newTestServer(t)
	missions := &fakeMissionService{}
	server.deps.Missions = missions
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	statement := "Maintain this repository safely"
	challengeBody, _ := json.Marshal(CreateMissionRequest{Statement: statement})
	response := doRequest(t, http.MethodPost, httpServer.URL+"/missions/challenge", "control-secret", bytes.NewReader(challengeBody))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("mission challenge status = %d", response.StatusCode)
	}
	var challenge MissionChallengeDTO
	decodeJSON(t, response, &challenge)
	if challenge.Challenge == "" || challenge.RequestDigest == "" {
		t.Fatalf("empty challenge: %+v", challenge)
	}

	request := CreateMissionRequest{Statement: statement, Challenge: challenge.Challenge}
	requestBody, _ := json.Marshal(request)
	response = doRequest(t, http.MethodPost, httpServer.URL+"/missions", "control-secret", bytes.NewReader(requestBody))
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || missions.calls != 0 {
		t.Fatalf("bearer token created mission: status=%d calls=%d", response.StatusCode, missions.calls)
	}

	request.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(testOwnerPrivateKey(t), MissionSigningMessage(challenge.Challenge, challenge.RequestDigest)))
	request.Statement = "Different mission"
	requestBody, _ = json.Marshal(request)
	response = doRequest(t, http.MethodPost, httpServer.URL+"/missions", "control-secret", bytes.NewReader(requestBody))
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || missions.calls != 0 {
		t.Fatalf("changed statement accepted: status=%d calls=%d", response.StatusCode, missions.calls)
	}

	request.Statement = statement
	requestBody, _ = json.Marshal(request)
	response = doRequest(t, http.MethodPost, httpServer.URL+"/missions", "control-secret", bytes.NewReader(requestBody))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("signed mission status = %d", response.StatusCode)
	}
	var mission MissionDTO
	decodeJSON(t, response, &mission)
	if mission.ID != "mission-1" || mission.Statement != statement || missions.calls != 1 {
		t.Fatalf("mission=%+v calls=%d", mission, missions.calls)
	}
	response = doRequest(t, http.MethodGet, httpServer.URL+"/missions/active", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("active Mission status = %d", response.StatusCode)
	}
	decodeJSON(t, response, &mission)
	if mission.ID != "mission-1" || mission.Statement != statement {
		t.Fatalf("active Mission = %+v", mission)
	}
	response = doRequest(t, http.MethodPost, httpServer.URL+"/missions", "control-secret", bytes.NewReader(requestBody))
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || missions.calls != 1 {
		t.Fatalf("signature replay accepted: status=%d calls=%d", response.StatusCode, missions.calls)
	}
}

func TestClientCreatesMissionThroughSignedControlFlow(t *testing.T) {
	server, _, _, _, _, _, _ := newTestServer(t)
	signer, err := identity.NewLocalEd25519(filepath.Join(t.TempDir(), "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	server.config.OwnerPublicKey = signer.PublicKey()
	missions := &fakeMissionService{}
	server.deps.Missions = missions
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := NewClient(httpServer.URL, "control-secret", httpServer.Client())
	mission, err := client.CreateMission(context.Background(), "  Maintain the project  ", signer)
	if err != nil {
		t.Fatal(err)
	}
	if mission.ID != "mission-1" || missions.statement != "Maintain the project" || missions.calls != 1 {
		t.Fatalf("mission=%+v service=%+v", mission, missions)
	}
}

func TestSignedMissionBecomesValidDurableTaskPurpose(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.DB().Close()
	service := purpose.New(store, clock.System{})
	server, _, _, _, _, _, _ := newTestServer(t)
	signer, err := identity.NewLocalEd25519(filepath.Join(t.TempDir(), "owner.key"), "owner")
	if err != nil {
		t.Fatal(err)
	}
	server.config.OwnerPublicKey = signer.PublicKey()
	server.deps.Missions = service
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := NewClient(httpServer.URL, "control-secret", httpServer.Client())
	mission, err := client.CreateMission(ctx, "Maintain the repository", signer)
	if err != nil {
		t.Fatal(err)
	}
	active, err := client.ActiveMission(ctx)
	if err != nil || active != mission {
		t.Fatalf("active Mission = %+v, err=%v; created %+v", active, err, mission)
	}
	if err := service.ValidatePurpose(ctx, domain.PurposeRef{Kind: domain.PurposeMission, ID: mission.ID}); err != nil {
		t.Fatalf("created Mission cannot be used as task purpose: %v", err)
	}
	if _, err := client.CreateMission(ctx, "Competing Mission", signer); err == nil {
		t.Fatal("second active Mission was accepted")
	}
}

func TestMissionChallengeKeepsOnlyNewestPendingRequest(t *testing.T) {
	server, _, _, _, _, _, _ := newTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(CreateMissionRequest{Statement: "Mission"})
		response := doRequest(t, http.MethodPost, httpServer.URL+"/missions/challenge", "control-secret", bytes.NewReader(body))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("challenge status = %d", response.StatusCode)
		}
		response.Body.Close()
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.challenges) != 1 {
		t.Fatalf("pending challenges = %d, want 1", len(server.challenges))
	}
}

func TestApprovalRequiresOwnerSignatureOverChallengeAndDigest(t *testing.T) {
	server, _, _, approvals, _, _, _ := newTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := doRequest(t, http.MethodPost, httpServer.URL+"/approvals/approval-1/approve", "control-secret", bytes.NewBufferString(`{}`))
	response.Body.Close()
	if response.StatusCode == http.StatusOK || approvals.calls != 0 {
		t.Fatalf("authenticated transport acted as Owner: status=%d calls=%d", response.StatusCode, approvals.calls)
	}

	challengeResponse := doRequest(t, http.MethodGet, httpServer.URL+"/approvals/approval-1/challenge?action=APPROVE", "control-secret", nil)
	if challengeResponse.StatusCode != http.StatusOK {
		t.Fatalf("challenge status = %d", challengeResponse.StatusCode)
	}
	var challenge ApprovalChallengeDTO
	decodeJSON(t, challengeResponse, &challenge)
	if challenge.Challenge == "" || challenge.RequestDigest == "" || challenge.Action != "APPROVE" {
		t.Fatalf("empty approval challenge: %+v", challenge)
	}

	_, wrongPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongSignature := base64.StdEncoding.EncodeToString(ed25519.Sign(wrongPrivate, ApprovalSigningMessage(challenge.Challenge, challenge.RequestDigest, "APPROVE")))
	response = postApproval(t, httpServer.URL, challenge, wrongSignature)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || approvals.calls != 0 {
		t.Fatalf("wrong Owner signature status=%d calls=%d", response.StatusCode, approvals.calls)
	}

	ownerSignature := base64.StdEncoding.EncodeToString(ed25519.Sign(testOwnerPrivateKey(t), ApprovalSigningMessage(challenge.Challenge, challenge.RequestDigest, "APPROVE")))
	response = postApproval(t, httpServer.URL, challenge, ownerSignature)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid Owner approval status = %d, want 200", response.StatusCode)
	}
	if approvals.calls != 1 || approvals.id != "approval-1" || approvals.actor != "owner-test" || approvals.digest != challenge.RequestDigest {
		t.Fatalf("approval call = calls:%d id:%s actor:%s digest:%s", approvals.calls, approvals.id, approvals.actor, approvals.digest)
	}

	response = postApproval(t, httpServer.URL, challenge, ownerSignature)
	response.Body.Close()
	if response.StatusCode == http.StatusOK || approvals.calls != 1 {
		t.Fatalf("approval challenge replay accepted: status=%d calls=%d", response.StatusCode, approvals.calls)
	}
}

func TestControlRoutesUseCoreServiceInterfaces(t *testing.T) {
	server, _, tasks, _, attempts, operations, shutdown := newTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	create := CreateTaskRequest{
		Purpose:             domain.PurposeRef{Kind: domain.PurposeOwnerDirective, ID: "owner-directive-1"},
		TaskClass:           "repo.review",
		AcceptanceCriteria:  []string{"result is verified"},
		RequiredEnforcement: domain.EnforcementPartial,
		ResourceEnvelopeID:  "resource-1",
		Priority:            7,
	}
	payload, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	response := doRequest(t, http.MethodPost, httpServer.URL+"/tasks", "control-secret", bytes.NewReader(payload))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("POST /tasks = %d", response.StatusCode)
	}
	var task TaskDTO
	decodeJSON(t, response, &task)
	if task.ID != "task-1" || tasks.created != 1 || tasks.last.Priority != 7 || tasks.last.TaskClass != "repo.review" {
		t.Fatalf("task route did not preserve TaskClass: dto=%+v calls=%d request=%+v", task, tasks.created, tasks.last)
	}

	response = doRequest(t, http.MethodGet, httpServer.URL+"/tasks/task-1", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET task = %d", response.StatusCode)
	}
	decodeJSON(t, response, &task)
	if task.ID != "task-1" {
		t.Fatalf("task = %+v", task)
	}

	response = doRequest(t, http.MethodGet, httpServer.URL+"/inspect/attempts/attempt-1", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET attempt = %d", response.StatusCode)
	}
	var attempt AttemptDTO
	decodeJSON(t, response, &attempt)
	if attempt.ID != attempts.attempt.ID {
		t.Fatalf("attempt = %+v", attempt)
	}

	response = doRequest(t, http.MethodGet, httpServer.URL+"/inspect/operations/operation-1", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET operation = %d", response.StatusCode)
	}
	var operation OperationDTO
	decodeJSON(t, response, &operation)
	if operation.ID != operations.operation.ID {
		t.Fatalf("operation = %+v", operation)
	}

	response = doRequest(t, http.MethodPost, httpServer.URL+"/shutdown", "control-secret", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted || shutdown.calls != 1 {
		t.Fatalf("shutdown status=%d calls=%d", response.StatusCode, shutdown.calls)
	}
}

func TestListenLocalUsesPlatformLocalTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix network assertion is covered on non-Windows CI; Windows is compile-gated separately")
	}
	listener, err := ListenLocal(filepath.Join(t.TempDir(), "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if got := listener.Addr().Network(); got != "unix" {
		t.Fatalf("listener network = %q, want unix", got)
	}
}

var ownerPrivate ed25519.PrivateKey

func newTestServer(t *testing.T) (*Server, *fakeStatusProvider, *fakeTaskService, *fakeApprovalService, *fakeAttemptReader, *fakeOperationReader, *fakeShutdownService) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ownerPrivate = private
	status := &fakeStatusProvider{value: StatusDTO{CollectiveID: "collective-test", State: "DORMANT"}}
	tasks := &fakeTaskService{task: domain.Task{ID: "task-1", State: domain.TaskEligible, Priority: 7}}
	approvals := &fakeApprovalService{record: domain.ApprovalRequestRecord{
		ID: "approval-1", SubjectKind: "EXTERNAL_OPERATION", SubjectID: "operation-1",
		RequestDigest: "durable-digest-1", PolicyDecisionID: "decision-1",
		RequiredApprovers: []domain.ID{"owner-test"}, RequestedBy: "cube-test",
		State: domain.ApprovalPending, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}}
	attempts := &fakeAttemptReader{attempt: domain.Attempt{ID: "attempt-1", TaskID: "task-1", State: domain.AttemptRunning}}
	operations := &fakeOperationReader{operation: domain.ExternalOperation{ID: "operation-1", TaskID: "task-1", AttemptID: "attempt-1", State: domain.OperationPrepared}}
	shutdown := &fakeShutdownService{}
	server, err := NewServer(ServerConfig{
		AuthToken:        "control-secret",
		OwnerPrincipalID: "owner-test",
		OwnerPublicKey:   public,
		ChallengeTTL:     time.Minute,
	}, Dependencies{
		Status: status, Tasks: tasks, Missions: &fakeMissionService{}, Approvals: approvals, Attempts: attempts, Operations: operations, Shutdown: shutdown,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server, status, tasks, approvals, attempts, operations, shutdown
}

func testOwnerPrivateKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	if len(ownerPrivate) != ed25519.PrivateKeySize {
		t.Fatal("Owner private key fixture not initialized")
	}
	return ownerPrivate
}

func doRequest(t *testing.T, method, url, token string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func postApproval(t *testing.T, baseURL string, challenge ApprovalChallengeDTO, signature string) *http.Response {
	t.Helper()
	payload, err := json.Marshal(ApprovalRequest{Challenge: challenge.Challenge, Signature: signature})
	if err != nil {
		t.Fatal(err)
	}
	return doRequest(t, http.MethodPost, baseURL+"/approvals/approval-1/approve", "control-secret", bytes.NewReader(payload))
}

func decodeJSON(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("content type = %q, want application/json", response.Header.Get("Content-Type"))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRejectAndReadRoutesUseDurableDigest(t *testing.T) {
	server, _, _, approvals, _, _, _ := newTestServer(t)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := doRequest(t, http.MethodGet, httpServer.URL+"/approvals", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /approvals = %d", response.StatusCode)
	}
	var pending []ApprovalDTO
	decodeJSON(t, response, &pending)
	if len(pending) != 1 || pending[0].RequestDigest != "durable-digest-1" {
		t.Fatalf("pending approvals = %+v", pending)
	}

	response = doRequest(t, http.MethodGet, httpServer.URL+"/approvals/approval-1", "control-secret", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET approval = %d", response.StatusCode)
	}
	var got ApprovalDTO
	decodeJSON(t, response, &got)
	if got.RequestDigest != "durable-digest-1" || got.Status != "PENDING" {
		t.Fatalf("approval = %+v", got)
	}

	challengeResponse := doRequest(t, http.MethodGet,
		httpServer.URL+"/approvals/approval-1/challenge?action=REJECT", "control-secret", nil)
	if challengeResponse.StatusCode != http.StatusOK {
		t.Fatalf("reject challenge status = %d", challengeResponse.StatusCode)
	}
	var challenge ApprovalChallengeDTO
	decodeJSON(t, challengeResponse, &challenge)
	if challenge.Action != "REJECT" || challenge.RequestDigest != "durable-digest-1" {
		t.Fatalf("reject challenge = %+v", challenge)
	}

	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(
		testOwnerPrivateKey(t),
		ApprovalSigningMessage(challenge.Challenge, challenge.RequestDigest, "REJECT"),
	))
	payload, err := json.Marshal(ApprovalRequest{Challenge: challenge.Challenge, Signature: signature})
	if err != nil {
		t.Fatal(err)
	}
	response = doRequest(t, http.MethodPost, httpServer.URL+"/approvals/approval-1/reject",
		"control-secret", bytes.NewReader(payload))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("reject status = %d", response.StatusCode)
	}
	decodeJSON(t, response, &got)
	if approvals.rejectCalls != 1 || got.Status != "REJECTED" || approvals.digest != "durable-digest-1" {
		t.Fatalf("reject calls=%d digest=%s dto=%+v", approvals.rejectCalls, approvals.digest, got)
	}
}

func TestOwnerCannotChallengeApprovalThatDoesNotRequireOwner(t *testing.T) {
	server, _, _, approvals, _, _, _ := newTestServer(t)
	approvals.record.RequiredApprovers = []domain.ID{"security-only"}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := doRequest(t, http.MethodGet,
		httpServer.URL+"/approvals/approval-1/challenge?action=APPROVE", "control-secret", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("challenge status = %d, want 403", response.StatusCode)
	}
}

func TestApprovalDTOIncludesCanonicalIntent(t *testing.T) {
	server, _, _, _, _, operations, _ := newTestServer(t)
	operations.operation.Provider = "github"
	operations.operation.EffectSlotID = "slot-1"
	operations.operation.CanonicalIntent = []byte(`{"branch":"fix/x"}`)
	operations.operation.Attributes = map[string]any{"caller": "cube"}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	response := doRequest(t, http.MethodGet, httpServer.URL+"/approvals/approval-1", "control-secret", nil)
	var approval ApprovalDTO
	decodeJSON(t, response, &approval)
	if approval.Provider != "github" || approval.EffectSlotID != "slot-1" ||
		string(approval.CanonicalIntent) != `{"branch":"fix/x"}` || approval.Attributes["caller"] != "cube" {
		t.Fatalf("approval DTO lacks bound intent: %+v", approval)
	}
	response = doRequest(t, http.MethodGet, httpServer.URL+"/inspect/operations/operation-1", "control-secret", nil)
	var operation OperationDTO
	decodeJSON(t, response, &operation)
	if string(operation.CanonicalIntent) != `{"branch":"fix/x"}` || operation.Attributes["caller"] != "cube" {
		t.Fatalf("operation DTO lacks intent: %+v", operation)
	}
}
