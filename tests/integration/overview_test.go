package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// TestDailyTotalsMatchIssueCounts checks the overview rollup against the
// per-issue daily counts it replaces, for errors and failed jobs.
func TestDailyTotalsMatchIssueCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	shop, _ := testutil.NewProjectKey(t, st, "Shop")
	jobs, _ := testutil.NewProjectKey(t, st, "Jobs")
	now := time.Now().UTC()

	var batch []store.NewEvent
	for d := range 10 {
		at := now.Add(-time.Duration(d) * 24 * time.Hour)
		for range d + 1 { // d+1 errors on day -d
			batch = append(batch, newEvent(shop.ID, "fp-err", at))
		}
		if d%3 == 0 {
			e := newEvent(jobs.ID, "fp-job", at)
			e.Category = store.CategoryQueue
			e.Job = &store.NewQueueFailure{Kind: "terminal", Name: "App\\Jobs\\Sync"}
			batch = append(batch, e)
		}
	}
	if _, err := st.IngestBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	trend, err := st.Trend(ctx, 14, 0)
	if err != nil {
		t.Fatal(err)
	}
	var errs, queue int64
	for _, d := range trend {
		errs += d.Errors
		queue += d.QueueFailures
	}
	if errs != 55 || queue != 4 { // 1+2+…+10 errors; failed jobs on days 0, 3, 6, 9
		t.Fatalf("trend totals: %d errors, %d failed jobs; want 55 and 4", errs, queue)
	}
	if last := trend[len(trend)-1]; last.Errors != 1 || last.QueueFailures != 1 {
		t.Errorf("today: %+v", last)
	}
	shopOnly, _ := st.Trend(ctx, 14, shop.ID)
	var shopSum int64
	for _, d := range shopOnly {
		shopSum += d.Errors + d.QueueFailures
	}
	if shopSum != 55 {
		t.Errorf("project filter: %d events, want 55", shopSum)
	}

	activity, err := st.ProjectsActivity(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	s := activity[shop.ID]
	// Days 0–6 hold 1+…+7 = 28 events; days 7–9 hold 8+9+10 = 27.
	if s == nil || s.Events != 28 || s.Previous != 27 || s.Change() != 1 || s.Unresolved != 1 || len(s.Days) != 7 || s.Days[6] != 1 {
		t.Errorf("shop activity %+v", s)
	}
	if j := activity[jobs.ID]; j == nil || j.Events != 3 || j.Previous != 1 {
		t.Errorf("jobs activity %+v", j)
	}
}
