// Package testutil holds helpers shared by the test suites.
package testutil

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sylvesteragyiri/faultbox/internal/app"
	"github.com/sylvesteragyiri/faultbox/internal/config"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Bootstrap admin credentials used by NewApp.
const (
	AdminEmail    = "admin@example.com"
	AdminPassword = "correct-horse-battery"
	Secret        = "test-secret-that-is-at-least-32-bytes-long!!"
)

// DecodeJSON decodes r into a value of type T and fails the test on error.
func DecodeJSON[T any](t *testing.T, r io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return v
}

// Logger discards log output unless the test fails verbosely.
func Logger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Config returns a valid configuration with a temporary database and a
// bootstrap admin. bcrypt runs at its minimum cost to keep tests fast.
func Config(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Port:            "8080",
		DatabasePath:    filepath.Join(t.TempDir(), "faultbox.db"),
		Secret:          []byte(Secret),
		AdminEmail:      AdminEmail,
		AdminPassword:   AdminPassword,
		RetentionDays:   30,
		SessionTTL:      7 * 24 * time.Hour,
		BcryptCost:      bcrypt.MinCost,
		ShutdownTimeout: time.Second,
	}
}

// OpenStore opens a fresh database in a temporary directory.
func OpenStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "faultbox.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// App is a running Faultbox instance plus a browser-like client.
type App struct {
	*app.App
	Server *httptest.Server
	Client *http.Client
	t      *testing.T
}

// NewApp starts Faultbox on a local port with a temporary database.
func NewApp(t *testing.T) *App {
	t.Helper()
	return NewAppWithConfig(t, Config(t))
}

// NewAppWithConfig starts Faultbox with a specific configuration.
func NewAppWithConfig(t *testing.T, cfg config.Config) *App {
	t.Helper()
	a, err := app.New(context.Background(), cfg, Logger(t), "test")
	if err != nil {
		t.Fatalf("start app: %v", err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = a.Close()
	})
	return &App{App: a, Server: srv, Client: NewBrowser(t), t: t}
}

// NewBrowser returns a client with a cookie jar that follows redirects.
func NewBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// Response is a fully read HTTP response.
type Response struct {
	*http.Response
	Body string
}

// Do sends a request with the given client and reads the whole body.
func Do(t *testing.T, c *http.Client, req *http.Request) Response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return Response{Response: resp, Body: string(b)}
}

// Get fetches a dashboard path with the app's browser client.
func (a *App) Get(path string) Response {
	a.t.Helper()
	req, err := http.NewRequestWithContext(a.t.Context(), http.MethodGet, a.Server.URL+path, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	return Do(a.t, a.Client, req)
}

// PostForm submits a form. A csrf_token is added from csrf unless form
// already has one; pass "" to send none.
func (a *App) PostForm(path string, form url.Values, csrf string) Response {
	a.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if csrf != "" && form.Get("csrf_token") == "" {
		form.Set("csrf_token", csrf)
	}
	req, err := http.NewRequestWithContext(a.t.Context(), http.MethodPost, a.Server.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return Do(a.t, a.Client, req)
}

// API calls the ingestion API with an optional API key.
func (a *App) API(method, path, key string) Response {
	a.t.Helper()
	req, err := http.NewRequestWithContext(a.t.Context(), method, a.Server.URL+path, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return Do(a.t, http.DefaultClient, req)
}

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// CSRF loads path and returns the CSRF token embedded in its forms.
func (a *App) CSRF(path string) string {
	a.t.Helper()
	resp := a.Get(path)
	m := csrfRe.FindStringSubmatch(resp.Body)
	if m == nil {
		a.t.Fatalf("no csrf token on %s (status %d)", path, resp.StatusCode)
	}
	return html.UnescapeString(m[1])
}

// Login signs the browser client in and fails the test if it doesn't work.
func (a *App) Login(email, password string) Response {
	a.t.Helper()
	resp := a.PostForm("/login", url.Values{"email": {email}, "password": {password}}, a.CSRF("/login"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, "Log out") {
		a.t.Fatalf("login as %s failed: status %d", email, resp.StatusCode)
	}
	return resp
}

// LoginAdmin signs in as the bootstrap admin.
func (a *App) LoginAdmin() { a.t.Helper(); a.Login(AdminEmail, AdminPassword) }

var keyRe = regexp.MustCompile(`id="api-key" class="mono" type="text" value="(fbx_[0-9A-Za-z]+)"`)

// CreateProject creates a project through the dashboard and returns the
// one-time API key shown on the result page.
func (a *App) CreateProject(name, environments string) (key string, page Response) {
	a.t.Helper()
	page = a.PostForm("/projects", url.Values{"name": {name}, "environments": {environments}}, a.CSRF("/projects/new"))
	if page.StatusCode != http.StatusOK {
		a.t.Fatalf("create project: status %d", page.StatusCode)
	}
	return ExtractAPIKey(a.t, page.Body), page
}

// ExtractAPIKey pulls the one-time key out of a key page.
func ExtractAPIKey(t *testing.T, body string) string {
	t.Helper()
	m := keyRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no API key on page")
	}
	return m[1]
}
