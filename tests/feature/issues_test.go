package feature

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// appWithIssues ingests every fixture through the API and logs in.
func appWithIssues(t *testing.T) *testutil.App {
	t.Helper()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")
	for _, name := range testutil.FixtureNames {
		ev := testutil.Stamp(testutil.Fixture(t, name))
		path := eventsPath
		if _, isJob := ev["job"]; isJob {
			path = queuePath
		}
		a.PostEvent(path, key, ev)
	}
	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "model_not_found"))) // second occurrence
	a.WaitProcessed()
	a.LoginAdmin()
	return a
}

var issueLinkRe = regexp.MustCompile(`href="/issues/(\d+)"`)

// results returns only the results section of the issue list, so filter
// dropdown options don't count as matches.
func results(body string) string {
	if i := strings.Index(body, `id="issue-results"`); i >= 0 {
		return body[i:]
	}
	return ""
}

func issueIDFor(t *testing.T, a *testutil.App, query string) string {
	t.Helper()
	page := a.Get("/issues?status=all&category=all&q=" + url.QueryEscape(query))
	m := issueLinkRe.FindStringSubmatch(page.Body)
	if m == nil {
		t.Fatalf("no issue found for %q", query)
	}
	return m[1]
}

func TestIssueListShowsAndFilters(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	page := a.Get("/issues")
	page.Body = results(page.Body)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("status %d", page.StatusCode)
	}
	for _, want := range []string{"ModelNotFoundException", "UniqueConstraintViolationException", "TypeError", "ErrorException"} {
		if !contains(page.Body, want) {
			t.Errorf("issue list is missing %s", want)
		}
	}
	if contains(page.Body, "SyncInvoiceToXero") {
		t.Error("failed jobs belong to their own view, not the default error list")
	}
	if n := strings.Count(page.Body, `class="spark"`); n != 4 {
		t.Errorf("%d sparklines, want one per issue (4)", n)
	}

	jobs := a.Get("/issues?category=queue_failure")
	jobs.Body = results(jobs.Body)
	if !contains(jobs.Body, "SyncInvoiceToXero") || !contains(jobs.Body, "ProcessPodcast") || contains(jobs.Body, "TypeError") {
		t.Fatal("failed-jobs filter is wrong")
	}

	search := a.Get("/issues?q=NotFound")
	search.Body = results(search.Body)
	if !contains(search.Body, "ModelNotFoundException") || contains(search.Body, "TypeError") {
		t.Fatal("search for NotFound is wrong")
	}

	staging := a.Get("/issues?env=staging")
	staging.Body = results(staging.Body)
	if !contains(staging.Body, "TypeError") || contains(staging.Body, "ModelNotFoundException") {
		t.Fatal("environment filter is wrong")
	}

	none := a.Get("/issues?q=no-such-error-anywhere")
	if !contains(none.Body, "No matching issues") || !contains(none.Body, "Clear filters") {
		t.Fatal("empty search result has no empty state")
	}
}

func TestIssueFilterReturnsFragmentForHTMX(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.Server.URL+"/issues?q=TypeError", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "issue-results")
	resp := testutil.Do(t, a.Client, req)

	if resp.StatusCode != http.StatusOK || contains(resp.Body, "<html") || !contains(resp.Body, `id="issue-results"`) {
		t.Fatalf("expected only the results fragment, got %d:\n%.300s", resp.StatusCode, resp.Body)
	}
	if !contains(resp.Body, "TypeError") || contains(resp.Body, "ModelNotFoundException") {
		t.Fatal("fragment has the wrong results")
	}
}

func TestIssueDetailPage(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "UniqueConstraintViolation")

	page := a.Get("/issues/" + id)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("status %d", page.StatusCode)
	}
	body := page.Body
	for _, want := range []string{
		"UniqueConstraintViolationException",
		`class="frame frame-app"`,                      // in-app frames highlighted
		"App\\Actions\\Fortify\\CreateNewUser::create", // with class::function
		"app/Actions/Fortify/CreateNewUser.php:34",     // and a release-independent path
		"framework frames",                             // vendor frames collapsed
		"<details>",
		"https://shop.example.com/register", // request
		"User-Agent",
		"Environments", "production", "Releases", "2026.11.1", // breakdowns
		`class="chart-svg"`, // trend chart
		"Show as table",
		"[redacted]", // scrubbed context
	} {
		if !contains(body, strings.ReplaceAll(want, "\\", "\\")) {
			t.Errorf("detail page is missing %q", want)
		}
	}
	for _, secret := range []string{"hunter2hunter2", "laravel_session", "secret-token"} {
		if contains(body, secret) {
			t.Errorf("detail page shows %q", secret)
		}
	}

	if resp := a.Get("/issues/999999"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown issue: status %d", resp.StatusCode)
	}
	if resp := a.Get("/issues/" + id + "?event=00000000-0000-0000-0000-000000000000"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown event: status %d", resp.StatusCode)
	}
}

func TestQueueIssueDetailShowsFailedJobs(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "SyncInvoiceToXero")

	body := a.Get("/issues/" + id).Body
	for _, want := range []string{"Failed jobs", "integrations", "redis", "terminal", "<h3>Job</h3>"} {
		if !contains(body, want) {
			t.Errorf("queue issue page is missing %q", want)
		}
	}
	if contains(body, "xero-secret-token") {
		t.Error("job payload secret is shown")
	}
}

func TestEventSelection(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "ModelNotFoundException")

	body := a.Get("/issues/" + id).Body
	m := regexp.MustCompile(`\?event=([0-9a-f-]{36})#event`).FindAllStringSubmatch(body, -1)
	if len(m) != 2 {
		t.Fatalf("%d event links, want 2", len(m))
	}
	older := a.Get("/issues/" + id + "?event=" + m[1][1])
	if older.StatusCode != http.StatusOK || !contains(older.Body, `class="selected"`) {
		t.Fatalf("selecting an event: status %d", older.StatusCode)
	}
}

// Exit criterion: issues can be resolved, ignored and reopened.
func TestResolveIgnoreReopen(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	id := issueIDFor(t, a, "TypeError")
	path := "/issues/" + id

	htmxPost := func(status string) testutil.Response {
		csrf := a.CSRF(path)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.Server.URL+path+"/status",
			strings.NewReader(url.Values{"status": {status}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-CSRF-Token", csrf)
		return testutil.Do(t, a.Client, req)
	}

	resp := htmxPost("resolved")
	if resp.StatusCode != http.StatusOK || contains(resp.Body, "<html") || !contains(resp.Body, "Resolved") || !contains(resp.Body, "Reopen") {
		t.Fatalf("resolve: %d\n%s", resp.StatusCode, resp.Body)
	}
	if list := a.Get("/issues"); contains(results(list.Body), "TypeError") {
		t.Fatal("resolved issue still listed as unresolved")
	}
	if list := a.Get("/issues?status=resolved"); !contains(results(list.Body), "TypeError") {
		t.Fatal("resolved issue missing from the resolved filter")
	}

	resp = htmxPost("unresolved")
	if !contains(resp.Body, "Unresolved") || !contains(resp.Body, "Ignore") {
		t.Fatalf("reopen: %s", resp.Body)
	}

	resp = htmxPost("ignored")
	if !contains(resp.Body, "Ignored") || !contains(resp.Body, "New events are still counted") {
		t.Fatalf("ignore: %s", resp.Body)
	}

	// Without HTMX the form redirects back to the issue.
	form := a.PostForm(path+"/status", url.Values{"status": {"resolved"}}, a.CSRF(path))
	if form.Request.URL.Path != path || !contains(form.Body, "Reopens automatically") {
		t.Fatalf("plain form: ended at %s", form.Request.URL.Path)
	}

	if bad := a.PostForm(path+"/status", url.Values{"status": {"deleted"}}, a.CSRF(path)); bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown status: %d", bad.StatusCode)
	}
	if noToken := a.PostForm(path+"/status", url.Values{"status": {"ignored"}}, ""); noToken.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF token: %d", noToken.StatusCode)
	}
}

func TestOverviewShowsTrendAndRecent(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	body := a.Get("/").Body
	for _, want := range []string{
		`class="chart-svg"`, `class="legend"`, "Errors", "Failed jobs", // two-series chart with a legend
		`data-tip="`, "Show as table",
		"Recent issues", "ModelNotFoundException",
		"Recent failed jobs", "SyncInvoiceToXero",
	} {
		if !contains(body, want) {
			t.Errorf("overview is missing %q", want)
		}
	}
	if n := strings.Count(body, `class="bar"`); n != 30 {
		t.Errorf("%d day columns, want 30", n)
	}
}
