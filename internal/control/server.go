package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/execution"
)

type StatusDTO struct {
	CollectiveID     domain.ID `json:"collective_id"`
	State            string    `json:"state"`
	ActiveTasks      int       `json:"active_tasks,omitempty"`
	ActiveAttempts   int       `json:"active_attempts,omitempty"`
	PendingApprovals int       `json:"pending_approvals,omitempty"`
}

type CreateTaskRequest struct {
	Purpose              domain.PurposeRef       `json:"purpose"`
	TaskClass            string                  `json:"task_class,omitempty"`
	AcceptanceCriteria   []string                `json:"acceptance_criteria"`
	RequiredCapabilities []string                `json:"required_capabilities,omitempty"`
	RequiredEnforcement  domain.EnforcementLevel `json:"required_enforcement"`
	AuthorityCeiling     []string                `json:"authority_ceiling,omitempty"`
	ResourceEnvelopeID   domain.ID               `json:"resource_envelope_id"`
	Priority             int                     `json:"priority"`
	EarliestStart        time.Time               `json:"earliest_start,omitempty"`
	Deadline             time.Time               `json:"deadline,omitempty"`
}

type CreateMissionRequest struct {
	Statement string `json:"statement"`
	Challenge string `json:"challenge,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type MissionChallengeDTO struct {
	Challenge     string `json:"challenge"`
	RequestDigest string `json:"request_digest"`
}

type MissionDTO struct {
	ID        domain.ID `json:"id"`
	Statement string    `json:"statement"`
}

type TaskDTO struct {
	ID                   domain.ID               `json:"id"`
	ParentTaskID         domain.ID               `json:"parent_task_id,omitempty"`
	Purpose              domain.PurposeRef       `json:"purpose"`
	TaskClass            string                  `json:"task_class,omitempty"`
	State                domain.TaskState        `json:"state"`
	CurrentAttemptID     domain.ID               `json:"current_attempt_id,omitempty"`
	CurrentFence         int64                   `json:"current_fence"`
	AcceptanceCriteria   []string                `json:"acceptance_criteria,omitempty"`
	RequiredCapabilities []string                `json:"required_capabilities,omitempty"`
	RequiredEnforcement  domain.EnforcementLevel `json:"required_enforcement"`
	AuthorityCeiling     []string                `json:"authority_ceiling,omitempty"`
	ResourceEnvelopeID   domain.ID               `json:"resource_envelope_id,omitempty"`
	Priority             int                     `json:"priority"`
	EarliestStart        time.Time               `json:"earliest_start,omitempty"`
	Deadline             time.Time               `json:"deadline,omitempty"`
	CreatedAt            time.Time               `json:"created_at,omitempty"`
	UpdatedAt            time.Time               `json:"updated_at,omitempty"`
}

type AttemptDTO struct {
	ID              domain.ID           `json:"id"`
	TaskID          domain.ID           `json:"task_id"`
	State           domain.AttemptState `json:"state"`
	FenceGeneration int64               `json:"fence_generation"`
	LeaseState      domain.LeaseState   `json:"lease_state"`
	LeaseExpiresAt  time.Time           `json:"lease_expires_at,omitempty"`
	ExecutorKind    string              `json:"executor_kind,omitempty"`
	StartedAt       time.Time           `json:"started_at,omitempty"`
	CompletedAt     time.Time           `json:"completed_at,omitempty"`
}

type OperationDTO struct {
	ID                domain.ID             `json:"id"`
	TaskID            domain.ID             `json:"task_id"`
	AttemptID         domain.ID             `json:"attempt_id"`
	EffectSlotID      domain.ID             `json:"effect_slot_id,omitempty"`
	State             domain.OperationState `json:"state"`
	IntentFingerprint string                `json:"intent_fingerprint,omitempty"`
	IntentRevision    int64                 `json:"intent_revision,omitempty"`
	Provider          string                `json:"provider,omitempty"`
	ProviderReference string                `json:"provider_reference,omitempty"`
	ReservationID     domain.ID             `json:"reservation_id,omitempty"`
	CanonicalIntent   json.RawMessage       `json:"canonical_intent,omitempty"`
	Attributes        map[string]any        `json:"attributes,omitempty"`
	CreatedAt         time.Time             `json:"created_at,omitempty"`
	DispatchedAt      time.Time             `json:"dispatched_at,omitempty"`
	SettledAt         time.Time             `json:"settled_at,omitempty"`
}

type ApprovalChallengeDTO struct {
	Challenge     string `json:"challenge"`
	RequestDigest string `json:"request_digest"`
	Action        string `json:"action"`
}

type ApprovalRequest struct {
	Challenge string `json:"challenge"`
	Signature string `json:"signature"`
}

type ApprovalDTO struct {
	ApprovalID        domain.ID            `json:"approval_id"`
	SubjectKind       string               `json:"subject_kind,omitempty"`
	SubjectID         domain.ID            `json:"subject_id,omitempty"`
	RequestDigest     string               `json:"request_digest,omitempty"`
	RequiredApprovers []domain.ID          `json:"required_approvers,omitempty"`
	RequestedBy       domain.ID            `json:"requested_by,omitempty"`
	State             domain.ApprovalState `json:"state,omitempty"`
	ExpiresAt         time.Time            `json:"expires_at,omitempty"`
	ApprovedBy        domain.ID            `json:"approved_by,omitempty"`
	Status            string               `json:"status"`
	// Provider, EffectSlotID, CanonicalIntent and Attributes describe the bound
	// operation so the Owner sees what the signed digest covers.
	Provider          string          `json:"provider,omitempty"`
	EffectSlotID      domain.ID       `json:"effect_slot_id,omitempty"`
	DescriptorType    string          `json:"descriptor_type,omitempty"`
	IntentFingerprint string          `json:"intent_fingerprint,omitempty"`
	IntentRevision    int64           `json:"intent_revision,omitempty"`
	CanonicalIntent   json.RawMessage `json:"canonical_intent,omitempty"`
	Attributes        map[string]any  `json:"attributes,omitempty"`
}

type StatusProvider interface {
	Status(context.Context) (StatusDTO, error)
}

type TaskService interface {
	CreateTask(context.Context, execution.TaskRequest) (domain.Task, error)
	Task(context.Context, domain.ID) (domain.Task, error)
}

type MissionService interface {
	CreateMission(context.Context, string) (domain.ID, error)
	ActiveMission(context.Context) (domain.ID, string, error)
}

type ApprovalService interface {
	Get(context.Context, domain.ID) (domain.ApprovalRequestRecord, error)
	Pending(context.Context) ([]domain.ApprovalRequestRecord, error)
	Approve(context.Context, domain.ID, domain.ID, string) error
	Reject(context.Context, domain.ID, domain.ID, string) error
}

type AttemptReader interface {
	Attempt(context.Context, domain.ID) (domain.Attempt, error)
}

type OperationReader interface {
	Operation(context.Context, domain.ID) (domain.ExternalOperation, error)
}

type ShutdownService interface {
	RequestShutdown(context.Context) error
}

type Dependencies struct {
	Status        StatusProvider
	Tasks         TaskService
	Missions      MissionService
	Approvals     ApprovalService
	Attempts      AttemptReader
	Operations    OperationReader
	Feedback      FeedbackService
	Sanitizer     FeedbackSanitizer
	FieldObserver FieldObserver
	Experience    ExperienceService
	Shutdown      ShutdownService
}

type ServerConfig struct {
	AuthToken        string
	OwnerPrincipalID domain.ID
	OwnerPublicKey   ed25519.PublicKey
	ChallengeTTL     time.Duration
	Now              func() time.Time
}

type challengeState struct {
	value   string
	digest  string
	action  string
	expires time.Time
}

type Server struct {
	config     ServerConfig
	deps       Dependencies
	handler    http.Handler
	httpServer *http.Server

	mu         sync.Mutex
	challenges map[domain.ID]challengeState
}

func NewServer(config ServerConfig, deps Dependencies) (*Server, error) {
	config.AuthToken = strings.TrimSpace(config.AuthToken)
	config.OwnerPrincipalID = domain.ID(strings.TrimSpace(string(config.OwnerPrincipalID)))
	if config.AuthToken == "" {
		return nil, errors.New("control auth token is required")
	}
	if config.OwnerPrincipalID == "" || len(config.OwnerPublicKey) != ed25519.PublicKeySize {
		return nil, errors.New("Owner principal id and Ed25519 public key are required")
	}
	if config.ChallengeTTL <= 0 {
		return nil, errors.New("positive approval challenge TTL is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if deps.Status == nil || deps.Tasks == nil || deps.Missions == nil || deps.Approvals == nil || deps.Attempts == nil || deps.Operations == nil || deps.Shutdown == nil {
		return nil, errors.New("all control dependencies are required")
	}

	s := &Server{config: config, deps: deps, challenges: make(map[domain.ID]challengeState)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("POST /missions/challenge", s.handleMissionChallenge)
	mux.HandleFunc("POST /missions", s.handleCreateMission)
	mux.HandleFunc("GET /missions/active", s.handleActiveMission)
	mux.HandleFunc("POST /tasks", s.handleCreateTask)
	mux.HandleFunc("GET /tasks/{id}", s.handleTask)
	mux.HandleFunc("GET /approvals", s.handleApprovals)
	mux.HandleFunc("GET /approvals/{id}", s.handleApprovalGet)
	mux.HandleFunc("GET /approvals/{id}/challenge", s.handleApprovalChallenge)
	mux.HandleFunc("POST /approvals/{id}/approve", s.handleApprovalApprove)
	mux.HandleFunc("POST /approvals/{id}/reject", s.handleApprovalReject)
	mux.HandleFunc("GET /inspect/attempts/{id}", s.handleAttempt)
	mux.HandleFunc("GET /inspect/operations/{id}", s.handleOperation)
	mux.HandleFunc("GET /feedback", s.handleFeedbackList)
	mux.HandleFunc("POST /feedback/candidates", s.handleFeedbackCandidateCreate)
	mux.HandleFunc("GET /feedback/{id}", s.handleFeedbackInspect)
	mux.HandleFunc("POST /feedback/{id}/emit", s.handleFeedbackEmit)
	mux.HandleFunc("POST /feedback/{id}/sanitize", s.handleFeedbackSanitize)
	mux.HandleFunc("POST /feedback/observations", s.handleFeedbackObserve)
	mux.HandleFunc("POST /feedback/scan", s.handleFeedbackScan)
	mux.HandleFunc("POST /experience/grants/requests", s.handleExperienceGrantRequest)
	mux.HandleFunc("POST /experience/grants/{id}/activate", s.handleExperienceGrantActivate)
	mux.HandleFunc("POST /experience/proposals", s.handleExperienceProposalCreate)
	mux.HandleFunc("POST /experience/outcomes", s.handleExperienceOutcomeCreate)
	mux.HandleFunc("POST /experience/proposals/{id}/evaluate", s.handleExperienceEvaluate)
	mux.HandleFunc("GET /experience", s.handleExperienceList)
	mux.HandleFunc("GET /experience/{id}", s.handleExperienceGet)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	s.handler = s.authenticate(mux)
	s.httpServer = &http.Server{Handler: s.handler}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("control listener is required")
	}
	err := s.httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Close(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, prefix) || !constantTimeEqual(strings.TrimSpace(strings.TrimPrefix(header, prefix)), s.config.AuthToken) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func ApprovalSigningMessage(challenge, requestDigest, action string) []byte {
	return []byte(
		"summa42-owner-approval-v2\n" +
			"challenge:" + challenge + "\n" +
			"request-digest:" + requestDigest + "\n" +
			"action:" + action + "\n",
	)
}

func MissionSigningMessage(challenge, requestDigest string) []byte {
	return []byte("summa42-owner-mission-create-v1\nchallenge:" + challenge + "\nrequest-digest:" + requestDigest + "\n")
}

func missionDigest(statement string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(statement)))
	return hex.EncodeToString(sum[:])
}

func (s *Server) handleMissionChallenge(w http.ResponseWriter, r *http.Request) {
	var request CreateMissionRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	statement := strings.TrimSpace(request.Statement)
	if statement == "" || len(statement) > 4096 || request.Challenge != "" || request.Signature != "" {
		writeError(w, http.StatusBadRequest, "valid Mission statement is required")
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "generate Mission challenge")
		return
	}
	challenge := base64.RawURLEncoding.EncodeToString(raw[:])
	digest := missionDigest(statement)
	s.mu.Lock()
	for key, state := range s.challenges {
		if state.action == "CREATE_MISSION" || s.config.Now().UTC().After(state.expires) {
			delete(s.challenges, key)
		}
	}
	s.challenges[domain.ID("mission:"+challenge)] = challengeState{value: challenge, digest: digest, action: "CREATE_MISSION", expires: s.config.Now().UTC().Add(s.config.ChallengeTTL)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, MissionChallengeDTO{Challenge: challenge, RequestDigest: digest})
}

func (s *Server) handleCreateMission(w http.ResponseWriter, r *http.Request) {
	var request CreateMissionRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	statement := strings.TrimSpace(request.Statement)
	if statement == "" || len(statement) > 4096 {
		writeError(w, http.StatusBadRequest, "valid Mission statement is required")
		return
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(request.Signature))
	if err != nil || len(signature) != ed25519.SignatureSize {
		writeError(w, http.StatusForbidden, "invalid Owner signature")
		return
	}
	s.mu.Lock()
	key := domain.ID("mission:" + request.Challenge)
	state, exists := s.challenges[key]
	if !exists || s.config.Now().UTC().After(state.expires) || state.action != "CREATE_MISSION" ||
		!constantTimeEqual(state.digest, missionDigest(statement)) ||
		!ed25519.Verify(s.config.OwnerPublicKey, MissionSigningMessage(state.value, state.digest), signature) {
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "invalid Mission challenge or Owner signature")
		return
	}
	delete(s.challenges, key)
	s.mu.Unlock()
	id, err := s.deps.Missions.CreateMission(r.Context(), statement)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, MissionDTO{ID: id, Statement: statement})
}

func (s *Server) handleActiveMission(w http.ResponseWriter, r *http.Request) {
	id, statement, err := s.deps.Missions.ActiveMission(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no active Mission")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, MissionDTO{ID: id, Statement: statement})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.deps.Status.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var request CreateTaskRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	task, err := s.deps.Tasks.CreateTask(r.Context(), execution.TaskRequest{
		Purpose: request.Purpose, TaskClass: request.TaskClass, AcceptanceCriteria: request.AcceptanceCriteria,
		RequiredCapabilities: request.RequiredCapabilities, RequiredEnforcement: request.RequiredEnforcement,
		AuthorityCeiling: request.AuthorityCeiling, ResourceEnvelopeID: request.ResourceEnvelopeID,
		Priority: request.Priority, EarliestStart: request.EarliestStart, Deadline: request.Deadline,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, taskDTO(task))
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	task, err := s.deps.Tasks.Task(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, taskDTO(task))
}

func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	records, err := s.deps.Approvals.Pending(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	result := make([]ApprovalDTO, 0, len(records))
	for _, record := range records {
		result = append(result, s.approvalDTO(r.Context(), record))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleApprovalGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	record, err := s.deps.Approvals.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.approvalDTO(r.Context(), record))
}

func (s *Server) handleApprovalChallenge(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	action := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("action")))
	if action != "APPROVE" && action != "REJECT" {
		writeError(w, http.StatusBadRequest, "action must be APPROVE or REJECT")
		return
	}
	record, err := s.deps.Approvals.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if record.State != domain.ApprovalPending || !record.ExpiresAt.After(s.config.Now().UTC()) {
		writeError(w, http.StatusConflict, "approval request is not live and pending")
		return
	}
	if !approvalRequires(record.RequiredApprovers, s.config.OwnerPrincipalID) {
		writeError(w, http.StatusForbidden, "Owner is not a required approver for this request")
		return
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "generate approval challenge")
		return
	}
	state := challengeState{
		value:   base64.RawURLEncoding.EncodeToString(raw[:]),
		digest:  record.RequestDigest,
		action:  action,
		expires: s.config.Now().UTC().Add(s.config.ChallengeTTL),
	}
	s.mu.Lock()
	s.challenges[id] = state
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, ApprovalChallengeDTO{
		Challenge: state.value, RequestDigest: state.digest, Action: state.action,
	})
}

func (s *Server) handleApprovalApprove(w http.ResponseWriter, r *http.Request) {
	s.handleApprovalDecision(w, r, "APPROVE")
}

func (s *Server) handleApprovalReject(w http.ResponseWriter, r *http.Request) {
	s.handleApprovalDecision(w, r, "REJECT")
}

func (s *Server) handleApprovalDecision(w http.ResponseWriter, r *http.Request, action string) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request ApprovalRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(request.Signature))
	if err != nil || len(signature) != ed25519.SignatureSize {
		writeError(w, http.StatusForbidden, "invalid Owner signature")
		return
	}

	s.mu.Lock()
	state, exists := s.challenges[id]
	if !exists || s.config.Now().UTC().After(state.expires) ||
		!constantTimeEqual(request.Challenge, state.value) ||
		state.action != action {
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "invalid or expired approval challenge")
		return
	}
	if !ed25519.Verify(
		s.config.OwnerPublicKey,
		ApprovalSigningMessage(state.value, state.digest, state.action),
		signature,
	) {
		s.mu.Unlock()
		writeError(w, http.StatusForbidden, "invalid Owner signature")
		return
	}
	delete(s.challenges, id)
	s.mu.Unlock()

	switch action {
	case "APPROVE":
		err = s.deps.Approvals.Approve(r.Context(), id, s.config.OwnerPrincipalID, state.digest)
	case "REJECT":
		err = s.deps.Approvals.Reject(r.Context(), id, s.config.OwnerPrincipalID, state.digest)
	default:
		err = errors.New("unsupported approval action")
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	record, err := s.deps.Approvals.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.approvalDTO(r.Context(), record))
}

func (s *Server) handleAttempt(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	attempt, err := s.deps.Attempts.Attempt(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, attemptDTO(attempt))
}

func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	operation, err := s.deps.Operations.Operation(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, operationDTO(operation))
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Shutdown.RequestShutdown(r.Context()); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "SHUTDOWN_REQUESTED"})
}

func pathID(w http.ResponseWriter, r *http.Request) (domain.ID, bool) {
	id := domain.ID(strings.TrimSpace(r.PathValue("id")))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return "", false
	}
	return id, true
}

func decodeBody(r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func taskDTO(task domain.Task) TaskDTO {
	return TaskDTO{
		ID: task.ID, ParentTaskID: task.ParentTaskID, Purpose: task.Purpose, TaskClass: task.TaskClass, State: task.State,
		CurrentAttemptID: task.CurrentAttemptID, CurrentFence: task.CurrentFence,
		AcceptanceCriteria:   append([]string(nil), task.AcceptanceCriteria...),
		RequiredCapabilities: append([]string(nil), task.RequiredCapabilities...), RequiredEnforcement: task.RequiredEnforcement,
		AuthorityCeiling: append([]string(nil), task.AuthorityCeiling...), ResourceEnvelopeID: task.ResourceEnvelopeID,
		Priority: task.Priority, EarliestStart: task.EarliestStart, Deadline: task.Deadline,
		CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt,
	}
}

func attemptDTO(attempt domain.Attempt) AttemptDTO {
	return AttemptDTO{
		ID: attempt.ID, TaskID: attempt.TaskID, State: attempt.State, FenceGeneration: attempt.FenceGeneration,
		LeaseState: attempt.LeaseState, LeaseExpiresAt: attempt.LeaseExpiresAt, ExecutorKind: attempt.ExecutorKind,
		StartedAt: attempt.StartedAt, CompletedAt: attempt.CompletedAt,
	}
}

func operationDTO(operation domain.ExternalOperation) OperationDTO {
	return OperationDTO{
		ID: operation.ID, TaskID: operation.TaskID, AttemptID: operation.AttemptID, EffectSlotID: operation.EffectSlotID,
		State: operation.State, IntentFingerprint: operation.IntentFingerprint, IntentRevision: operation.IntentRevision,
		Provider: operation.Provider, ProviderReference: operation.ProviderReference, ReservationID: operation.ReservationID,
		CreatedAt: operation.CreatedAt, DispatchedAt: operation.DispatchedAt, SettledAt: operation.SettledAt,
		CanonicalIntent: json.RawMessage(operation.CanonicalIntent), Attributes: operation.Attributes,
	}
}

func (s *Server) approvalDTO(ctx context.Context, record domain.ApprovalRequestRecord) ApprovalDTO {
	dto := ApprovalDTO{
		ApprovalID: record.ID, SubjectKind: record.SubjectKind, SubjectID: record.SubjectID,
		RequestDigest: record.RequestDigest, RequiredApprovers: append([]domain.ID(nil), record.RequiredApprovers...),
		RequestedBy: record.RequestedBy, State: record.State, ExpiresAt: record.ExpiresAt,
		ApprovedBy: record.ApproverID, Status: string(record.State),
	}
	if record.SubjectKind == "EXTERNAL_OPERATION" {
		operation, err := s.deps.Operations.Operation(ctx, record.SubjectID)
		if err != nil {
			log.Printf("control: load operation %s for approval %s: %v", record.SubjectID, record.ID, err)
		} else {
			dto.Provider, dto.EffectSlotID = operation.Provider, operation.EffectSlotID
			dto.DescriptorType, dto.IntentFingerprint, dto.IntentRevision = operation.DescriptorType, operation.IntentFingerprint, operation.IntentRevision
			dto.CanonicalIntent, dto.Attributes = json.RawMessage(operation.CanonicalIntent), operation.Attributes
		}
	}
	return dto
}

func approvalRequires(required []domain.ID, principal domain.ID) bool {
	for _, candidate := range required {
		if candidate == principal {
			return true
		}
	}
	return false
}
