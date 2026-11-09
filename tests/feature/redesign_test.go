package feature

import (
	"net/http"
	"net/url"
	"regexp"
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

var cardStatusRe = regexp.MustCompile(`hx-post="(/issues/(\d+)/status)" hx-target="#issue-card-(\d+)"`)

func TestIssueCardQuickActions(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	list := a.Get("/issues?sort=count")
	m := cardStatusRe.FindStringSubmatch(list.Body)
	if m == nil || m[2] != m[3] {
		t.Fatal("issue cards should post status changes to their own card")
	}
	path, id := m[1], m[2]

	// HTMX: the card re-renders itself, now resolved.
	form := url.Values{"status": {"resolved"}, "csrf_token": {a.CSRF("/issues")}, "next": {"/issues?sort=count"}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, a.Server.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "issue-card-"+id)
	resp := testutil.Do(t, a.Client, req)
	card := strings.TrimSpace(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(card, `<article class="issue-card" id="issue-card-`+id+`"`) ||
		!contains(card, "Resolved") || !contains(card, "Reopen") || contains(card, "<html") {
		t.Fatalf("card after resolving: %d %.200s", resp.StatusCode, card)
	}
	if strings.Count(card, `class="spark"`) != 1 {
		t.Error("the re-rendered card keeps its sparkline")
	}

	// Plain form: back to the same filtered list.
	plain := a.PostForm(path, url.Values{"status": {"unresolved"}, "next": {"/issues?sort=count&q=x"}}, a.CSRF("/issues"))
	if plain.Request.URL.Path != "/issues" || plain.Request.URL.Query().Get("sort") != "count" {
		t.Fatalf("plain form returned to %s", plain.Request.URL)
	}
	// Anything that isn't the local list falls back to the issue page.
	for _, next := range []string{"https://evil.example/issues", "//evil.example/issues", "/issues/../admin", "/projects"} {
		got := a.PostForm(path, url.Values{"status": {"unresolved"}, "next": {next}}, a.CSRF("/issues"))
		if got.Request.URL.Path != "/issues/"+id || got.Request.URL.Host != strings.TrimPrefix(a.Server.URL, "http://") {
			t.Errorf("next=%q: ended at %s", next, got.Request.URL)
		}
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
