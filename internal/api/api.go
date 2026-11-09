// Package api serves the versioned ingestion API under /api/v1.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Handler serves /api/v1/*.
type Handler struct {
	store  *store.Store
	logger *slog.Logger
	mux    *http.ServeMux
}

// New builds the API handler.
func New(st *store.Store, logger *slog.Logger) *Handler {
	h := &Handler{store: st, logger: logger, mux: http.NewServeMux()}
	h.mux.Handle("GET /api/v1/projects/me", h.requireProject(http.HandlerFunc(h.projectMe)))
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpx.JSONMuxErrors(h.mux).ServeHTTP(w, r)
}

type ctxKey struct{}

// ProjectFrom returns the project authenticated for this request.
func ProjectFrom(ctx context.Context) (store.Project, bool) {
	p, ok := ctx.Value(ctxKey{}).(store.Project)
	return p, ok
}

// KeyFromRequest reads the key from "Authorization: Bearer <key>" or
// the "X-Faultbox-Key" header.
func KeyFromRequest(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		scheme, token, ok := strings.Cut(v, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Faultbox-Key"))
}

// requireProject authenticates the project API key on every API route (PRD §4.1).
func (h *Handler) requireProject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := KeyFromRequest(r)
		if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="faultbox"`)
			httpx.WriteError(w, http.StatusUnauthorized, "missing_api_key",
				"Send the project API key as 'Authorization: Bearer <key>'.")
			return
		}
		if !auth.LooksLikeAPIKey(key) {
			h.unauthorized(w)
			return
		}
		project, err := h.store.ProjectByAPIKeyHash(r.Context(), auth.HashAPIKey(key))
		switch {
		case errors.Is(err, store.ErrNotFound):
			h.unauthorized(w)
			return
		case err != nil:
			h.logger.Error("look up api key", "error", err)
			httpx.WriteError(w, http.StatusServiceUnavailable, "unavailable", "The server cannot check API keys right now.")
			return
		case !project.Active():
			httpx.WriteError(w, http.StatusForbidden, "project_disabled", "This project is disabled.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, project)))
	})
}

func (h *Handler) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="faultbox", error="invalid_token"`)
	httpx.WriteError(w, http.StatusUnauthorized, "invalid_api_key", "The API key is not valid.")
}

// ProjectResponse is the body of GET /api/v1/projects/me.
type ProjectResponse struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Slug         string   `json:"slug"`
	Status       string   `json:"status"`
	Environments []string `json:"environments"`
	CreatedAt    string   `json:"created_at"`
}

func (h *Handler) projectMe(w http.ResponseWriter, r *http.Request) {
	p, _ := ProjectFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, ProjectResponse{
		ID:           p.ID,
		Name:         p.Name,
		Slug:         p.Slug,
		Status:       p.Status,
		Environments: p.Environments,
		CreatedAt:    p.CreatedAt.Format(time.RFC3339),
	})
}
