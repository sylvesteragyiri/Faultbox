package dashboard

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// projectForm is the create/edit form state, re-rendered on validation errors.
type projectForm struct {
	Name         string
	Environments string
	Error        string
}

func (h *Handler) projectList(w http.ResponseWriter, r *http.Request) {
	projects, err := h.store.Projects(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "projects", "", h.newPage(r, "Projects", "projects", projects))
}

func (h *Handler) projectNew(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "project_new", "", h.newPage(r, "New project", "projects",
		projectForm{Environments: "production, staging"}))
}

func parseProjectForm(r *http.Request) (projectForm, string, []string, error) {
	form := projectForm{Name: r.PostFormValue("name"), Environments: r.PostFormValue("environments")}
	name, err := store.NormalizeProjectName(form.Name)
	if err != nil {
		return form, "", nil, err
	}
	envs, err := store.NormalizeEnvironments(form.Environments)
	if err != nil {
		return form, "", nil, err
	}
	return form, name, envs, nil
}

func validationMessage(err error) (string, bool) {
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		return ve.Msg, true
	}
	return "", false
}

func (h *Handler) projectCreate(w http.ResponseWriter, r *http.Request) {
	form, name, envs, err := parseProjectForm(r)
	if msg, ok := validationMessage(err); ok {
		form.Error = msg
		h.render(w, http.StatusUnprocessableEntity, "project_new", "", h.newPage(r, "New project", "projects", form))
		return
	}

	key, hash, hint, err := auth.NewAPIKey()
	if err != nil {
		h.serverError(w, err)
		return
	}
	project, err := h.store.CreateProject(r.Context(), name, envs, hash, hint)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.logger.Info("project created", "project_id", project.ID, "user_id", h.userID(r))
	h.renderKey(w, r, project, key, true)
}

// renderKey shows a freshly generated API key. It is the only time the full
// key is ever displayed (D10).
func (h *Handler) renderKey(w http.ResponseWriter, r *http.Request, p store.Project, key string, created bool) {
	data := map[string]any{
		"Project": p,
		"Key":     key,
		"Created": created,
		"DSN":     h.dsnHint(r),
	}
	if isHTMX(r) {
		h.render(w, http.StatusOK, "project_key", "key_panel", h.newPage(r, p.Name, "projects", data))
		return
	}
	h.render(w, http.StatusOK, "project_key", "", h.newPage(r, p.Name, "projects", data))
}

// dsnHint guesses the URL Laravel apps should report to, for the setup snippet.
func (h *Handler) dsnHint(r *http.Request) string {
	if h.opts.PublicURL != "" {
		return h.opts.PublicURL
	}
	scheme := "http"
	if h.secureCookies(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (h *Handler) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		h.notFound(w, r)
		return store.Project{}, false
	}
	p, err := h.store.Project(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r)
		return store.Project{}, false
	}
	if err != nil {
		h.serverError(w, err)
		return store.Project{}, false
	}
	return p, true
}

func (h *Handler) projectShow(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProject(w, r)
	if !ok {
		return
	}
	h.renderProject(w, r, http.StatusOK, p, projectForm{
		Name:         p.Name,
		Environments: strings.Join(p.Environments, ", "),
	}, r.URL.Query().Get("saved") == "1")
}

func (h *Handler) renderProject(w http.ResponseWriter, r *http.Request, status int, p store.Project, form projectForm, saved bool) {
	pg := h.newPage(r, p.Name, "projects", map[string]any{"Project": p, "Form": form})
	if saved {
		pg.Flash = "Project saved."
	}
	h.render(w, status, "project", "", pg)
}

func (h *Handler) projectUpdate(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProject(w, r)
	if !ok {
		return
	}
	form, name, envs, err := parseProjectForm(r)
	if msg, ok := validationMessage(err); ok {
		form.Error = msg
		h.renderProject(w, r, http.StatusUnprocessableEntity, p, form, false)
		return
	}
	if err := h.store.UpdateProject(r.Context(), p.ID, name, envs); err != nil {
		h.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"?saved=1", http.StatusSeeOther)
}

func (h *Handler) projectSetStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProject(w, r)
	if !ok {
		return
	}
	status := r.PostFormValue("status")
	if status != store.ProjectActive && status != store.ProjectDisabled {
		h.errorPage(w, r, http.StatusBadRequest, "Unknown project status.")
		return
	}
	if err := h.store.SetProjectStatus(r.Context(), p.ID, status); err != nil {
		h.serverError(w, err)
		return
	}
	h.logger.Info("project status changed", "project_id", p.ID, "status", status, "user_id", h.userID(r))
	if !isHTMX(r) {
		http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10), http.StatusSeeOther)
		return
	}
	p.Status = status
	h.render(w, http.StatusOK, "project", "status_panel", h.newPage(r, p.Name, "projects", map[string]any{"Project": p}))
}

func (h *Handler) projectRotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := h.loadProject(w, r)
	if !ok {
		return
	}
	key, hash, hint, err := auth.NewAPIKey()
	if err != nil {
		h.serverError(w, err)
		return
	}
	if err := h.store.RotateProjectKey(r.Context(), p.ID, hash, hint); err != nil {
		h.serverError(w, err)
		return
	}
	h.logger.Info("project api key regenerated", "project_id", p.ID, "user_id", h.userID(r))
	p.APIKeyHint = hint
	h.renderKey(w, r, p, key, false)
}

func (h *Handler) userID(r *http.Request) int64 {
	u, _ := userFrom(r.Context())
	return u.ID
}
