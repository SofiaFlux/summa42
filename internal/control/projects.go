package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/domain"
	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/project"
	"github.com/SofiaFlux/summa42/internal/purpose"
	"github.com/SofiaFlux/summa42/internal/resources"
)

// EnvelopeService applies Owner-signed resource envelope grants.
type EnvelopeService interface {
	ApplyOwnerGrant(context.Context, resources.OwnerGrant) (resources.Envelope, error)
}

// ProjectService exposes kernel Project scope operations over control.
type ProjectService interface {
	CreateSigned(context.Context, project.CreateCommand) (project.Project, error)
	ApplyManifestSigned(context.Context, project.ApplyCommand) (project.Project, error)
	TransitionSigned(context.Context, project.TransitionCommand) (project.Project, error)
	FundSigned(context.Context, project.EnvelopeApplier, string, resources.OwnerGrant) (resources.Envelope, error)
	Get(context.Context, string) (project.Project, error)
	List(context.Context) ([]project.Project, error)
}

func writeProjectError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, project.ErrUnauthorized), errors.Is(err, project.ErrExpired), errors.Is(err, project.ErrReplayed),
		errors.Is(err, resources.ErrGrantUnauthorized), errors.Is(err, resources.ErrGrantExpired), errors.Is(err, resources.ErrGrantReplayed):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, project.ErrProjectExists), errors.Is(err, domain.ErrProjectPaused), errors.Is(err, domain.ErrProjectArchived):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, project.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// MissionManager extends MissionService with standby creation, listing, and
// Owner-signed switching across stored Collective Missions.
type MissionManager interface {
	CreateStandbyMission(context.Context, string) (domain.ID, error)
	ListMissions(context.Context) ([]purpose.Mission, error)
	SwitchMission(context.Context, domain.ID) error
}

// SwitchMissionRequest requests activating a stored Mission with an Owner signature.
type SwitchMissionRequest struct {
	MissionID domain.ID `json:"mission_id"`
	Challenge string    `json:"challenge,omitempty"`
	Signature string    `json:"signature,omitempty"`
}

func MissionSwitchSigningMessage(challenge, requestDigest string) []byte {
	return []byte("summa42-owner-mission-switch-v1\nchallenge:" + challenge + "\nrequest-digest:" + requestDigest + "\n")
}

func (s *Server) issueOwnerChallenge(action, digest string) (MissionChallengeDTO, bool) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return MissionChallengeDTO{}, false
	}
	challenge := base64.RawURLEncoding.EncodeToString(raw[:])
	s.mu.Lock()
	for key, st := range s.challenges {
		if st.action == action || s.config.Now().UTC().After(st.expires) {
			delete(s.challenges, key)
		}
	}
	s.challenges[domain.ID(action+":"+challenge)] = challengeState{
		value: challenge, digest: digest, action: action,
		expires: s.config.Now().UTC().Add(s.config.ChallengeTTL),
	}
	s.mu.Unlock()
	return MissionChallengeDTO{Challenge: challenge, RequestDigest: digest}, true
}

func (s *Server) verifyOwnerChallenge(action, challenge, digest, signatureB64 string, msgFn func(string, string) []byte) bool {
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signatureB64))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := domain.ID(action + ":" + challenge)
	st, exists := s.challenges[key]
	if !exists || s.config.Now().UTC().After(st.expires) || st.action != action ||
		!constantTimeEqual(st.digest, digest) ||
		!ed25519.Verify(s.config.OwnerPublicKey, msgFn(st.value, st.digest), signature) {
		return false
	}
	delete(s.challenges, key)
	return true
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if s.deps.Projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project service unavailable")
		return
	}
	projects, err := s.deps.Projects.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	if s.deps.Projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project service unavailable")
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	proj, err := s.deps.Projects.Get(r.Context(), ref)
	if err != nil {
		if errors.Is(err, project.ErrProjectNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if s.deps.Projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project service unavailable")
		return
	}
	var cmd project.CreateCommand
	if err := decodeBody(r, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proj, err := s.deps.Projects.CreateSigned(r.Context(), cmd)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, proj)
}

func (s *Server) handleApplyProjectManifest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Projects == nil {
		writeError(w, http.StatusServiceUnavailable, "project service unavailable")
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	var cmd project.ApplyCommand
	if err := decodeBody(r, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if ref != "" && cmd.Slug != ref && string(cmd.ProjectID) != ref {
		writeError(w, http.StatusBadRequest, "project ref in path does not match apply command")
		return
	}
	proj, err := s.deps.Projects.ApplyManifestSigned(r.Context(), cmd)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proj)
}

func (s *Server) handleTransitionProject(action project.TransitionAction) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Projects == nil {
			writeError(w, http.StatusServiceUnavailable, "project service unavailable")
			return
		}
		ref := strings.TrimSpace(r.PathValue("ref"))
		var cmd project.TransitionCommand
		if err := decodeBody(r, &cmd); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if cmd.Action != action || (ref != "" && cmd.Slug != ref && string(cmd.ProjectID) != ref) {
			writeError(w, http.StatusBadRequest, "project transition command does not match route")
			return
		}
		proj, err := s.deps.Projects.TransitionSigned(r.Context(), cmd)
		if err != nil {
			writeProjectError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, proj)
	}
}

func (s *Server) handleFundProject(w http.ResponseWriter, r *http.Request) {
	if s.deps.Projects == nil || s.deps.Envelopes == nil {
		writeError(w, http.StatusServiceUnavailable, "project or resource service unavailable")
		return
	}
	ref := strings.TrimSpace(r.PathValue("ref"))
	var grant resources.OwnerGrant
	if err := decodeBody(r, &grant); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	env, err := s.deps.Projects.FundSigned(r.Context(), s.deps.Envelopes, ref, grant)
	if err != nil {
		writeProjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) handleListMissions(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.deps.Missions.(MissionManager)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission listing unavailable")
		return
	}
	missions, err := mgr.ListMissions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, missions)
}

func (s *Server) handleMissionSwitchChallenge(w http.ResponseWriter, r *http.Request) {
	var request SwitchMissionRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(string(request.MissionID))
	if id == "" || request.Challenge != "" || request.Signature != "" {
		writeError(w, http.StatusBadRequest, "valid Mission id is required")
		return
	}
	sum := sha256.Sum256([]byte(id))
	challenge, ok := s.issueOwnerChallenge("SWITCH_MISSION", hex.EncodeToString(sum[:]))
	if !ok {
		writeError(w, http.StatusInternalServerError, "generate Mission switch challenge")
		return
	}
	writeJSON(w, http.StatusOK, challenge)
}

func (s *Server) handleSwitchMission(w http.ResponseWriter, r *http.Request) {
	mgr, ok := s.deps.Missions.(MissionManager)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "mission switching unavailable")
		return
	}
	var request SwitchMissionRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := domain.ID(strings.TrimSpace(string(request.MissionID)))
	if id == "" {
		writeError(w, http.StatusBadRequest, "valid Mission id is required")
		return
	}
	sum := sha256.Sum256([]byte(id))
	if !s.verifyOwnerChallenge("SWITCH_MISSION", request.Challenge, hex.EncodeToString(sum[:]), request.Signature, MissionSwitchSigningMessage) {
		writeError(w, http.StatusForbidden, "invalid Mission switch challenge or Owner signature")
		return
	}
	if err := mgr.SwitchMission(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	activeID, stmt, err := s.deps.Missions.ActiveMission(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, MissionDTO{ID: activeID, Statement: stmt})
}

func (c *Client) Projects(ctx context.Context) ([]project.Project, error) {
	var result []project.Project
	err := c.doJSON(ctx, http.MethodGet, "/projects", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) Project(ctx context.Context, ref string) (project.Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = project.DefaultProjectSlug
	}
	var result project.Project
	err := c.doJSON(ctx, http.MethodGet, "/projects/"+url.PathEscape(ref), nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) CreateProject(ctx context.Context, slug, name, reason string, signer identity.Signer) (project.Project, error) {
	if signer == nil {
		return project.Project{}, errors.New("Owner signer is required")
	}
	status, err := c.Status(ctx)
	if err != nil {
		return project.Project{}, fmt.Errorf("read Collective identity: %w", err)
	}
	cmd, err := project.NewCreateCommand(signer, status.CollectiveID, slug, name, reason, time.Now().UTC())
	if err != nil {
		return project.Project{}, err
	}
	var result project.Project
	err = c.doJSON(ctx, http.MethodPost, "/projects", cmd, &result, http.StatusCreated)
	return result, err
}

func (c *Client) ApplyProjectManifest(ctx context.Context, ref string, cmd project.ApplyCommand) (project.Project, error) {
	var result project.Project
	err := c.doJSON(ctx, http.MethodPost, "/projects/"+url.PathEscape(strings.TrimSpace(ref))+"/apply", cmd, &result, http.StatusOK)
	return result, err
}

func (c *Client) TransitionProject(ctx context.Context, ref string, action project.TransitionAction, reason string, signer identity.Signer) (project.Project, error) {
	if signer == nil {
		return project.Project{}, errors.New("Owner signer is required")
	}
	proj, err := c.Project(ctx, ref)
	if err != nil {
		return project.Project{}, err
	}
	status, err := c.Status(ctx)
	if err != nil {
		return project.Project{}, fmt.Errorf("read Collective identity: %w", err)
	}
	cmd, err := project.NewTransitionCommand(signer, status.CollectiveID, proj.ID, proj.Slug, action, reason, time.Now().UTC())
	if err != nil {
		return project.Project{}, err
	}
	var result project.Project
	err = c.doJSON(ctx, http.MethodPost, "/projects/"+url.PathEscape(proj.Slug)+"/"+strings.ToLower(string(action)), cmd, &result, http.StatusOK)
	return result, err
}

func (c *Client) FundProject(ctx context.Context, ref string, limit int64, reason string, signer identity.Signer) (resources.Envelope, error) {
	if signer == nil {
		return resources.Envelope{}, errors.New("Owner signer is required")
	}
	proj, err := c.Project(ctx, ref)
	if err != nil {
		return resources.Envelope{}, err
	}
	status, err := c.Status(ctx)
	if err != nil {
		return resources.Envelope{}, fmt.Errorf("read Collective identity: %w", err)
	}
	if strings.TrimSpace(reason) == "" {
		reason = fmt.Sprintf("fund project %s limit %d", proj.Slug, limit)
	}
	grant, err := resources.NewOwnerGrant(signer, status.CollectiveID, resources.GrantSetLimit, proj.EnvelopeID, limit, reason, time.Now().UTC())
	if err != nil {
		return resources.Envelope{}, err
	}
	var result resources.Envelope
	err = c.doJSON(ctx, http.MethodPost, "/projects/"+url.PathEscape(proj.Slug)+"/fund", grant, &result, http.StatusOK)
	return result, err
}

func (c *Client) Missions(ctx context.Context) ([]purpose.Mission, error) {
	var result []purpose.Mission
	err := c.doJSON(ctx, http.MethodGet, "/missions", nil, &result, http.StatusOK)
	return result, err
}

func (c *Client) CreateStandbyMission(ctx context.Context, statement string, signer identity.Signer) (MissionDTO, error) {
	return c.createMission(ctx, statement, true, signer)
}

func (c *Client) SwitchMission(ctx context.Context, missionID domain.ID, signer identity.Signer) (MissionDTO, error) {
	if signer == nil {
		return MissionDTO{}, errors.New("Owner signer is required")
	}
	missionID = domain.ID(strings.TrimSpace(string(missionID)))
	sum := sha256.Sum256([]byte(missionID))
	digest := hex.EncodeToString(sum[:])
	var challenge MissionChallengeDTO
	if err := c.doJSON(ctx, http.MethodPost, "/missions/switch/challenge", SwitchMissionRequest{MissionID: missionID}, &challenge, http.StatusOK); err != nil {
		return MissionDTO{}, err
	}
	if challenge.RequestDigest != digest || challenge.Challenge == "" {
		return MissionDTO{}, errors.New("Mission switch challenge does not match mission id")
	}
	signature, err := signer.Sign(MissionSwitchSigningMessage(challenge.Challenge, challenge.RequestDigest))
	if err != nil {
		return MissionDTO{}, fmt.Errorf("sign Mission switch challenge: %w", err)
	}
	request := SwitchMissionRequest{MissionID: missionID, Challenge: challenge.Challenge, Signature: base64.StdEncoding.EncodeToString(signature)}
	var result MissionDTO
	err = c.doJSON(ctx, http.MethodPost, "/missions/switch", request, &result, http.StatusOK)
	return result, err
}
