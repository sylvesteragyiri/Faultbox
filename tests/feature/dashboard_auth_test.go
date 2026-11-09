package feature

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestAnonymousVisitorsAreSentToLogin(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	for _, path := range []string{"/", "/projects", "/settings", "/no-such-page"} {
		resp := a.Get(path)
		if resp.Request.URL.Path != "/login" {
			t.Errorf("GET %s ended at %s, want /login", path, resp.Request.URL.Path)
		}
	}
	resp := a.Get("/projects")
	if got := resp.Request.URL.Query().Get("next"); got != "/projects" {
		t.Errorf("next = %q, want /projects", got)
	}
}

func TestLoginAndLogout(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	resp := a.Login(testutil.AdminEmail, testutil.AdminPassword)
	if resp.Request.URL.Path != "/" || !contains(resp.Body, "Overview") || !contains(resp.Body, testutil.AdminEmail) {
		t.Fatalf("after login: at %s", resp.Request.URL.Path)
	}
	if got := a.Get("/settings"); got.StatusCode != http.StatusOK || !contains(got.Body, "Schema version") {
		t.Fatalf("settings: status %d", got.StatusCode)
	}

	csrf := a.CSRF("/")
	resp = a.PostForm("/logout", nil, csrf)
	if resp.Request.URL.Path != "/login" {
		t.Fatalf("after logout: at %s", resp.Request.URL.Path)
	}
	if got := a.Get("/projects"); got.Request.URL.Path != "/login" {
		t.Fatal("session still valid after logout")
	}
}

func TestLoginRejectsWrongCredentials(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	for _, creds := range [][2]string{
		{testutil.AdminEmail, "wrong-password-here"},
		{"nobody@example.com", testutil.AdminPassword},
	} {
		resp := a.PostForm("/login", url.Values{"email": {creds[0]}, "password": {creds[1]}}, a.CSRF("/login"))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401", creds[0], resp.StatusCode)
		}
		if !contains(resp.Body, "don&#39;t match") {
			t.Fatalf("%s: missing error message", creds[0])
		}
	}
}

func TestLoginIsRateLimitedPerEmail(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	for range 5 {
		a.PostForm("/login", url.Values{"email": {testutil.AdminEmail}, "password": {"wrong-password-here"}}, a.CSRF("/login"))
	}
	resp := a.PostForm("/login", url.Values{"email": {testutil.AdminEmail}, "password": {testutil.AdminPassword}}, a.CSRF("/login"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 even with the right password", resp.StatusCode)
	}
}

func TestLoginRedirectsOnlyToLocalPaths(t *testing.T) {
	t.Parallel()

	for next, want := range map[string]string{
		"/projects":            "/projects",
		"//evil.example.com":   "/",
		"https://evil.example": "/",
		"/\\evil.example.com":  "/",
	} {
		a := testutil.NewApp(t)
		resp := a.PostForm("/login", url.Values{
			"email": {testutil.AdminEmail}, "password": {testutil.AdminPassword}, "next": {next},
		}, a.CSRF("/login"))
		if resp.Request.URL.Host != a.Server.Listener.Addr().String() || resp.Request.URL.Path != want {
			t.Errorf("next=%q: ended at %s, want %s on this server", next, resp.Request.URL, want)
		}
	}
}

func TestFormsWithoutCSRFTokenAreRejected(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	resp := a.PostForm("/login", url.Values{"email": {testutil.AdminEmail}, "password": {testutil.AdminPassword}}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login without token: status %d, want 403", resp.StatusCode)
	}

	a.LoginAdmin()
	resp = a.PostForm("/projects", url.Values{"name": {"Sneaky"}}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create project without token: status %d, want 403", resp.StatusCode)
	}
	resp = a.PostForm("/projects", url.Values{"name": {"Sneaky"}, "csrf_token": {"forged"}}, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create project with forged token: status %d, want 403", resp.StatusCode)
	}
}

func TestCrossSiteRequestsAreRejected(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	csrf := a.CSRF("/projects/new")

	form := url.Values{"name": {"Cross site"}, "csrf_token": {csrf}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.Server.URL+"/projects", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp := testutil.Do(t, a.Client, req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestSessionCookieIsHardened(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	form := url.Values{"email": {testutil.AdminEmail}, "password": {testutil.AdminPassword}, "csrf_token": {a.CSRF("/login")}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.Server.URL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noRedirect := *a.Client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp := testutil.Do(t, &noRedirect, req)

	var found bool
	for _, c := range resp.Cookies() {
		if c.Name != "faultbox_session" {
			continue
		}
		found = true
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
			t.Fatalf("session cookie not hardened: %+v", c)
		}
	}
	if !found {
		t.Fatal("no session cookie set")
	}
}

func TestBootstrapAdminIsCreatedOnce(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	a := testutil.NewAppWithConfig(t, cfg)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	// Restarting with a different password must not reset the existing admin.
	cfg.AdminPassword = "a-different-password"
	b := testutil.NewAppWithConfig(t, cfg)
	b.Login(testutil.AdminEmail, testutil.AdminPassword)
	if n, err := b.Store().CountUsers(t.Context()); err != nil || n != 1 {
		t.Fatalf("users = %d, %v; want 1", n, err)
	}
}
