package integration

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// TestIngestLoad pushes events through the full HTTP pipeline concurrently.
// It is opt-in because it takes a few seconds:
//
//	FAULTBOX_LOAD=1 go test -run TestIngestLoad -v ./tests/integration/
//	FAULTBOX_LOAD=20000 ...   (number of events)
func TestIngestLoad(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("FAULTBOX_LOAD"))
	if os.Getenv("FAULTBOX_LOAD") == "" {
		t.Skip("set FAULTBOX_LOAD=1 (or an event count) to run")
	}
	if n < 100 {
		n = 5000
	}
	const workers = 32

	a := testutil.NewApp(t)
	p, key := testutil.NewProjectKey(t, a.Store(), "Load")

	// Pre-build bodies so the client side doesn't dominate the measurement.
	bodies := make([][]byte, n)
	for i := range bodies {
		ev := testutil.Stamp(testutil.Fixture(t, testutil.FixtureNames[i%4])) // the four HTTP fixtures
		bodies[i] = testutil.MustJSON(t, ev)
	}

	var sent, retried atomic.Int64
	jobs := make(chan []byte)
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for body := range jobs {
				for {
					resp := a.PostRaw(client, "/api/v1/events", key, body)
					if resp == http.StatusAccepted {
						sent.Add(1)
						break
					}
					if resp != http.StatusServiceUnavailable && resp != http.StatusTooManyRequests {
						t.Errorf("unexpected status %d", resp)
						return
					}
					retried.Add(1)
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
	for _, b := range bodies {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
	accepted := time.Since(start)
	a.WaitProcessed()
	stored := time.Since(start)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m := a.Metrics()
	issues, _ := a.Store().ProjectIssues(t.Context(), p.ID, 10)
	var total int64
	for _, is := range issues {
		total += is.OccurrenceCount
	}

	t.Logf("%d events from %d workers: accepted in %v (%.0f/s), stored in %v (%.0f/s)",
		n, workers, accepted.Round(time.Millisecond), float64(n)/accepted.Seconds(),
		stored.Round(time.Millisecond), float64(n)/stored.Seconds())
	t.Logf("503/429 retries: %d · batches: %d · avg latency %v · max %v · last batch write %v",
		retried.Load(), m.Batches, m.AvgLatency.Round(time.Millisecond), m.MaxLatency.Round(time.Millisecond), m.LastBatchWrite.Round(time.Millisecond))
	t.Logf("Go heap in use %.1f MB · heap sys %.1f MB · total sys %.1f MB",
		float64(ms.HeapInuse)/1e6, float64(ms.HeapSys)/1e6, float64(ms.Sys)/1e6)

	if sent.Load() != int64(n) || m.Processed != int64(n) || total != int64(n) || len(issues) != 4 {
		t.Fatalf("sent %d, processed %d, stored %d in %d issues; want %d in 4", sent.Load(), m.Processed, total, len(issues), n)
	}
}
