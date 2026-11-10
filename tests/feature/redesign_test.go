package feature

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestOverviewProjectCardsHeatmapAndRange(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	body := a.Get("/").Body
	for _, want := range []string{
		`class="project-card card"`, "Shop", "Events · 7 days", "Unresolved", // project card
		`class="chart heatmap"`, `class="cell"`, "Show as table", "Less", "More", // activity heatmap
		`aria-current="true">30 days`, // default chart range
	} {
		if !contains(body, want) {
			t.Errorf("overview is missing %q", want)
		}
	}
	if n := strings.Count(body, `class="cell"`); n < 7*11 || n > 7*12 {
		t.Errorf("%d heatmap days, want 12 weeks up to today", n)
	}

	week := a.Get("/?range=7d").Body
	if n := strings.Count(week, `class="bar"`); n != 7 {
		t.Errorf("7-day range: %d columns, want 7", n)
	}
	if !contains(week, `aria-current="true">7 days`) {
		t.Error("the 7-day tab should be marked current")
	}
}

func TestIssueTableBulkActionsAndAssignment(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "ModelNotFoundException")
	list := a.Get("/issues?sort=count")
	for _, want := range []string{`class="data-table issue-table"`, `data-select-all="issue-bulk"`, "Assign to me", "First seen", "Last seen"} {
		if !contains(list.Body, want) {
			t.Errorf("compact issues table is missing %q", want)
		}
	}
	if !contains(list.Body, `data-issue-tooltip=`) || !contains(list.Body, `title="`) {
		t.Error("issue message should be available before opening the issue")
	}
	if !contains(a.Get("/issues?assigned=unassigned").Body, `value="`+id+`"`) {
		t.Fatal("unassigned issue does not appear in Unassigned")
	}

	assigned := a.PostForm("/issues/"+id+"/assign", url.Values{"assigned": {"me"}, "next": {"/issues"}}, a.CSRF("/issues"))
	if assigned.Request.URL.Path != "/issues" {
		t.Fatalf("assignment returned to %s", assigned.Request.URL)
	}
	mine := a.Get("/issues?assigned=mine")
	if !contains(mine.Body, `value="`+id+`"`) {
		t.Fatal("assigned issue does not appear in Mine")
	}

	resolved := a.PostForm("/issues/bulk", url.Values{"id": {id}, "operation": {"resolved"}, "next": {"/issues?assigned=mine"}}, a.CSRF("/issues"))
	if resolved.Request.URL.Path != "/issues" || resolved.Request.URL.Query().Get("assigned") != "mine" {
		t.Fatalf("bulk action returned to %s", resolved.Request.URL)
	}
	if !contains(a.Get("/issues?status=resolved&assigned=mine").Body, `value="`+id+`"`) {
		t.Fatal("bulk resolve did not update the selected issue")
	}
}

func TestIssueDetailInsights(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "ModelNotFoundException")
	body := a.Get("/issues/" + id).Body
	for _, want := range []string{`class="insights"`, "Environments", "Releases", "Type", "Exception", `id="environments"`, `id="releases"`} {
		if !contains(body, want) {
			t.Errorf("issue detail is missing %q", want)
		}
	}
}

func TestThemeSwitchOnEveryPage(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	check := func(name, body string) {
		t.Helper()
		js, css := strings.Index(body, "/static/js/theme.js"), strings.Index(body, "/static/css/app.css")
		if js < 0 || css < 0 || js > css {
			t.Errorf("%s: theme.js must load before the stylesheet so the saved theme applies before paint", name)
		}
		for _, theme := range []string{"system", "light", "dark"} {
			if !contains(body, `data-theme-set="`+theme+`"`) {
				t.Errorf("%s: no %q button in the theme switch", name, theme)
			}
		}
	}
	check("login", a.Get("/login").Body)
	a.LoginAdmin()
	check("overview", a.Get("/").Body)
}

func TestV2SidebarShowsScopeAndGroupedMenus(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	body := a.Get("/").Body
	for _, want := range []string{
		`class="scope-switcher scope-menu"`, "All projects", "All environments",
		`aria-label="Main"`, ">Activity</p>", `aria-label="Manage"`, "Dashboard", "Issues", "Commands", "Settings",
	} {
		if !contains(body, want) {
			t.Errorf("v2 sidebar is missing %q", want)
		}
	}
	if contains(body, `class="topbar"`) || contains(body, `id="global-search"`) {
		t.Error("the global topbar and search should be removed")
	}
}

func TestV2InsightPagesShowHonestEmptyStates(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	requests := a.Get("/requests")
	for _, want := range []string{"Requests", "0 Routes", "No request telemetry yet", "No duration data yet", "No routes found"} {
		if !contains(requests.Body, want) {
			t.Errorf("Requests page is missing %q", want)
		}
	}
	jobs := a.Get("/jobs")
	for _, want := range []string{"Jobs", "No job run data yet", "successful jobs", "/queue-failures"} {
		if !contains(jobs.Body, want) {
			t.Errorf("Jobs page is missing %q", want)
		}
	}
}
