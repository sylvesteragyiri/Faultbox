package main

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/ingest"
)

// Every seeded job run must pass the API's validation, and each dispatch's
// attempts must look like the SDK's: numbered from 1, one job UUID, a wait
// on the first attempt only, and a stop at the first success or last try.
func TestSeededJobRunsAreValid(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic demo data, not security
	now := time.Now().UTC()
	hour := now.Add(-3 * time.Hour).Truncate(time.Hour)
	for pi, spec := range telemetryFor {
		for _, j := range spec.jobs {
			for range 200 {
				runs := j.runs(rng, hour, now, 0.5) // a high failure rate exercises retries
				if len(runs) == 0 || len(runs) > max(1, j.tries) {
					t.Fatalf("%s: %d attempts for %d tries", j.job, len(runs), j.tries)
				}
				for i, r := range runs {
					if r.Attempt != i+1 || r.UUID != runs[0].UUID || (r.WaitMs != nil) != (i == 0) {
						t.Fatalf("%s attempt %d: %+v", j.job, i+1, r)
					}
					last := i == len(runs)-1
					switch {
					case r.Outcome == "succeeded" && !last:
						t.Fatalf("%s: an attempt after a success", j.job)
					case r.Outcome == "failed" && r.Attempt != j.tries:
						t.Fatalf("%s: failed before its last try", j.job)
					}
				}
				b := &ingest.TelemetryBatch{BatchID: newUUID(), Environment: "production", Jobs: runs}
				if _, err := ingest.ValidateTelemetry(b, projects[pi].envs, hour.Add(2*time.Hour)); err != nil {
					t.Fatalf("%s: %v", j.job, err)
				}
			}
		}
	}
}
