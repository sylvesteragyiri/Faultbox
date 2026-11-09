package dashboard

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Scope is the project and environment picked in the switcher at the top
// left. It is the default filter for the overview and the lists, so the
// dashboard shows one app at a time. Zero values mean "all".
type Scope struct {
	ProjectID int64
	Env       string
}

// Project returns the scoped project ID as a form value ("" for all).
func (s Scope) Project() string {
	if s.ProjectID == 0 {
		return ""
	}
	return strconv.FormatInt(s.ProjectID, 10)
}

// scopeView is what the switcher renders.
type scopeView struct {
	Scope
	Projects []store.Project
	Envs     []string
	Next     string // the current page, to return to after switching
}

const scopeCookie = "fb_scope"

var scopeEnvRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// scopeFrom reads the scope cookie. A malformed cookie means "all".
func scopeFrom(r *http.Request) Scope {
	c, err := r.Cookie(scopeCookie)
	if err != nil {
		return Scope{}
	}
	project, env, _ := strings.Cut(c.Value, ":")
	var s Scope
	if id, err := strconv.ParseInt(project, 10, 64); err == nil && id > 0 {
		s.ProjectID = id
	}
	if scopeEnvRe.MatchString(env) {
		s.Env = env
	}
	return s
}

// scopeView loads the switcher's choices. A scope pointing at a deleted
// project falls back to "all projects".
func (h *Handler) scopeView(r *http.Request) scopeView {
	v := scopeView{Scope: scopeFrom(r), Next: r.URL.RequestURI()}
	projects, err := h.store.Projects(r.Context())
	if err != nil {
		h.logger.Error("load projects for the scope switcher", "error", err)
		return v
	}
	v.Projects = projects
	found := v.ProjectID == 0
	for _, p := range projects {
		if p.ID == v.ProjectID {
			found = true
		}
	}
	if !found {
		v.ProjectID = 0
	}
	envs, err := h.store.KnownEnvironments(r.Context(), v.ProjectID)
	if err != nil {
		h.logger.Error("load environments for the scope switcher", "error", err)
		return v
	}
	v.Envs = envs
	if v.Env != "" && !slices.Contains(v.Envs, v.Env) {
		v.Envs = append(v.Envs, v.Env) // keep the current choice visible
	}
	return v
}

// setScope saves the switcher's choice and returns to the page it came from,
// without that page's own project and environment filters, so the new scope
// takes effect.
func (h *Handler) setScope(w http.ResponseWriter, r *http.Request) {
	var s Scope
	if id, err := strconv.ParseInt(r.PostFormValue("project"), 10, 64); err == nil && id > 0 {
		if _, err := h.store.Project(r.Context(), id); err == nil {
			s.ProjectID = id
		}
	}
	if env := r.PostFormValue("env"); scopeEnvRe.MatchString(env) {
		s.Env = env
	}
	// Switching project resets the environment unless it exists there too.
	if s.Env != "" {
		if envs, err := h.store.KnownEnvironments(r.Context(), s.ProjectID); err == nil && !slices.Contains(envs, s.Env) {
			s.Env = ""
		}
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is conditional on HTTPS (secureCookies)
		Name:     scopeCookie,
		Value:    strconv.FormatInt(s.ProjectID, 10) + ":" + s.Env,
		Path:     "/",
		MaxAge:   int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookies(r), // honours FAULTBOX_TRUST_PROXY, like the session cookie
		SameSite: http.SameSiteLaxMode,
	})
	next := scopeReturnPath(r.PostFormValue("next"))
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", next)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: scopeReturnPath only returns local paths
}

// scopeReturnPath turns the page the switcher was on into a local path
// without project, env or page parameters. Anything that isn't a local
// path goes to the overview.
func scopeReturnPath(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") ||
		strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "\\") {
		return "/"
	}
	// Detail pages belong to one project; after switching, go to the list.
	switch {
	case strings.HasPrefix(u.Path, "/issues/"):
		return "/issues"
	case strings.HasPrefix(u.Path, "/queue-failures/") && u.Path != "/queue-failures/jobs":
		return "/queue-failures"
	}
	q := u.Query()
	q.Del("project")
	q.Del("env")
	q.Del("page")
	out := url.URL{Path: u.Path, RawQuery: q.Encode()}
	return out.String()
}
