package unit

import (
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/config"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
)

func jobItem(mod func(*ingest.JobItem)) ingest.JobItem {
	ms, wait, tries := int64(120), int64(40), 3
	it := ingest.JobItem{
		ID: "0192f3a4-5b6c-7d8e-9f01-23456789abcd", UUID: "0192f3a4-5b6c-7d8e-9f01-000000000001", JobID: "42",
		Attempt: 1, MaxTries: &tries, Job: `App\Jobs\SyncInvoice`, Kind: "job", Queue: "default", Connection: "redis",
		Outcome: "succeeded", DurationMs: &ms, WaitMs: &wait, StartedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
	}
	if mod != nil {
		mod(&it)
	}
	return it
}

func jobBatch(items ...ingest.JobItem) *ingest.TelemetryBatch {
	return &ingest.TelemetryBatch{BatchID: "0192f3a4-5b6c-7d8e-9f01-aaaaaaaaaaaa", Environment: "Production", Release: "1.2", Jobs: items}
}

func TestValidateJobRuns(t *testing.T) {
	t.Parallel()
	out, err := ingest.ValidateTelemetry(jobBatch(
		jobItem(nil),
		jobItem(func(it *ingest.JobItem) {
			it.ID, it.Kind, it.Job, it.Outcome, it.WaitMs, it.UUID, it.JobID, it.MaxTries = "0192F3A4-5B6C-7D8E-9F01-23456789ABCE", "", `\Closure`, "timed_out", nil, "", "", nil
			it.Queue = "https://sqs.eu-west-1.amazonaws.com/123456789012/default" // SQS reports a URL
		}),
	), nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Jobs) != 2 {
		t.Fatalf("jobs = %+v", out.Jobs)
	}
	j := out.Jobs[0]
	if j.Environment != "production" || j.Release != "1.2" || j.JobName != `App\Jobs\SyncInvoice` || j.DurationMs != 120 ||
		*j.WaitMs != 40 || j.JobID != "42" || *j.MaxTries != 3 || !j.StartedAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("first run = %+v", j)
	}
	j = out.Jobs[1]
	if j.Kind != "job" || j.JobName != "Closure" || j.WaitMs != nil || j.JobUUID != "" || !strings.HasPrefix(j.UUID, "0192f3a4") {
		t.Errorf("defaults and normalization = %+v", j)
	}
}

func TestValidateJobRunsRejectsBadFields(t *testing.T) {
	t.Parallel()
	neg, huge := int64(-1), int64(400*24*time.Hour/time.Millisecond)
	_, err := ingest.ValidateTelemetry(jobBatch(
		jobItem(func(it *ingest.JobItem) { it.ID = "nope" }),
		jobItem(func(it *ingest.JobItem) { it.Attempt = 0 }),
		jobItem(func(it *ingest.JobItem) { it.Job = "ship order to jane@example.com" }),
		jobItem(func(it *ingest.JobItem) { it.Kind = "cron" }),
		jobItem(func(it *ingest.JobItem) { it.Outcome = "ok" }),
		jobItem(func(it *ingest.JobItem) { it.DurationMs = nil }),
		jobItem(func(it *ingest.JobItem) { it.DurationMs = &neg }),
		jobItem(func(it *ingest.JobItem) { it.WaitMs = &huge }),
		jobItem(func(it *ingest.JobItem) { it.Queue = "has space" }),
		jobItem(func(it *ingest.JobItem) { it.StartedAt = now.Add(-48 * time.Hour).Format(time.RFC3339) }),
		jobItem(func(it *ingest.JobItem) { it.UUID = "x" }),
		jobItem(func(it *ingest.JobItem) { it.JobID = strings.Repeat("9", 65) }),
	), nil, now)
	fields := fieldErrors(t, err)
	for _, want := range []string{
		"jobs[0].id", "jobs[1].attempt", "jobs[2].job", "jobs[3].kind", "jobs[4].outcome", "jobs[5].duration_ms",
		"jobs[6].duration_ms", "jobs[7].wait_ms", "jobs[8].queue", "jobs[9].started_at", "jobs[10].uuid", "jobs[11].job_id",
	} {
		if _, ok := fields[want]; !ok {
			t.Errorf("no error for %s (got %v)", want, fields)
		}
	}

	many := make([]ingest.JobItem, ingest.MaxItemsPerList+1)
	for i := range many {
		many[i] = jobItem(nil)
	}
	if _, err := ingest.ValidateTelemetry(jobBatch(many...), nil, now); fieldErrors(t, err)["jobs"] == "" {
		t.Error("more than 500 job runs in one batch should be rejected")
	}
	if _, err := ingest.ValidateTelemetry(jobBatch(), nil, now); fieldErrors(t, err)["notifications"] == "" {
		t.Error("an empty batch should be rejected")
	}
}

func TestConfigShortRetention(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(env(map[string]string{"FAULTBOX_SECRET": testSecret}))
	if err != nil || cfg.ShortRetentionHours != 48 {
		t.Fatalf("default = %d, %v", cfg.ShortRetentionHours, err)
	}
	cfg, err = config.Load(env(map[string]string{"FAULTBOX_SECRET": testSecret, "FAULTBOX_SHORT_RETENTION_HOURS": "24"}))
	if err != nil || cfg.ShortRetentionHours != 24 {
		t.Fatalf("from the environment = %d, %v", cfg.ShortRetentionHours, err)
	}
	for _, bad := range []map[string]string{
		{"FAULTBOX_SHORT_RETENTION_HOURS": "0"},
		{"FAULTBOX_SHORT_RETENTION_HOURS": "169"},
		{"FAULTBOX_SHORT_RETENTION_HOURS": "two"},
		{"FAULTBOX_SHORT_RETENTION_HOURS": "48", "FAULTBOX_RETENTION_DAYS": "1"},
	} {
		bad["FAULTBOX_SECRET"] = testSecret
		if _, err := config.Load(env(bad)); err == nil || !strings.Contains(err.Error(), "FAULTBOX_SHORT_RETENTION_HOURS") {
			t.Errorf("Load(%v) = %v, want a FAULTBOX_SHORT_RETENTION_HOURS error", bad, err)
		}
	}
}
