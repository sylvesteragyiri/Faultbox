package feature

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/api"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

const (
	eventsPath = "/api/v1/events"
	queuePath  = "/api/v1/queue-failures"
)

// Exit criterion: the same exception N times → 1 issue, N events, count N.
func TestSameExceptionNTimesMakesOneIssue(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	const n = 25
	for i := range n {
		ev := testutil.Stamp(testutil.Fixture(t, "model_not_found"))
		// Different order IDs each time: still the same bug.
		ex := testutil.Exception(ev)
		ex["message"] = strings.Replace(ex["message"].(string), "48213", strings.Repeat("7", i+1), 1)
		resp := a.PostEvent(eventsPath, key, ev)
		got := testutil.DecodeJSON[api.AcceptedResponse](t, strings.NewReader(resp.Body))
		if got.Status != "accepted" || got.ID != ev["event_id"] {
			t.Fatalf("202 body = %+v", got)
		}
	}
	a.WaitProcessed()

	issues, err := a.Store().ProjectIssues(t.Context(), p.ID, 10)
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues = %d, %v; want 1", len(issues), err)
	}
	is := issues[0]
	if is.OccurrenceCount != n || is.ExceptionType != "Illuminate\\Database\\Eloquent\\ModelNotFoundException" ||
		is.Culprit != "App\\Http\\Controllers\\OrderController::show" || is.Status != "unresolved" {
		t.Fatalf("issue = %+v", is)
	}
	if events, _ := a.Store().IssueEvents(t.Context(), is.ID, 100); len(events) != n {
		t.Fatalf("events = %d, want %d", len(events), n)
	}
	if m := a.Metrics(); m.Accepted != n || m.Processed != n {
		t.Fatalf("metrics = %+v", m)
	}
}

// Exit criterion: a duplicate event ID is stored only once, and the SDK
// still gets 202 so it stops retrying.
func TestDuplicateEventIDIsStoredOnce(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	ev := testutil.Stamp(testutil.Fixture(t, "undefined_array_key"))
	for range 3 {
		a.PostEvent(eventsPath, key, ev)
	}
	a.WaitProcessed()

	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 10)
	if len(issues) != 1 || issues[0].OccurrenceCount != 1 {
		t.Fatalf("issues = %+v", issues)
	}
	if m := a.Metrics(); m.Processed != 1 || m.Duplicates != 2 {
		t.Fatalf("processed %d, duplicates %d", m.Processed, m.Duplicates)
	}
}

func TestEveryFixtureIsIngested(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "All")

	for _, name := range testutil.FixtureNames {
		ev := testutil.Stamp(testutil.Fixture(t, name))
		path := eventsPath
		if _, isJob := ev["job"]; isJob {
			path = queuePath
		}
		a.PostEvent(path, key, ev)
	}
	a.WaitProcessed()

	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 20)
	if len(issues) != len(testutil.FixtureNames) {
		t.Fatalf("%d issues, want one per fixture (%d)", len(issues), len(testutil.FixtureNames))
	}
	var queueIssues int
	for _, is := range issues {
		if is.Category == "queue_failure" {
			queueIssues++
			if is.JobName == "" {
				t.Errorf("queue issue %d has no job name", is.ID)
			}
		}
	}
	if queueIssues != 2 {
		t.Fatalf("%d queue issues, want 2", queueIssues)
	}

	// The overview counts what was ingested.
	a.LoginAdmin()
	page := a.Get("/")
	if !contains(page.Body, ">6<") || !contains(page.Body, "Failed jobs") {
		t.Fatalf("overview does not show the new counts:\n%s", page.Body)
	}
}

func TestQueueFailureIsGroupedByJob(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Workers")

	for _, job := range []string{"App\\Jobs\\SyncInvoiceToXero", "App\\Jobs\\SyncInvoiceToXero", "App\\Jobs\\SyncContactToXero"} {
		ev := testutil.Stamp(testutil.Fixture(t, "job_timeout"))
		ev["job"].(map[string]any)["name"] = job
		a.PostEvent(queuePath, key, ev)
	}
	a.WaitProcessed()

	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 10)
	if len(issues) != 2 {
		t.Fatalf("%d issues; the same exception in two jobs must make two issues", len(issues))
	}
	for _, is := range issues {
		qf, _ := a.Store().IssueQueueFailures(t.Context(), is.ID, 10)
		if int64(len(qf)) != is.OccurrenceCount {
			t.Fatalf("issue %s: %d queue rows for %d occurrences", is.JobName, len(qf), is.OccurrenceCount)
		}
		if strings.Contains(qf[0].Payload, "xero-secret-token") {
			t.Fatalf("job payload not scrubbed: %s", qf[0].Payload)
		}
	}
}

func TestRegressionReopensResolvedIssue(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "type_error")))
	a.WaitProcessed()
	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 1)
	if err := a.Store().SetIssueStatus(t.Context(), issues[0].ID, "resolved"); err != nil {
		t.Fatal(err)
	}

	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "type_error")))
	a.WaitProcessed()
	got, _ := a.Store().Issue(t.Context(), issues[0].ID)
	if got.Status != "unresolved" || got.OccurrenceCount != 2 {
		t.Fatalf("after regression: status %s, count %d", got.Status, got.OccurrenceCount)
	}
}

func TestSensitiveDataIsScrubbed(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "unique_violation")))
	a.WaitProcessed()
	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 1)
	events, _ := a.Store().IssueEvents(t.Context(), issues[0].ID, 1)
	stored := events[0].RequestContext + events[0].EventContext + issues[0].SampleContext
	for _, secret := range []string{"hunter2hunter2", "secret-token", "laravel_session"} {
		if strings.Contains(stored, secret) {
			t.Fatalf("stored data contains %q", secret)
		}
	}
	if !contains(stored, "Mozilla/5.0") || !contains(stored, "jane.doe@example.com") {
		t.Fatal("scrubbing removed non-sensitive data")
	}
}

func TestIngestRejections(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	cfg.MaxEventBytes = 64 << 10
	a := testutil.NewAppWithConfig(t, cfg)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop", "production")
	valid := func() []byte { return testutil.MustJSON(t, testutil.Stamp(testutil.Fixture(t, "model_not_found"))) }

	staging := testutil.Stamp(testutil.Fixture(t, "model_not_found"))
	staging["environment"] = "staging"
	noTimestamp := testutil.Fixture(t, "model_not_found")
	noTimestamp["event_id"] = testutil.NewEventID()
	huge := testutil.Stamp(testutil.Fixture(t, "model_not_found"))
	huge["padding"] = strings.Repeat("x", 70<<10)

	tests := []struct {
		name    string
		path    string
		key     string
		body    []byte
		headers []string
		status  int
		code    string
	}{
		{"no key", eventsPath, "", valid(), nil, http.StatusUnauthorized, "missing_api_key"},
		{"not json content type", eventsPath, key, valid(), []string{"Content-Type", "text/plain"}, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"broken json", eventsPath, key, []byte(`{"event_id":`), nil, http.StatusBadRequest, "invalid_json"},
		{"two objects", eventsPath, key, append(valid(), valid()...), nil, http.StatusBadRequest, "invalid_json"},
		{"too large", eventsPath, key, testutil.MustJSON(t, huge), nil, http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"missing timestamp", eventsPath, key, testutil.MustJSON(t, noTimestamp), nil, http.StatusUnprocessableEntity, "validation_failed"},
		{"environment not configured", eventsPath, key, testutil.MustJSON(t, staging), nil, http.StatusUnprocessableEntity, "validation_failed"},
		{"queue failure without job", queuePath, key, valid(), nil, http.StatusUnprocessableEntity, "validation_failed"},
		{"GET on ingest endpoint", eventsPath, key, nil, nil, http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp testutil.Response
			if tt.body == nil {
				resp = a.API(http.MethodGet, tt.path, tt.key)
			} else {
				resp = a.Post(tt.path, tt.key, tt.body, tt.headers...)
			}
			if resp.StatusCode != tt.status || !contains(resp.Body, `"code":"`+tt.code+`"`) {
				t.Fatalf("status %d body %s; want %d %s", resp.StatusCode, resp.Body, tt.status, tt.code)
			}
		})
	}

	// 422 responses explain every problem.
	resp := a.Post(eventsPath, key, testutil.MustJSON(t, staging))
	body := testutil.DecodeJSON[api.ValidationErrorBody](t, strings.NewReader(resp.Body))
	if len(body.Error.Details) != 1 || body.Error.Details[0].Field != "environment" {
		t.Fatalf("details = %+v", body.Error.Details)
	}

	a.WaitProcessed()
	if m := a.Metrics(); m.Accepted != 0 || m.RejectedTotal < 8 {
		t.Fatalf("metrics = %+v", m)
	}
}

func TestGzipBodiesAreAccepted(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(testutil.MustJSON(t, testutil.Stamp(testutil.Fixture(t, "unique_violation"))))
	_ = zw.Close()
	resp := a.Post(eventsPath, key, buf.Bytes(), "Content-Encoding", "gzip")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("gzip event: %d %s", resp.StatusCode, resp.Body)
	}

	// A small gzip body that inflates past the limit is refused.
	buf.Reset()
	zw = gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"pad":"` + strings.Repeat("A", 2<<20) + `"}`))
	_ = zw.Close()
	if buf.Len() > 64<<10 {
		t.Fatalf("test bomb is not small: %d bytes", buf.Len())
	}
	resp = a.Post(eventsPath, key, buf.Bytes(), "Content-Encoding", "gzip")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip bomb: %d %s", resp.StatusCode, resp.Body)
	}

	a.WaitProcessed()
	if issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 5); len(issues) != 1 {
		t.Fatalf("%d issues", len(issues))
	}
}

func TestPerProjectRateLimit(t *testing.T) {
	t.Parallel()
	cfg := testutil.Config(t)
	cfg.RateLimit, cfg.RateBurst = 1, 3
	a := testutil.NewAppWithConfig(t, cfg)
	_, noisy := testutil.NewProjectKey(t, a.Store(), "Noisy")
	_, quiet := testutil.NewProjectKey(t, a.Store(), "Quiet")

	for range 3 {
		a.PostEvent(eventsPath, noisy, testutil.Stamp(testutil.Fixture(t, "model_not_found")))
	}
	resp := a.Post(eventsPath, noisy, testutil.MustJSON(t, testutil.Stamp(testutil.Fixture(t, "model_not_found"))))
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("4th event: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	a.PostEvent(eventsPath, quiet, testutil.Stamp(testutil.Fixture(t, "model_not_found")))
}

func TestDisabledProjectCannotIngest(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Off")
	if err := a.Store().SetProjectStatus(t.Context(), p.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	resp := a.Post(eventsPath, key, testutil.MustJSON(t, testutil.Stamp(testutil.Fixture(t, "model_not_found"))))
	if resp.StatusCode != http.StatusForbidden || !contains(resp.Body, "project_disabled") {
		t.Fatalf("status %d body %s", resp.StatusCode, resp.Body)
	}
}

func TestReadyReportsQueue(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	resp := a.API(http.MethodGet, "/ready", "")
	if resp.StatusCode != http.StatusOK || !contains(resp.Body, `"queue":"ok"`) {
		t.Fatalf("ready: %d %s", resp.StatusCode, resp.Body)
	}
}
