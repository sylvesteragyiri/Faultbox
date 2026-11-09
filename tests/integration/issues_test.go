package integration

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// seedIssues stores a small, varied set of issues across two projects.
func seedIssues(t *testing.T, st *store.Store) (shop, workers store.Project, ids map[string]int64) {
	t.Helper()
	ctx := context.Background()
	shop, _ = testutil.NewProjectKey(t, st, "Shop")
	workers, _ = testutil.NewProjectKey(t, st, "Workers")
	now := time.Now().UTC()

	type spec struct {
		key, fp, typ, msg, culprit, env, release, url string
		project                                       store.Project
		count                                         int
		ago                                           time.Duration
		job                                           string
	}
	specs := []spec{
		{"order", "aa11aa11", "Illuminate\\Database\\Eloquent\\ModelNotFoundException", "No query results for model [App\\Models\\Order] <n>",
			"App\\Http\\Controllers\\OrderController::show", "production", "1.0", "https://shop.test/orders/5?token=x", shop, 5, time.Hour, ""},
		{"checkout", "bb22bb22", "ErrorException", "Undefined array key \"shipping_address\"",
			"App\\Services\\CheckoutService::buildShipment", "production", "1.1", "https://shop.test/checkout", shop, 12, 2 * time.Hour, ""},
		{"staging", "cc33cc33", "TypeError", "PriceCalculator::total(): Argument #1 ($items) must be of type array",
			"App\\Services\\PriceCalculator::total", "staging", "1.1-rc.2", "https://staging.shop.test/cart", shop, 2, 40 * 24 * time.Hour, ""},
		{"job", "dd44dd44", "GuzzleHttp\\Exception\\ConnectException", "cURL error 28: Operation timed out",
			"App\\Services\\Xero\\InvoiceClient::push", "production", "1.1", "", workers, 3, 30 * time.Minute, "App\\Jobs\\SyncInvoiceToXero"},
		{"ab", "ee55ee55", "RuntimeException", "zq happened", "x", "production", "", "", workers, 1, 3 * time.Hour, ""},
	}
	ids = map[string]int64{}
	for _, sp := range specs {
		var batch []store.NewEvent
		for i := range sp.count {
			e := newEvent(sp.project.ID, sp.fp, now.Add(-sp.ago).Add(time.Duration(i)*time.Second))
			e.ExceptionType, e.Message, e.Culprit, e.Environment, e.Release, e.RequestURL =
				sp.typ, sp.msg, sp.culprit, sp.env, sp.release, sp.url
			if sp.job != "" {
				e.Category = store.CategoryQueue
				e.Job = &store.NewQueueFailure{Kind: "terminal", Name: sp.job, Queue: "integrations"}
			}
			batch = append(batch, e)
		}
		r, err := st.IngestBatch(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		ids[sp.key] = r[0].IssueID
	}
	return shop, workers, ids
}

func listKeys(t *testing.T, st *store.Store, ids map[string]int64, f store.IssueFilter) []string {
	t.Helper()
	page, err := st.ListIssues(context.Background(), f)
	if err != nil {
		t.Fatalf("ListIssues(%+v): %v", f, err)
	}
	byID := map[int64]string{}
	for k, id := range ids {
		byID[id] = k
	}
	var keys []string
	for _, is := range page.Issues {
		keys = append(keys, byID[is.ID])
	}
	return keys
}

func TestIssueListFilters(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	shop, workers, ids := seedIssues(t, st)
	if err := st.SetIssueStatus(context.Background(), ids["checkout"], store.IssueResolved); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		f    store.IssueFilter
		want []string
	}{
		{"default: errors by last seen", store.IssueFilter{}, []string{"order", "checkout", "ab", "staging"}},
		{"unresolved only", store.IssueFilter{Status: "unresolved"}, []string{"order", "ab", "staging"}},
		{"resolved only", store.IssueFilter{Status: "resolved"}, []string{"checkout"}},
		{"failed jobs", store.IssueFilter{Category: store.CategoryQueue}, []string{"job"}},
		{"everything", store.IssueFilter{Category: store.CategoryAll}, []string{"job", "order", "checkout", "ab", "staging"}},
		{"project", store.IssueFilter{ProjectID: workers.ID, Category: store.CategoryAll}, []string{"job", "ab"}},
		{"environment", store.IssueFilter{Environment: "staging"}, []string{"staging"}},
		{"release", store.IssueFilter{Release: "1.1", Category: store.CategoryAll}, []string{"job", "checkout"}},
		{"exception type", store.IssueFilter{ExceptionType: "TypeError"}, []string{"staging"}},
		{"seen in the last week", store.IssueFilter{SeenFrom: time.Now().Add(-7 * 24 * time.Hour)}, []string{"order", "checkout", "ab"}},
		{"most events first", store.IssueFilter{Sort: store.SortCount, ProjectID: shop.ID}, []string{"checkout", "order", "staging"}},
		{"newest first", store.IssueFilter{Sort: store.SortFirstSeen}, []string{"order", "checkout", "ab", "staging"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := listKeys(t, st, ids, tt.f); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIssueSearch(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	_, _, ids := seedIssues(t, st)
	all := store.IssueFilter{Category: store.CategoryAll}

	tests := []struct {
		q    string
		want []string
	}{
		{"NotFound", []string{"order"}},               // substring of the class
		{"modelnotfoundexception", []string{"order"}}, // case-insensitive
		{"shipping_address", []string{"checkout"}},    // message
		{"CheckoutService", []string{"checkout"}},     // culprit
		{"SyncInvoice", []string{"job"}},              // job name
		{"/orders/5", []string{"order"}},              // request path
		{"token=x", nil},                              // query strings are not indexed
		{"1.1-rc.2", []string{"staging"}},             // exact release
		{"bb22bb", []string{"checkout"}},              // fingerprint prefix (hex, 6+ chars)
		{"Order controller", []string{"order"}},       // every term must match
		{"zq", []string{"ab"}},                        // short term: LIKE fallback
		{`"; DROP TABLE issues; --`, nil},             // quoted safely
		{"definitely-not-there", nil},
	}
	for _, tt := range tests {
		f := all
		f.Search = tt.q
		if got := listKeys(t, st, ids, f); !slices.Equal(got, tt.want) {
			t.Errorf("search %q: got %v, want %v", tt.q, got, tt.want)
		}
	}
	if n, _ := st.CountUsers(context.Background()); n < 0 {
		t.Fatal("database damaged")
	}
}

func TestIssueListPagination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	p, _ := testutil.NewProjectKey(t, st, "Many")
	var batch []store.NewEvent
	base := time.Now().Add(-time.Hour)
	for i := range 30 {
		batch = append(batch, newEvent(p.ID, "fp-"+string(rune('a'+i/26))+string(rune('a'+i%26)), base.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := st.IngestBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	first, _ := st.ListIssues(ctx, store.IssueFilter{PerPage: 25})
	second, _ := st.ListIssues(ctx, store.IssueFilter{PerPage: 25, Page: 2})
	if len(first.Issues) != 25 || !first.HasNext || len(second.Issues) != 5 || second.HasNext {
		t.Fatalf("pages: %d (next %v), %d (next %v)", len(first.Issues), first.HasNext, len(second.Issues), second.HasNext)
	}
	if first.Issues[24].LastSeenAt.Before(second.Issues[0].LastSeenAt) {
		t.Fatal("pages overlap or are out of order")
	}
	if len(first.Issues[0].Trend) != store.TrendDays || first.Issues[0].Trend[store.TrendDays-1] != 1 {
		t.Fatalf("trend = %v", first.Issues[0].Trend)
	}
}

// Exit criterion: list queries are served by indexes.
func TestIssueListQueryPlansUseIndexes(t *testing.T) {
	t.Parallel()
	st := testutil.OpenStore(t)
	shop, _, _ := seedIssues(t, st)

	ordered := []store.IssueFilter{
		{Status: "unresolved"},
		{Status: "unresolved", Sort: store.SortCount},
		{Status: "unresolved", ProjectID: shop.ID},
		{Status: "unresolved", ProjectID: shop.ID, Sort: store.SortCount},
		{ProjectID: shop.ID, Sort: store.SortFirstSeen},
		{Sort: store.SortFirstSeen},
		{},
		{Sort: store.SortCount},
	}
	filtered := []store.IssueFilter{
		{Environment: "staging"},
		{Release: "1.1"},
		{ExceptionType: "TypeError"},
		{Search: "NotFound"},
		{Search: "bb22bb22"},
		{SeenFrom: time.Now().Add(-time.Hour)},
		{ProjectID: shop.ID, Environment: "production", Release: "1.0", Search: "Order"},
	}

	check := func(f store.IssueFilter, mustAvoidSort bool) {
		plan, err := st.ExplainIssueList(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(plan, "\n")
		for _, line := range plan {
			if line == "SCAN i" {
				t.Errorf("filter %+v scans the issues table without an index:\n%s", f, joined)
			}
		}
		if mustAvoidSort && strings.Contains(joined, "USE TEMP B-TREE") {
			t.Errorf("filter %+v sorts instead of reading an index in order:\n%s", f, joined)
		}
	}
	for _, f := range ordered {
		check(f, true)
	}
	for _, f := range filtered {
		check(f, false)
	}
}

func TestOverviewAndDetailQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	_, _, ids := seedIssues(t, st)

	trend, err := st.Trend(ctx, 30, 0)
	if err != nil || len(trend) != 30 {
		t.Fatalf("trend: %d days, %v", len(trend), err)
	}
	var errs, jobs int64
	for _, d := range trend {
		errs += d.Errors
		jobs += d.QueueFailures
	}
	// "staging" is 40 days old, outside the window.
	if errs != 5+12+1 || jobs != 3 {
		t.Fatalf("trend totals: %d errors, %d jobs", errs, jobs)
	}
	if !trend[29].Day.Equal(time.Now().UTC().Truncate(24 * time.Hour)) {
		t.Fatalf("last trend day = %v, want today", trend[29].Day)
	}

	failures, _ := st.RecentQueueFailures(ctx, 0, 5)
	if len(failures) != 3 || failures[0].JobName != "App\\Jobs\\SyncInvoiceToXero" {
		t.Fatalf("recent failures = %+v", failures)
	}

	envs, _ := st.IssueBreakdown(ctx, ids["checkout"], "environment")
	if len(envs) != 1 || envs[0].Name != "production" || envs[0].Count != 12 {
		t.Fatalf("environment breakdown = %+v", envs)
	}
	latest, err := st.IssueEventDetail(ctx, ids["order"], "")
	if err != nil || latest.RequestURL == "" {
		t.Fatalf("latest event = %+v, %v", latest, err)
	}
	byID, err := st.IssueEventDetail(ctx, ids["order"], latest.UUID)
	if err != nil || byID.ID != latest.ID {
		t.Fatalf("event by uuid: %+v, %v", byID, err)
	}
	events, more, _ := st.IssueEventList(ctx, ids["order"], 3, 0)
	if len(events) != 3 || !more {
		t.Fatalf("event list: %d, more %v", len(events), more)
	}
	facets, _ := st.IssueFacets(ctx, 0)
	if !slices.Contains(facets.Environments, "staging") || !slices.Contains(facets.Releases, "1.1-rc.2") ||
		!slices.Contains(facets.ExceptionTypes, "TypeError") {
		t.Fatalf("facets = %+v", facets)
	}
}
