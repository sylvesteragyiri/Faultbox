package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// seedQueue stores failed jobs across two projects, plus one request
// exception that must never show up in the queue views.
func seedQueue(t *testing.T, st *store.Store) (shop, billing store.Project) {
	t.Helper()
	shop, _ = testutil.NewProjectKey(t, st, "Shop")
	billing, _ = testutil.NewProjectKey(t, st, "Billing")
	now := time.Now().UTC()

	type spec struct {
		project    store.Project
		fp, job    string
		kind       string
		queue      string
		connection string
		env        string
		attempts   int
		count      int
		ago        time.Duration
	}
	specs := []spec{
		{shop, "q-sync", "App\\Jobs\\SyncInvoice", "terminal", "integrations", "redis", "production", 3, 4, time.Hour},
		{shop, "q-sync", "App\\Jobs\\SyncInvoice", "attempt", "integrations", "redis", "production", 1, 6, 2 * time.Hour},
		{shop, "q-mail", "App\\Jobs\\SendReceipt", "manual", "mail", "sqs", "staging", 1, 2, 3 * time.Hour},
		{billing, "q-charge", "App\\Jobs\\ChargeCard", "terminal", "billing", "database", "production", 5, 3, 40 * 24 * time.Hour},
	}
	var batch []store.NewEvent
	for _, sp := range specs {
		for i := range sp.count {
			e := newEvent(sp.project.ID, sp.fp, now.Add(-sp.ago).Add(time.Duration(i)*time.Second))
			e.Category, e.Environment, e.ExceptionType = store.CategoryQueue, sp.env, "RuntimeException"
			attempts := sp.attempts
			e.Job = &store.NewQueueFailure{Kind: sp.kind, Name: sp.job, Queue: sp.queue, Connection: sp.connection,
				Attempts: &attempts, Payload: `{"invoice_id":42}`}
			batch = append(batch, e)
		}
	}
	batch = append(batch, newEvent(shop.ID, "http-error", now.Add(-time.Minute)))
	if _, err := st.IngestBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	return shop, billing
}

func jobNames(t *testing.T, st *store.Store, f store.QueueFilter) []string {
	t.Helper()
	page, err := st.ListFailedJobs(context.Background(), f)
	if err != nil {
		t.Fatalf("ListFailedJobs(%+v): %v", f, err)
	}
	var names []string
	for _, j := range page.Jobs {
		names = append(names, j.JobName+"/"+j.Kind)
	}
	return names
}

func TestFailedJobListFilters(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	shop, billing := seedQueue(t, st)

	count := func(f store.QueueFilter) int { return len(jobNames(t, st, f)) }
	cases := []struct {
		name string
		f    store.QueueFilter
		want int
	}{
		{"default excludes retries", store.QueueFilter{}, 4 + 2 + 3},
		{"retries only", store.QueueFilter{Kind: store.QueueKindAttempts}, 6},
		{"all kinds", store.QueueFilter{Kind: store.QueueKindAll}, 15},
		{"project", store.QueueFilter{ProjectID: billing.ID}, 3},
		{"queue", store.QueueFilter{Queue: "mail"}, 2},
		{"connection", store.QueueFilter{Connection: "redis", Kind: store.QueueKindAll}, 10},
		{"job class", store.QueueFilter{JobName: "App\\Jobs\\SyncInvoice"}, 4},
		{"environment", store.QueueFilter{Environment: "staging"}, 2},
		{"since", store.QueueFilter{Since: time.Now().Add(-24 * time.Hour)}, 6},
		{"combined", store.QueueFilter{ProjectID: shop.ID, Connection: "redis", Environment: "production"}, 4},
		{"no match", store.QueueFilter{ProjectID: billing.ID, Queue: "mail"}, 0},
	}
	for _, c := range cases {
		if got := count(c.f); got != c.want {
			t.Errorf("%s: %d failed jobs, want %d", c.name, got, c.want)
		}
	}

	// Newest first, and request exceptions never appear.
	all := jobNames(t, st, store.QueueFilter{Kind: store.QueueKindAll, PerPage: 100})
	if all[0] != "App\\Jobs\\SyncInvoice/terminal" || all[len(all)-1] != "App\\Jobs\\ChargeCard/terminal" {
		t.Errorf("order: first %s, last %s", all[0], all[len(all)-1])
	}
	for _, n := range all {
		if !strings.HasPrefix(n, "App\\Jobs\\") {
			t.Errorf("non-job row in the failed-job list: %s", n)
		}
	}

	// Paging.
	p1, _ := st.ListFailedJobs(context.Background(), store.QueueFilter{Kind: store.QueueKindAll, PerPage: 10})
	p2, _ := st.ListFailedJobs(context.Background(), store.QueueFilter{Kind: store.QueueKindAll, PerPage: 10, Page: 2})
	if len(p1.Jobs) != 10 || !p1.HasNext || len(p2.Jobs) != 5 || p2.HasNext {
		t.Errorf("paging: page 1 %d (next %v), page 2 %d (next %v)", len(p1.Jobs), p1.HasNext, len(p2.Jobs), p2.HasNext)
	}
}

func TestFailedJobEnvironmentIsStored(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	seedQueue(t, st)
	facets, err := st.FailedJobFacets(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(facets.Environments, []string{"production", "staging"}) {
		t.Errorf("environments %v", facets.Environments)
	}
	if !slices.Equal(facets.Connections, []string{"redis", "database", "sqs"}) {
		t.Errorf("connections %v, want most frequent first", facets.Connections)
	}
	if len(facets.Queues) != 3 || len(facets.JobNames) != 3 {
		t.Errorf("queues %v, jobs %v", facets.Queues, facets.JobNames)
	}
}

func TestQueueDashboardQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	shop, _ := seedQueue(t, st)

	sum, err := st.QueueSummary(ctx, 0, 30)
	if err != nil {
		t.Fatal(err)
	}
	// The 40-day-old ChargeCard failures fall outside the window, but their
	// issue still counts as unresolved.
	if sum.Failures != 6 || sum.Attempts != 6 || sum.JobTypes != 2 || sum.Unresolved != 3 {
		t.Errorf("summary %+v", sum)
	}
	if s, _ := st.QueueSummary(ctx, shop.ID, 30); s.Unresolved != 2 {
		t.Errorf("project summary %+v", s)
	}

	top, err := st.TopFailingJobs(ctx, 0, 30, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].JobName != "App\\Jobs\\SyncInvoice" || top[0].Failures != 4 || top[0].Attempts != 6 {
		t.Errorf("top jobs %+v", top)
	}

	queues, err := st.QueueBreakdown(ctx, 0, 30, "queue")
	if err != nil {
		t.Fatal(err)
	}
	if len(queues) != 2 || queues[0].Name != "integrations" || queues[0].Count != 10 {
		t.Errorf("queue breakdown %+v", queues)
	}
	conns, _ := st.QueueBreakdown(ctx, 0, 30, "connection")
	if len(conns) != 2 || conns[1].Name != "sqs" {
		t.Errorf("connection breakdown %+v", conns)
	}

	rates, err := st.QueueFailureRates(ctx, shop.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Shop production: 10 failed-job events and 1 request exception.
	if len(rates) != 2 || rates[0].Environment != "production" || rates[0].QueueFailures != 10 || rates[0].Events != 11 || rates[0].Percent() != 91 {
		t.Errorf("rates %+v", rates)
	}

	trend, err := st.QueueTrend(ctx, 0, 30)
	if err != nil {
		t.Fatal(err)
	}
	var failures, attempts int64
	for _, d := range trend {
		failures += d.Errors
		attempts += d.QueueFailures
	}
	if len(trend) != 30 || failures != 6 || attempts != 6 {
		t.Errorf("trend: %d days, %d failures, %d retries", len(trend), failures, attempts)
	}
}

func TestFailedJobDetail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	shop, _ := seedQueue(t, st)
	page, _ := st.ListFailedJobs(ctx, store.QueueFilter{Queue: "mail"})
	d, err := st.FailedJob(ctx, page.Jobs[0].EventID)
	if err != nil {
		t.Fatal(err)
	}
	if d.JobName != "App\\Jobs\\SendReceipt" || d.Kind != "manual" || d.Connection != "sqs" || d.Environment != "staging" ||
		d.Payload != `{"invoice_id":42}` || d.Event.UUID == "" {
		t.Errorf("detail %+v", d)
	}
	if _, err := st.FailedJob(ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing job: %v", err)
	}
	history, err := st.JobFailures(ctx, shop.ID, "App\\Jobs\\SyncInvoice", 5)
	if err != nil || len(history) != 5 {
		t.Errorf("history: %d rows, %v", len(history), err)
	}
}

func TestFailedJobListQueryPlansUseIndexes(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	shop, _ := seedQueue(t, st)
	filters := []store.QueueFilter{
		{},
		{Kind: store.QueueKindAll},
		{ProjectID: shop.ID},
		{Queue: "mail"},
		{Connection: "redis"},
		{JobName: "App\\Jobs\\SyncInvoice"},
		{Environment: "staging"},
		{ProjectID: shop.ID, Queue: "mail"},
		{ProjectID: shop.ID, Connection: "redis"},
		{ProjectID: shop.ID, JobName: "App\\Jobs\\SyncInvoice"},
		{ProjectID: shop.ID, Environment: "production"},
		{Since: time.Now().Add(-time.Hour)},
	}
	for _, f := range filters {
		plan, err := st.ExplainFailedJobList(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		for _, line := range plan {
			if line == "SCAN q" {
				t.Errorf("filter %+v scans queue_failures without an index:\n%s", f, joined)
			}
		}
		if strings.Contains(joined, "USE TEMP B-TREE") {
			t.Errorf("filter %+v sorts instead of reading an index in order:\n%s", f, joined)
		}
	}
}
