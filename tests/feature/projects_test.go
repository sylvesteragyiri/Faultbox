package feature

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/api"
	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

const projectsMe = "/api/v1/projects/me"

var projectLinkRe = regexp.MustCompile(`href="/projects/(\d+)"`)

func projectPath(t *testing.T, body string) string {
	t.Helper()
	m := projectLinkRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no project link on page")
	}
	return "/projects/" + m[1]
}

func TestCreateProjectShowsKeyOnceAndKeyWorks(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()

	key, page := a.CreateProject("Billing API", "Production, staging")
	if !contains(page.Body, "Copy this key now") || !contains(page.Body, "FAULTBOX_API_KEY="+key) {
		t.Fatal("key page is missing the warning or the .env snippet")
	}
	if cc := page.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("key page Cache-Control = %q, want no-store", cc)
	}

	// The key is never shown again, only its hint.
	path := projectPath(t, page.Body)
	detail := a.Get(path)
	if contains(detail.Body, key) {
		t.Fatal("project page shows the full key")
	}
	if !contains(detail.Body, "fbx_…"+key[len(key)-4:]) {
		t.Fatal("project page is missing the key hint")
	}
	if list := a.Get("/projects"); contains(list.Body, key) || !contains(list.Body, "Billing API") {
		t.Fatal("project list leaks the key or misses the project")
	}

	resp := a.API(http.MethodGet, projectsMe, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("projects/me: status %d, body %s", resp.StatusCode, resp.Body)
	}
	got := testutil.DecodeJSON[api.ProjectResponse](t, strings.NewReader(resp.Body))
	if got.Name != "Billing API" || got.Slug != "billing-api" || got.Status != "active" ||
		strings.Join(got.Environments, ",") != "production,staging" {
		t.Fatalf("projects/me = %+v", got)
	}
}

func TestAPIKeyAuthentication(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	key, _ := a.CreateProject("Shop", "")

	tests := []struct {
		name   string
		header func(*http.Request)
		status int
		code   string
	}{
		{"missing", func(*http.Request) {}, http.StatusUnauthorized, "missing_api_key"},
		{"wrong scheme", func(r *http.Request) { r.Header.Set("Authorization", "Basic "+key) }, http.StatusUnauthorized, "missing_api_key"},
		{"malformed", func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-a-key") }, http.StatusUnauthorized, "invalid_api_key"},
		{"unknown", func(r *http.Request) { r.Header.Set("Authorization", "Bearer fbx_"+strings.Repeat("A", 43)) }, http.StatusUnauthorized, "invalid_api_key"},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }, http.StatusOK, ""},
		{"x-faultbox-key", func(r *http.Request) { r.Header.Set("X-Faultbox-Key", key) }, http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.Server.URL+projectsMe, nil)
			if err != nil {
				t.Fatal(err)
			}
			tt.header(req)
			resp := testutil.Do(t, http.DefaultClient, req)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tt.status, resp.Body)
			}
			if tt.code != "" {
				body := testutil.DecodeJSON[httpx.ErrorBody](t, strings.NewReader(resp.Body))
				if body.Error.Code != tt.code {
					t.Fatalf("code %q, want %q", body.Error.Code, tt.code)
				}
			}
		})
	}
}

func TestDisabledProjectIsRejectedUntilEnabled(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	key, page := a.CreateProject("Worker", "")
	path := projectPath(t, page.Body)

	resp := a.PostForm(path+"/status", url.Values{"status": {"disabled"}}, a.CSRF(path))
	if resp.StatusCode != http.StatusOK || !contains(resp.Body, "Disabled") {
		t.Fatalf("disable: status %d", resp.StatusCode)
	}
	api := a.API(http.MethodGet, projectsMe, key)
	if api.StatusCode != http.StatusForbidden || !contains(api.Body, "project_disabled") {
		t.Fatalf("disabled project: status %d, body %s", api.StatusCode, api.Body)
	}

	a.PostForm(path+"/status", url.Values{"status": {"active"}}, a.CSRF(path))
	if api := a.API(http.MethodGet, projectsMe, key); api.StatusCode != http.StatusOK {
		t.Fatalf("re-enabled project: status %d", api.StatusCode)
	}
}

func TestStatusToggleReturnsFragmentForHTMX(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	_, page := a.CreateProject("Fragment", "")
	path := projectPath(t, page.Body)
	csrf := a.CSRF(path)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.Server.URL+path+"/status",
		strings.NewReader(url.Values{"status": {"disabled"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-CSRF-Token", csrf)
	resp := testutil.Do(t, a.Client, req)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if contains(resp.Body, "<html") || !contains(resp.Body, `id="status-panel"`) || !contains(resp.Body, "Enable project") {
		t.Fatalf("expected only the status panel fragment, got:\n%s", resp.Body)
	}
}

func TestRegenerateKeyRevokesOldKey(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	oldKey, page := a.CreateProject("Rotating", "")
	path := projectPath(t, page.Body)

	resp := a.PostForm(path+"/key", nil, a.CSRF(path))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("regenerate: status %d", resp.StatusCode)
	}
	newKey := testutil.ExtractAPIKey(t, resp.Body)
	if newKey == oldKey {
		t.Fatal("key did not change")
	}
	if api := a.API(http.MethodGet, projectsMe, oldKey); api.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old key: status %d, want 401", api.StatusCode)
	}
	if api := a.API(http.MethodGet, projectsMe, newKey); api.StatusCode != http.StatusOK {
		t.Fatalf("new key: status %d, want 200", api.StatusCode)
	}
}

func TestEditProject(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	key, page := a.CreateProject("Old name", "production")
	path := projectPath(t, page.Body)

	resp := a.PostForm(path, url.Values{"name": {"New name"}, "environments": {"staging, local"}}, a.CSRF(path))
	if resp.StatusCode != http.StatusOK || !contains(resp.Body, "Project saved.") {
		t.Fatalf("save: status %d", resp.StatusCode)
	}
	got := testutil.DecodeJSON[api.ProjectResponse](t, strings.NewReader(a.API(http.MethodGet, projectsMe, key).Body))
	if got.Name != "New name" || strings.Join(got.Environments, ",") != "local,staging" {
		t.Fatalf("after edit: %+v", got)
	}
}

func TestProjectFormValidation(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()

	for _, form := range []url.Values{
		{"name": {"   "}},
		{"name": {"Ok"}, "environments": {"prod/eu"}},
	} {
		resp := a.PostForm("/projects", form, a.CSRF("/projects/new"))
		if resp.StatusCode != http.StatusUnprocessableEntity || !contains(resp.Body, "alert-error") {
			t.Fatalf("%v: status %d, want 422 with an error", form, resp.StatusCode)
		}
	}
	if n, _, _ := a.Store().CountProjects(t.Context()); n != 0 {
		t.Fatalf("%d projects created from invalid input", n)
	}
}

func TestUnknownProjectAndAPIRouteReturn404(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()

	for _, path := range []string{"/projects/9999", "/projects/abc"} {
		if resp := a.Get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", path, resp.StatusCode)
		}
	}
	resp := a.API(http.MethodGet, "/api/v1/nope", "")
	if resp.StatusCode != http.StatusNotFound || !contains(resp.Body, `"code":"not_found"`) {
		t.Fatalf("unknown API route: status %d, body %s", resp.StatusCode, resp.Body)
	}
}
