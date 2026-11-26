package feature

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// appWithTwoProjects has a TypeError in "Shop" (production) and a
// ModelNotFoundException in "Admin" (staging).
func appWithTwoProjects(t *testing.T) (*testutil.App, int64, int64) {
	t.Helper()
	a := testutil.NewApp(t)
	shop, shopKey := testutil.NewProjectKey(t, a.Store(), "Shop")
	admin, adminKey := testutil.NewProjectKey(t, a.Store(), "Admin")
	ev := testutil.Stamp(testutil.Fixture(t, "type_error"))
	ev["environment"] = "production"
	a.PostEvent(eventsPath, shopKey, ev)
	ev = testutil.Stamp(testutil.Fixture(t, "model_not_found"))
	ev["environment"] = "staging"
	a.PostEvent(eventsPath, adminKey, ev)
	a.WaitProcessed()
	a.LoginAdmin()
	return a, shop.ID, admin.ID
}

func setScope(t *testing.T, a *testutil.App, project int64, env, next string) testutil.Response {
	t.Helper()
	form := url.Values{"env": {env}, "next": {next}}
	if project > 0 {
		form.Set("project", strconv.FormatInt(project, 10))
	}
	return a.PostForm("/scope", form, a.CSRF("/"))
}

func TestScopeSwitcherScopesTheDashboard(t *testing.T) {
	t.Parallel()
	a, shop, admin := appWithTwoProjects(t)

	// The switcher sits at the top left of every dashboard page.
	home := a.Get("/").Body
	if !contains(home, `class="scope-switcher scope-menu"`) || !contains(home, "All projects") || !contains(home, ">Shop</option>") {
		t.Fatal("the overview should show the project switcher")
	}

	// Switch to Shop from a filtered list: back on the list, without the old project filter.
	resp := setScope(t, a, shop, "", "/issues?project="+strconv.FormatInt(admin, 10)+"&sort=count")
	if resp.Request.URL.Path != "/issues" || resp.Request.URL.Query().Get("sort") != "count" || resp.Request.URL.Query().Has("project") {
		t.Fatalf("after switching: %s", resp.Request.URL)
	}
	list := results(resp.Body)
	if !contains(list, "TypeError") || contains(list, "ModelNotFoundException") {
		t.Error("the issue list should follow the scoped project")
	}
	if contains(list, "Clear filters") || contains(resp.Body, `pill is-set`) {
		t.Error("the scope itself isn't a filter: no pill lit, nothing to clear")
	}

	// Pages don't take their own project or environment: URL parameters
	// can't override the switcher.
	adminID := strconv.FormatInt(admin, 10)
	for _, path := range []string{"/issues?project=", "/issues?project=" + adminID, "/issues?env=staging"} {
		if l := results(a.Get(path).Body); !contains(l, "TypeError") || contains(l, "ModelNotFoundException") {
			t.Errorf("%s should still show only the scoped project", path)
		}
	}
	queue := a.Get("/queue-failures?project=" + adminID).Body
	jobs := a.Get("/queue-failures/jobs?project=" + adminID + "&env=staging").Body
	for name, body := range map[string]string{"queue failures": queue, "failed jobs": jobs} {
		if contains(body, `name="project" class="select-sm"`) || contains(body, `<option value="">Any environment</option>`) ||
			strings.Count(body, `name="project"`) != 1 { // only the switcher's
			t.Errorf("%s should not have its own project or environment filter", name)
		}
	}

	// The overview follows the scope too.
	home = a.Get("/").Body
	if !contains(home, "TypeError") || contains(home, "ModelNotFoundException") || strings.Count(home, `class="project-card card"`) != 1 {
		t.Error("the overview should show only the scoped project")
	}
}

func TestOverviewProjectLinksSwitchTheScope(t *testing.T) {
	t.Parallel()
	a, _, admin := appWithTwoProjects(t)
	adminID := strconv.FormatInt(admin, 10)

	home := a.Get("/").Body
	if contains(home, "/issues?project=") || contains(home, "/queue-failures?project=") {
		t.Error("project cards should switch the scope, not add a project parameter")
	}
	if !contains(home, `<input type="hidden" name="project" value="`+adminID+`"><input type="hidden" name="env" value=""><input type="hidden" name="next" value="/issues">`) {
		t.Fatal("the Admin card should switch the scope to Admin and open its issues")
	}
	// Following it scopes the issue list, and the issue page's Project insight does the same.
	resp := setScope(t, a, admin, "", "/issues")
	if l := results(resp.Body); !contains(l, "ModelNotFoundException") || contains(l, "TypeError") {
		t.Error("after choosing Admin, the issue list should show only Admin")
	}
	id := issueIDFor(t, a, "ModelNotFoundException")
	issue := a.Get("/issues/" + id).Body
	if contains(issue, "/issues?project=") || !contains(issue, `name="project" value="`+adminID+`"><input type="hidden" name="env" value=""><input type="hidden" name="next" value="/issues?status=all">`) {
		t.Error("the issue page's project link should switch the scope")
	}
}

func TestScopeEnvironment(t *testing.T) {
	t.Parallel()
	a, _, admin := appWithTwoProjects(t)

	setScope(t, a, 0, "staging", "/issues")
	list := results(a.Get("/issues").Body)
	if contains(list, "TypeError") || !contains(list, "ModelNotFoundException") {
		t.Error("the issue list should follow the scoped environment")
	}

	// Switching to a project without that environment drops it.
	setScope(t, a, admin, "production", "/")
	page := a.Get("/issues").Body
	if !contains(results(page), "ModelNotFoundException") {
		t.Error("an environment the project doesn't have should be ignored")
	}
	if contains(page, `<option selected>production</option>`) {
		t.Error("the dropped environment shouldn't stay selected")
	}
}

func TestScopeRejectsBadInput(t *testing.T) {
	t.Parallel()
	a, _, _ := appWithTwoProjects(t)

	for _, next := range []string{"https://evil.example/", "//evil.example/x", `/\evil.example`, "javascript:alert(1)"} {
		resp := setScope(t, a, 0, "", next)
		if resp.Request.URL.Host != strings.TrimPrefix(a.Server.URL, "http://") || resp.Request.URL.Path != "/" {
			t.Errorf("next=%q ended at %s", next, resp.Request.URL)
		}
	}
	// Detail pages go back to their list.
	if resp := setScope(t, a, 0, "", "/issues/1?event=x"); resp.Request.URL.Path != "/issues" {
		t.Errorf("from an issue: ended at %s", resp.Request.URL)
	}

	// An unknown project and a junk environment fall back to "all".
	resp := a.PostForm("/scope", url.Values{"project": {"999999"}, "env": {"<script>"}, "next": {"/issues"}}, a.CSRF("/"))
	if list := results(resp.Body); !contains(list, "TypeError") || !contains(list, "ModelNotFoundException") {
		t.Error("an invalid scope should show everything")
	}

	// Switching needs the CSRF token.
	if bad := a.PostForm("/scope", url.Values{"project": {"1"}}, ""); bad.StatusCode != http.StatusForbidden {
		t.Errorf("without a CSRF token: %d", bad.StatusCode)
	}
}
