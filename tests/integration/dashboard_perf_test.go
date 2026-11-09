package integration

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// TestDashboardPerformance seeds a realistic database and times every
// dashboard page against the D15 target (p95 under 300 ms). Opt-in:
//
//	FAULTBOX_SEED=1 go test -run TestDashboardPerformance -v -timeout 30m ./tests/integration/
//	FAULTBOX_SEED=1000000 ...   (number of events; default 200,000)
func TestDashboardPerformance(t *testing.T) {
	if os.Getenv("FAULTBOX_SEED") == "" {
		t.Skip("set FAULTBOX_SEED=1 (or an event count) to run")
	}
	events, _ := strconv.Atoi(os.Getenv("FAULTBOX_SEED"))
	if events < 1000 {
		events = 200_000
	}
	const (
		issues   = 5000
		projects = 3
		target   = 300 * time.Millisecond
		runs     = 20
	)

	cfg := testutil.Config(t)
	cfg.DatabasePath = filepath.Join(t.TempDir(), "seed.db")
	st, err := store.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	var projectIDs []int64
	for i := range projects {
		p, _ := testutil.NewProjectKey(t, st, fmt.Sprintf("Project %d", i+1))
		projectIDs = append(projectIDs, p.ID)
	}

	start := time.Now()
	rng := rand.New(rand.NewPCG(1, 2))
	zipf := rand.NewZipf(rand.New(rand.NewPCG(3, 4)), 1.1, 1, issues-1) // long tail of issue sizes
	types := []string{"Illuminate\\Database\\QueryException", "ErrorException", "TypeError",
		"Illuminate\\Database\\Eloquent\\ModelNotFoundException", "RuntimeException", "GuzzleHttp\\Exception\\ConnectException",
		"App\\Exceptions\\PaymentFailed", "LogicException"}
	envs := []string{"production", "production", "production", "staging", "local"}
	now := time.Now().UTC()
	batch := make([]store.NewEvent, 0, 2000)
	for n := range events {
		k := int(zipf.Uint64())
		e := newEvent(projectIDs[k%projects], fmt.Sprintf("%064x", k), now.Add(-time.Duration(rng.Int64N(int64(30*24*time.Hour)))))
		e.ExceptionType = types[k%len(types)]
		e.Message = fmt.Sprintf("Something failed in handler %d while processing order", k)
		e.Culprit = fmt.Sprintf("App\\Http\\Controllers\\Controller%d::handle", k%400)
		e.Environment = envs[k%len(envs)]
		e.Release = "r" + strconv.Itoa(k%12)
		e.RequestURL = fmt.Sprintf("https://app.test/orders/%d/items", k%900)
		e.Stacktrace = `{"frames":[{"file":"/var/www/app/Http/Controllers/OrderController.php","line":42,"function":"store","class":"App\\Http\\Controllers\\OrderController"},{"file":"/var/www/vendor/laravel/framework/src/Illuminate/Routing/Controller.php","line":54,"function":"callAction"}]}`
		e.RequestContext = `{"url":"` + e.RequestURL + `","method":"POST","status_code":500,"headers":{"User-Agent":"Mozilla/5.0"}}`
		if k%10 == 0 {
			e.Category = store.CategoryQueue
			e.Job = &store.NewQueueFailure{Kind: "terminal", Name: fmt.Sprintf("App\\Jobs\\Job%d", k%50), Queue: "default"}
		}
		batch = append(batch, e)
		if len(batch) == cap(batch) || n == events-1 {
			if _, err := st.IngestBatch(context.Background(), batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	seeded := time.Since(start)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(cfg.DatabasePath)
	t.Logf("seeded %d events into %d issues in %v (%.0f events/s); database %.0f MB",
		events, issues, seeded.Round(time.Second), float64(events)/seeded.Seconds(), float64(fi.Size())/1e6)

	a := testutil.NewAppWithConfig(t, cfg)
	a.LoginAdmin()
	big, err := a.Store().ListIssues(context.Background(), store.IssueFilter{Sort: store.SortCount, Category: store.CategoryAll, PerPage: 1})
	if err != nil || len(big.Issues) == 0 {
		t.Fatal(err)
	}
	bigID := strconv.FormatInt(big.Issues[0].ID, 10)

	pages := []string{
		"/",
		"/issues",
		"/issues?sort=count",
		"/issues?sort=first_seen&status=all",
		"/issues?category=queue_failure&status=all",
		"/issues?project=" + strconv.FormatInt(projectIDs[0], 10),
		"/issues?env=staging",
		"/issues?release=r5",
		"/issues?type=TypeError",
		"/issues?range=24h",
		"/issues?q=Controller12",
		"/issues?q=orders%2F5",
		"/issues?page=40",
		"/issues/" + bigID,
		"/issues/" + bigID + "?events=200",
		"/queue-failures",
		"/queue-failures?project=" + strconv.FormatInt(projectIDs[0], 10),
		"/queue-failures/jobs",
		"/queue-failures/jobs?kind=all&page=40",
		"/queue-failures/jobs?queue=default",
		"/queue-failures/jobs?job=" + url.QueryEscape(`App\Jobs\Job7`),
	}
	var failed []string
	for _, path := range pages {
		var times []time.Duration
		for range runs {
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, a.Server.URL+path, nil)
			t0 := time.Now()
			resp := testutil.Do(t, a.Client, req)
			times = append(times, time.Since(t0))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d", path, resp.StatusCode)
			}
		}
		slices.Sort(times)
		p50, p95 := times[len(times)/2], times[len(times)*95/100]
		t.Logf("%-48s p50 %6.1f ms  p95 %6.1f ms", path, ms(p50), ms(p95))
		if p95 > target {
			failed = append(failed, path)
		}
	}
	if len(failed) > 0 {
		t.Fatalf("pages over the %v p95 target: %v", target, failed)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
