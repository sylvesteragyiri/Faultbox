// Package dashboard serves the server-rendered web UI (Go templates + HTMX).
package dashboard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
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
	// Metrics reports ingestion counters for the Settings page.
	Metrics func() ingest.Snapshot
	// EncryptionKey seals channel secrets (D17).
	EncryptionKey []byte
	// Notifier sends test alerts and retries deliveries (M6).
	Notifier Notifier
}

// Notifier is what the dashboard needs from the notification service.
type Notifier interface {
	SendTest(ctx context.Context, channelID int64) (int64, error)
	Retry(ctx context.Context, id int64) error
	InvalidateChannels()
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
	authed.HandleFunc("GET /issues", h.issueList)
	authed.HandleFunc("GET /issues/{id}", h.issueShow)
	authed.HandleFunc("POST /issues/{id}/status", h.issueSetStatus)
	authed.HandleFunc("GET /queue-failures", h.queueDashboard)
	authed.HandleFunc("GET /queue-failures/jobs", h.failedJobList)
	authed.HandleFunc("GET /queue-failures/{id}", h.failedJobShow)
	authed.HandleFunc("GET /notifications", h.notificationsPage)
	authed.HandleFunc("GET /notifications/channels/new", h.channelNew)
	authed.HandleFunc("POST /notifications/channels", h.channelCreate)
	authed.HandleFunc("GET /notifications/channels/{id}", h.channelShow)
	authed.HandleFunc("POST /notifications/channels/{id}", h.channelUpdate)
	authed.HandleFunc("POST /notifications/channels/{id}/enabled", h.channelSetEnabled)
	authed.HandleFunc("POST /notifications/channels/{id}/delete", h.channelDelete)
	authed.HandleFunc("POST /notifications/channels/{id}/test", h.channelTest)
	authed.HandleFunc("POST /notifications/channels/{id}/secret", h.channelRotateSecret)
	authed.HandleFunc("POST /notifications/history/{id}/retry", h.notificationRetry)
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
	"settings", "error", "issues", "issue", "queue", "queue_jobs", "queue_job",
	"notifications", "notification_channel", "notification_secret",
}

// assetURL returns a function mapping a static path to its URL with a
// content-hash query (?v=…). Each file's URL changes whenever its bytes do, so
// browsers fetch the new copy at once despite the max-age cache header.
func assetURL() func(string) string {
	versions := map[string]string{}
	_ = fs.WalkDir(web.FS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(web.FS, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		versions[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:5])
		return nil
	})
	return func(p string) string {
		if v, ok := versions[p]; ok {
			return "/static/" + p + "?v=" + v
		}
		return "/static/" + p
	}
}

func (h *Handler) parseTemplates() error {
	funcs := template.FuncMap{
		"asset":   assetURL(),
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
		"compact": compact,
		"add":     func(a, b int) int { return a + b },
		"sub":     func(a, b int) int { return a - b },
		"percent": func(part, total int64) int64 {
			if total <= 0 {
				return 0
			}
			return min(100, (part*100+total/2)/total)
		},
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				if k, ok := kv[i].(string); ok {
					m[k] = kv[i+1]
				}
			}
			return m
		},
		"hasString": slices.Contains[[]string],
		"lower":     strings.ToLower,
		"today":     func() string { return time.Now().Format("Monday, 2 January 2006") },
		"signed": func(n int64) string {
			if n > 0 {
				return "+" + compact(n)
			}
			if n < 0 {
				return "−" + compact(-n)
			}
			return "±0"
		},
		"triggerLabel": func(t string) string {
			switch t {
			case store.TriggerNewIssue:
				return "new issue"
			case store.TriggerRegression:
				return "regression"
			case store.TriggerQueueFailure:
				return "failed job"
			case store.TriggerThreshold:
				return "threshold"
			case store.TriggerTest:
				return "test"
			}
			return t
		},
		"truncate": func(s string, n int) string {
			if r := []rune(s); len(r) > n {
				return string(r[:n]) + "…"
			}
			return s
		},
		// classShort and classNamespace split a PHP class name:
		// App\Jobs\SyncInvoice → "SyncInvoice" and "App\Jobs".
		"classShort": func(class string) string {
			if i := strings.LastIndex(class, `\`); i >= 0 {
				return class[i+1:]
			}
			return class
		},
		"classNamespace": func(class string) string {
			if i := strings.LastIndex(class, `\`); i >= 0 {
				return class[:i]
			}
			return ""
		},
		"pathOf": func(raw string) string {
			if u, err := url.Parse(raw); err == nil && u.Path != "" {
				return u.Path
			}
			return raw
		},
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

// renderPartial executes one template block with arbitrary data, for HTMX
// fragments that aren't driven by a full page.
func (h *Handler) renderPartial(w http.ResponseWriter, name, block string, data any) {
	t, ok := h.pages[name]
	if !ok {
		h.serverError(w, fmt.Errorf("unknown template %q", name))
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		h.serverError(w, fmt.Errorf("render %s/%s: %w", name, block, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
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
	ctx := r.Context()
	stats, err := h.store.Stats(ctx)
	if err != nil {
		h.serverError(w, err)
		return
	}
	// One rollup read covers the heatmap's eight weeks and every chart range.
	days := 30
	switch r.URL.Query().Get("range") {
	case "7d":
		days = 7
	case "14d":
		days = 14
	}
	history, err := h.store.Trend(ctx, heatmapWeeks*7, 0)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend := history[len(history)-days:]
	activity, err := h.store.ProjectsActivity(ctx, 7)
	if err != nil {
		h.serverError(w, err)
		return
	}
	recent, err := h.store.ListIssues(ctx, store.IssueFilter{Status: store.IssueUnresolved, PerPage: 6})
	if err != nil {
		h.serverError(w, err)
		return
	}
	failures, err := h.store.RecentQueueFailures(ctx, 6)
	if err != nil {
		h.serverError(w, err)
		return
	}
	names, projects, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	cards := make([]projectCard, 0, len(projects))
	for _, p := range projects {
		a := activity[p.ID]
		if a == nil {
			a = &store.ProjectActivity{ProjectID: p.ID, Days: make([]int64, 7)}
		}
		cards = append(cards, projectCard{Project: p, Activity: *a, Spark: sparkline(a.Days)})
	}
	h.render(w, http.StatusOK, "overview", "", h.newPage(r, "Overview", "overview", map[string]any{
		"ActiveProjects":   active,
		"DisabledProjects": disabled,
		"Stats":            stats,
		"Range":            days,
		"Chart":            newBarChart(fmt.Sprintf("Events per day, last %d days", days), trend, []string{"Errors", "Failed jobs"}),
		"Heatmap":          newHeatmap(history, heatmapWeeks),
		"Cards":            cards,
		"Recent":           h.issueRows(names, recent.Issues),
		"Failures":         failures,
	}))
}

// heatmapWeeks is how many weeks the overview's activity heatmap shows.
const heatmapWeeks = 12

// projectCard is one project on the overview.
type projectCard struct {
	Project  store.Project
	Activity store.ProjectActivity
	Spark    string
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
		"Ingest":        h.metricsSnapshot(),
	}))
}

// ---- small helpers ----

func (h *Handler) metricsSnapshot() *ingest.Snapshot {
	if h.opts.Metrics == nil {
		return nil
	}
	s := h.opts.Metrics()
	return &s
}

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
