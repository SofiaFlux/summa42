package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SofiaFlux/summa42/internal/identity"
	"github.com/SofiaFlux/summa42/internal/project"
)

// ProjectService exposes kernel Project scope operations over control.
type ProjectService interface {
	CreateSigned(context.Context, project.CreateCommand) (project.Project, error)
	Get(context.Context, string) (project.Project, error)
	List(context.Context) ([]project.Project, error)
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
	proj, err := s.deps.Projects.Get(r.Context(), strings.TrimSpace(r.PathValue("ref")))
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
		switch {
		case errors.Is(err, project.ErrUnauthorized), errors.Is(err, project.ErrExpired), errors.Is(err, project.ErrReplayed):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, project.ErrProjectExists):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, proj)
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
