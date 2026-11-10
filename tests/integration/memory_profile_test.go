package integration

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// TestMemoryProfile measures Go heap use while the real HTTP ingestion path is
// under a representative load or a burst of near-limit events. It is opt-in:
//
//	FAULTBOX_MEMORY_PROFILE=normal go test -run TestMemoryProfile -v ./tests/integration/
//	FAULTBOX_MEMORY_PROFILE=worst  go test -run TestMemoryProfile -v ./tests/integration/
//
// The test server and load generator share a process, so the reported heap
// delta includes the in-process HTTP client as well as Faultbox.
func TestMemoryProfile(t *testing.T) {
	profile := strings.ToLower(strings.TrimSpace(getenv("FAULTBOX_MEMORY_PROFILE")))
	if profile == "" {
		t.Skip("set FAULTBOX_MEMORY_PROFILE=normal or worst to measure memory")
	}

	n, workers := 1000, 8
	if profile == "worst" {
		n, workers = 128, 96
	} else if profile != "normal" {
		t.Fatalf("unknown FAULTBOX_MEMORY_PROFILE %q; use normal or worst", profile)
	}

	a := testutil.NewApp(t)
	project, key := testutil.NewProjectKey(t, a.Store(), "Memory profile")
	body := memoryProfileBody(t, profile == "worst")
	if len(body) > 256<<10 {
		t.Fatalf("profile event is %d bytes, above the 256 KiB API limit", len(body))
	}

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peakHeap atomic.Uint64
	peakHeap.Store(baseline.HeapAlloc)
	stopSampling := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				for old := peakHeap.Load(); current.HeapAlloc > old; old = peakHeap.Load() {
					if peakHeap.CompareAndSwap(old, current.HeapAlloc) {
						break
					}
				}
			}
		}
	}()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: workers}}
	jobs := make(chan int)
	var accepted atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				eventID := []byte(fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
				requestBody := bytes.Replace(body, []byte("00000000-0000-4000-8000-000000000000"), eventID, 1)
				for {
					status := a.PostRaw(client, "/api/v1/events", key, requestBody)
					if status == http.StatusAccepted {
						accepted.Add(1)
						break
					}
					if status != http.StatusServiceUnavailable && status != http.StatusTooManyRequests {
						t.Errorf("unexpected response status %d", status)
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}
	for i := range n {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	a.WaitProcessed()
	close(stopSampling)
	sampler.Wait()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if accepted.Load() != int64(n) || a.Metrics().Processed != int64(n) {
		t.Fatalf("accepted %d and processed %d events; want %d", accepted.Load(), a.Metrics().Processed, n)
	}
	if project.ID == 0 {
		t.Fatal("test project was not created")
	}
	t.Logf("profile=%s events=%d workers=%d event_body=%d KiB elapsed=%s", profile, n, workers, len(body)/1024, time.Since(start).Round(time.Millisecond))
	t.Logf("heap baseline=%.1f MiB sampled_peak=%.1f MiB after_gc=%.1f MiB peak_delta=%.1f MiB", bytesToMiB(baseline.HeapAlloc), bytesToMiB(peakHeap.Load()), bytesToMiB(after.HeapAlloc), bytesToMiB(max(peakHeap.Load()-baseline.HeapAlloc, 0)))
	t.Logf("heap_inuse_after=%.1f MiB heap_sys_after=%.1f MiB total_sys_after=%.1f MiB queue_budget=32 MiB", bytesToMiB(after.HeapInuse), bytesToMiB(after.HeapSys), bytesToMiB(after.Sys))
}

func memoryProfileBody(t *testing.T, worst bool) []byte {
	t.Helper()
	payload := testutil.Fixture(t, "type_error")
	payload["event_id"] = "00000000-0000-4000-8000-000000000000"
	payload["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	if worst {
		frames := make([]any, 180)
		for i := range frames {
			frames[i] = map[string]any{
				"file":     "/app/" + strings.Repeat("f", 900),
				"line":     i + 1,
				"function": "handleMemoryProfile",
				"class":    "App\\MemoryProfileController",
			}
		}
		exception := testutil.Exception(payload)
		exception["frames"] = frames
		payload["context"] = map[string]any{"sample": strings.Repeat("c", 28_000)}
	}
	return testutil.MustJSON(t, payload)
}

func bytesToMiB(n uint64) float64 { return float64(n) / (1024 * 1024) }

func getenv(key string) string { return strings.TrimSpace(os.Getenv(key)) }
