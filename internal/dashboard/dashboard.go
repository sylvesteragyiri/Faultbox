// Package dashboard serves the server-rendered web UI (Go templates + HTMX).
package dashboard

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/web"
)

// Options configures the dashboard.
type Options struct {
	Store         *store.Store
	Logger        *slog.Logger
	CSRFKey       []byte
	SessionTTL    time.Duration
	TrustProxy    bool
	Version       string
	RetentionDays int
	// PublicURL is the address Laravel apps report to, shown in setup snippets.
	PublicURL string
}

// Handler is the dashboard's root handler.
type Handler struct {
	opts      Options
	store     *store.Store
	logger    *slog.Logger
	pages     map[string]*template.Template
	handler   http.Handler
	loginByIP *auth.FailureLimiter
	loginByID *auth.FailureLimiter
}

// Login limits: 10 failures per IP and 5 per email in 15 minutes (D9).
const (
	loginFailuresPerIP    = 10
	loginFailuresPerEmail = 5
	loginFailureWindow    = 15 * time.Minute
)

// New builds the dashboard handler.
func New(opts Options) (*Handler, error) {
	h := &Handler{
		opts:      opts,
		store:     opts.Store,
		logger:    opts.Logger,
		loginByIP: auth.NewFailureLimiter(loginFailuresPerIP, loginFailureWindow),
		loginByID: auth.NewFailureLimiter(loginFailuresPerEmail, loginFailureWindow),
	}
	if err := h.parseTemplates(); err != nil {
		return nil, err
	}

	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))

	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.login)

	authed := http.NewServeMux()
	authed.HandleFunc("POST /logout", h.logout)
	authed.HandleFunc("GET /{$}", h.overview)
	authed.HandleFunc("GET /projects", h.projectList)
	authed.HandleFunc("GET /projects/new", h.projectNew)
	authed.HandleFunc("POST /projects", h.projectCreate)
	authed.HandleFunc("GET /projects/{id}", h.projectShow)
	authed.HandleFunc("POST /projects/{id}", h.projectUpdate)
	authed.HandleFunc("POST /projects/{id}/status", h.projectSetStatus)
	authed.HandleFunc("POST /projects/{id}/key", h.projectRotateKey)
	authed.HandleFunc("GET /issues", h.comingSoon("issues", "Issues", "M4",
		"Grouped exceptions from your Laravel apps will appear here once the ingestion pipeline (M2) and issue views (M4) land."))
	authed.HandleFunc("GET /queue-failures", h.comingSoon("queue", "Queue Failures", "M5",
		"Failed jobs that exhaust their retries will be listed here, separately from request exceptions."))
	authed.HandleFunc("GET /notifications", h.comingSoon("notifications", "Notifications", "M6",
		"Email and webhook alerts for new issues, regressions and failed jobs."))
	authed.HandleFunc("GET /settings", h.settings)
	authed.HandleFunc("/", h.notFound)
	mux.Handle("/", h.requireUser(authed))

	cop := http.NewCrossOriginProtection()
	h.handler = cop.Handler(h.withCSRF(h.withSession(mux)))
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// ---- templates ----

var pageNames = []string{
	"login", "overview", "projects", "project_new", "project", "project_key",
	"coming_soon", "settings", "error",
}

func (h *Handler) parseTemplates() error {
	funcs := template.FuncMap{
		"timeago": timeAgo,
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"isoTime": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
		"join":    strings.Join,
		"bytes":   humanBytes,
	}
	h.pages = make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(funcs).ParseFS(web.FS,
			"templates/layout.html", "templates/partials/*.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("parse template %s: %w", name, err)
		}
		h.pages[name] = t
	}
	return nil
}

// page is the data every template receives.
type page struct {
	Title   string
	Nav     string
	User    *store.User
	CSRF    string
	Version string
	Flash   string
	Data    any
}

func (h *Handler) newPage(r *http.Request, title, nav string, data any) page {
	p := page{Title: title, Nav: nav, CSRF: csrfFrom(r.Context()), Version: h.opts.Version, Data: data}
	if u, ok := userFrom(r.Context()); ok {
		p.User = &u
	}
	return p
}

// render executes a page, or one named block of it for HTMX swaps.
func (h *Handler) render(w http.ResponseWriter, status int, name, block string, p page) {
	t, ok := h.pages[name]
	if !ok {
		h.serverError(w, fmt.Errorf("unknown template %q", name))
		return
	}
	if block == "" {
		block = "layout"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, p); err != nil {
		h.serverError(w, fmt.Errorf("render %s/%s: %w", name, block, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Pages can show one-time API keys, so nothing may be cached (D10).
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (h *Handler) serverError(w http.ResponseWriter, err error) {
	h.logger.Error("dashboard error", "error", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, status int, message string) {
	h.render(w, status, "error", "", h.newPage(r, http.StatusText(status), "", map[string]any{
		"Status": status, "Message": message,
	}))
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.errorPage(w, r, http.StatusNotFound, "There's nothing at this address.")
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// ---- pages that are mostly read-only ----

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	active, disabled, err := h.store.CountProjects(r.Context())
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "overview", "", h.newPage(r, "Overview", "overview", map[string]any{
		"ActiveProjects":   active,
		"DisabledProjects": disabled,
	}))
}

func (h *Handler) comingSoon(nav, title, milestone, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.render(w, http.StatusOK, "coming_soon", "", h.newPage(r, title, nav, map[string]any{
			"Milestone": milestone, "Text": text,
		}))
	}
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	version, err := h.store.SchemaVersion(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	users, err := h.store.CountUsers(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "settings", "", h.newPage(r, "Settings", "settings", map[string]any{
		"SchemaVersion": version,
		"DatabasePath":  h.store.Path(),
		"DatabaseSize":  databaseSize(h.store.Path()),
		"RetentionDays": h.opts.RetentionDays,
		"Users":         users,
	}))
}

// ---- small helpers ----

// databaseSize sums the database file and its WAL.
func databaseSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	return total
}

func timeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	default:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
	csrfKey
)

func userFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey).(store.User)
	return u, ok
}

func csrfFrom(ctx context.Context) string {
	s, _ := ctx.Value(csrfKey).(string)
	return s
}

// clientIP is the rate-limit key for login attempts.
func (h *Handler) clientIP(r *http.Request) string {
	return httpx.ClientIP(r, h.opts.TrustProxy)
}
