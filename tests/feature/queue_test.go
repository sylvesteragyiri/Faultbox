package feature

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

var failedJobLinkRe = regexp.MustCompile(`href="/queue-failures/(\d+)"`)

// jobResults returns only the results section of the failed-job list.
func jobResults(body string) string {
	if i := strings.Index(body, `id="job-results"`); i >= 0 {
		return body[i:]
	}
	return ""
}

func TestQueueDashboard(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	page := a.Get("/queue-failures")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("status %d", page.StatusCode)
	}
	for _, want := range []string{
		"Queue Failures", "Failed jobs", "Failing job classes", "Failed retries",
		`class="chart-svg"`, "Failed jobs per day",
		"Most failing jobs", `App\Jobs\SyncInvoiceToXero`, `App\Jobs\ProcessPodcast`,
		"Recent failures", "By queue", "integrations", "media", "By connection", "redis",
		"Failure rate by environment",
	} {
		if !contains(page.Body, want) {
			t.Errorf("queue dashboard is missing %q", want)
		}
	}
	// Request exceptions stay out of the queue views.
	for _, unwanted := range []string{"ModelNotFoundException", "UniqueConstraintViolationException", "Coming in"} {
		if contains(page.Body, unwanted) {
			t.Errorf("queue dashboard shows %q", unwanted)
		}
	}
}

func TestFailedJobListFiltersAndSeparation(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	body := jobResults(a.Get("/queue-failures/jobs").Body)
	if n := len(failedJobLinkRe.FindAllString(body, -1)); n != 2 {
		t.Fatalf("%d failed jobs listed, want 2", n)
	}
	if contains(body, "ModelNotFoundException") || contains(body, "TypeError") {
		t.Error("request exceptions appear in the failed-job list")
	}
	for _, want := range []string{`App\Jobs\SyncInvoiceToXero`, "Terminal", "integrations", "redis", "3<span class=\"muted\"> / 3</span>"} {
		if !contains(body, want) {
			t.Errorf("failed-job list is missing %q", want)
		}
	}

	// And failed jobs stay out of the default issue list.
	if issues := results(a.Get("/issues").Body); contains(issues, "SyncInvoiceToXero") {
		t.Error("a failed job appears in the default issue list")
	}

	filtered := jobResults(a.Get("/queue-failures/jobs?queue=media").Body)
	if !contains(filtered, "ProcessPodcast") || contains(filtered, "SyncInvoiceToXero") {
		t.Error("queue filter did not narrow the list")
	}
	byJob := jobResults(a.Get("/queue-failures/jobs?job=" + url.QueryEscape(`App\Jobs\SyncInvoiceToXero`)).Body)
	if !contains(byJob, "SyncInvoiceToXero") || contains(byJob, "ProcessPodcast") {
		t.Error("job class filter did not narrow the list")
	}
	none := jobResults(a.Get("/queue-failures/jobs?connection=sqs").Body)
	if !contains(none, "No matching failed jobs") {
		t.Error("an empty result should say nothing matches")
	}

	// HTMX filter changes swap only the results.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, a.Server.URL+"/queue-failures/jobs?queue=media", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "job-results")
	partial := testutil.Do(t, a.Client, req)
	if !strings.HasPrefix(strings.TrimSpace(partial.Body), `<section id="job-results"`) || contains(partial.Body, "<html") {
		t.Errorf("HTMX request should return only the results section, got: %.120s", partial.Body)
	}
}

func TestFailedJobDetail(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)

	list := jobResults(a.Get("/queue-failures/jobs?queue=integrations").Body)
	m := failedJobLinkRe.FindStringSubmatch(list)
	if m == nil {
		t.Fatal("no failed job link")
	}
	path := "/queue-failures/" + m[1]
	page := a.Get(path)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("status %d", page.StatusCode)
	}
	for _, want := range []string{
		`App\Jobs\SyncInvoiceToXero`, `GuzzleHttp\Exception\ConnectException`, "cURL error 28",
		"Terminal", "Out of retries", "integrations", "on redis", "9d3c1a5e-7b2f-4c8e-a1d6-3f5b8e2c7a90", // job ID
		`class="frame frame-app"`, "Payload metadata", "invoice_id", "Exception group",
		"Recent failures of this job", "Resolve", "Ignore", "project settings",
	} {
		if !contains(page.Body, want) {
			t.Errorf("failed-job detail is missing %q", want)
		}
	}
	if contains(page.Body, "xero-secret-token") {
		t.Error("a sensitive payload value was shown")
	}

	// A plain status form returns to the failed job, not the issue.
	resolved := a.PostForm(issuePathFrom(t, page.Body)+"/status",
		url.Values{"status": {"resolved"}, "next": {path}}, a.CSRF(path))
	if resolved.Request.URL.Path != path || !contains(resolved.Body, "Reopens automatically") {
		t.Fatalf("resolve from failed job: ended at %s", resolved.Request.URL.Path)
	}
	// An off-site "next" falls back to the issue page.
	issue := issuePathFrom(t, page.Body)
	offsite := a.PostForm(issue+"/status", url.Values{"status": {"ignored"}, "next": {"//evil.example/queue-failures/1"}}, a.CSRF(path))
	if offsite.Request.URL.Path != issue {
		t.Fatalf("off-site next: ended at %s", offsite.Request.URL.Path)
	}

	for _, bad := range []string{"/queue-failures/999999", "/queue-failures/abc"} {
		if resp := a.Get(bad); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", bad, resp.StatusCode)
		}
	}
}

var issueFormRe = regexp.MustCompile(`action="(/issues/\d+)/status"`)

func issuePathFrom(t *testing.T, body string) string {
	t.Helper()
	m := issueFormRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no issue status form on the page")
	}
	return m[1]
}

func TestOverviewLinksToFailedJobs(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	body := a.Get("/").Body
	if !contains(body, `href="/queue-failures"`) || !failedJobLinkRe.MatchString(body) {
		t.Error("overview should link the failed-jobs counter and recent failures to the queue pages")
	}
}

func TestProjectQueueDashboardLink(t *testing.T) {
	t.Parallel()
	a := appWithIssues(t)
	projects, err := a.Store().Projects(t.Context())
	if err != nil || len(projects) == 0 {
		t.Fatal(err)
	}
	path := "/projects/" + strconv.FormatInt(projects[0].ID, 10)
	form := url.Values{"name": {"Shop"}, "environments": {""}}

	form.Set("queue_url", "javascript:alert(1)")
	if bad := a.PostForm(path, form, a.CSRF(path)); bad.StatusCode != http.StatusUnprocessableEntity || !contains(bad.Body, "must be a full http") {
		t.Fatalf("unsafe link: status %d", bad.StatusCode)
	}

	form.Set("queue_url", "https://shop.example.com/horizon")
	saved := a.PostForm(path, form, a.CSRF(path))
	if !contains(saved.Body, "Project saved") || !contains(saved.Body, `value="https://shop.example.com/horizon"`) {
		t.Fatalf("link not saved: status %d", saved.StatusCode)
	}
	dashboard := a.Get("/queue-failures").Body
	if !contains(dashboard, `href="https://shop.example.com/horizon"`) || !contains(dashboard, `rel="noopener noreferrer"`) {
		t.Error("the queue dashboard should link to the project's queue tool")
	}
	m := failedJobLinkRe.FindStringSubmatch(jobResults(a.Get("/queue-failures/jobs").Body))
	if m == nil || !contains(a.Get("/queue-failures/"+m[1]).Body, `href="https://shop.example.com/horizon"`) {
		t.Error("the failed-job page should link to the project's queue tool")
	}
}
