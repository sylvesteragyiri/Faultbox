package unit

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/ingest"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

var now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func payloadFrom(t *testing.T, p map[string]any) *ingest.Payload {
	t.Helper()
	out, err := ingest.Decode(bytes.NewReader(testutil.MustJSON(t, p)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func validFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	p := testutil.Fixture(t, name)
	p["event_id"] = "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	p["timestamp"] = now.Add(-time.Minute).Format(time.RFC3339)
	return p
}

func fieldErrors(t *testing.T, err error) map[string]string {
	t.Helper()
	var ve *ingest.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	out := map[string]string{}
	for _, f := range ve.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func TestValidateAcceptsEveryFixture(t *testing.T) {
	t.Parallel()
	for _, name := range testutil.FixtureNames {
		p := validFixture(t, name)
		kind := ingest.KindException
		if _, isJob := p["job"]; isJob {
			kind = ingest.KindQueueFailure
		}
		ev, err := ingest.Validate(payloadFrom(t, p), kind, 1, nil, now, 1000)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ev.UUID == "" || ev.OccurredAt.IsZero() || ev.Level == "" || ev.Size <= 0 {
			t.Fatalf("%s: incomplete event %+v", name, ev)
		}
		if kind == ingest.KindQueueFailure && (ev.Job == nil || ev.Job.Kind != ingest.JobTerminal) {
			t.Fatalf("%s: job not carried over: %+v", name, ev.Job)
		}
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()

	p := validFixture(t, "model_not_found")
	p["event_id"] = "not-a-uuid"
	p["timestamp"] = "yesterday"
	p["level"] = "loud"
	p["environment"] = "Prod/EU"
	p["context"] = []any{"not", "an", "object"}
	p["request"] = map[string]any{"method": "G3T", "status_code": 99}
	testutil.Exception(p)["type"] = ""

	_, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, nil, now, 1000)
	got := fieldErrors(t, err)
	for _, field := range []string{"event_id", "timestamp", "level", "environment", "context", "request.method", "request.status_code", "exception.type"} {
		if got[field] == "" {
			t.Errorf("no error reported for %s (got %v)", field, got)
		}
	}
}

func TestValidateTimestampWindow(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		offset time.Duration
		ok     bool
	}{
		{-23 * time.Hour, true},
		{-25 * time.Hour, false},
		{4 * time.Minute, true},
		{6 * time.Minute, false},
	} {
		p := validFixture(t, "model_not_found")
		p["timestamp"] = now.Add(tt.offset).Format(time.RFC3339Nano)
		_, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, nil, now, 1000)
		if (err == nil) != tt.ok {
			t.Errorf("offset %v: err = %v, want ok=%v", tt.offset, err, tt.ok)
		}
	}
}

func TestValidateEnvironmentAllowList(t *testing.T) {
	t.Parallel()

	p := validFixture(t, "type_error") // environment "staging"
	if _, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, []string{"production", "staging"}, now, 1000); err != nil {
		t.Fatalf("allowed environment rejected: %v", err)
	}
	_, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, []string{"production"}, now, 1000)
	if msg := fieldErrors(t, err)["environment"]; !strings.Contains(msg, "not configured") {
		t.Fatalf("environment error = %q", msg)
	}
}

func TestValidateMessageEvent(t *testing.T) {
	t.Parallel()

	p := map[string]any{
		"event_id": "0192f3a4-5b6c-7d8e-9f01-23456789abcd", "timestamp": now.Format(time.RFC3339),
		"environment": "production", "level": "warning", "message": "Payment reconciliation failed",
	}
	ev, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, nil, now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != ingest.KindMessage || ev.ExceptionType != "message" || ev.Message != "Payment reconciliation failed" || ev.Level != "warning" {
		t.Fatalf("message event = %+v", ev)
	}

	// The queue endpoint requires an exception and a job.
	_, err = ingest.Validate(payloadFrom(t, p), ingest.KindQueueFailure, 1, nil, now, 100)
	got := fieldErrors(t, err)
	if got["exception"] == "" || got["job"] == "" {
		t.Fatalf("queue failure without exception/job: %v", got)
	}
}

func TestValidateQueueJob(t *testing.T) {
	t.Parallel()

	p := validFixture(t, "job_timeout")
	job := p["job"].(map[string]any)
	job["name"] = ""
	job["kind"] = "retrying"
	job["attempts"] = -1
	_, err := ingest.Validate(payloadFrom(t, p), ingest.KindQueueFailure, 1, nil, now, 1000)
	got := fieldErrors(t, err)
	for _, f := range []string{"job.name", "job.kind", "job.attempts"} {
		if got[f] == "" {
			t.Errorf("no error for %s", f)
		}
	}
}

func TestValidateTruncatesDeepTraces(t *testing.T) {
	t.Parallel()

	p := validFixture(t, "model_not_found")
	frames := make([]any, 350)
	for i := range frames {
		frames[i] = map[string]any{"file": "/var/www/html/vendor/x/Y.php", "line": i, "function": "f"}
	}
	testutil.Exception(p)["frames"] = frames
	testutil.Exception(p)["message"] = strings.Repeat("é", ingest.MaxMessageLen)

	ev, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, nil, now, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Frames) != ingest.MaxFrames || ev.FramesOmitted != 150 {
		t.Fatalf("frames = %d, omitted = %d; want %d and 150", len(ev.Frames), ev.FramesOmitted, ingest.MaxFrames)
	}
	if ev.Frames[0].Line != 0 || ev.Frames[len(ev.Frames)-1].Line != 349 {
		t.Fatal("truncation must keep the first and last frames")
	}
	if len(ev.Message) > ingest.MaxMessageLen+len("…") || !strings.HasSuffix(ev.Message, "…") {
		t.Fatalf("message not truncated: %d bytes", len(ev.Message))
	}
	if !json.Valid(testutil.MustJSON(t, ev.Message)) {
		t.Fatal("truncation split a UTF-8 character")
	}
}

func TestValidateRejectsOversizedContext(t *testing.T) {
	t.Parallel()

	p := validFixture(t, "model_not_found")
	p["context"] = map[string]any{"blob": strings.Repeat("x", ingest.MaxObjectBytes)}
	_, err := ingest.Validate(payloadFrom(t, p), ingest.KindException, 1, nil, now, 40000)
	if fieldErrors(t, err)["context"] == "" {
		t.Fatal("oversized context accepted")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"", "not json", `{"a":1}{"b":2}`, `[1,2]`} {
		if _, err := ingest.Decode(strings.NewReader(body)); err == nil {
			t.Errorf("Decode(%q) accepted invalid input", body)
		}
	}
}

func TestScrubJSON(t *testing.T) {
	t.Parallel()

	in := `{"form":{"email":"a@b.c","password":"hunter2","card_number":"4242"},"api_key":"k","items":[{"token":"t","sku":"A1"}],"author":null,"ok":1}`
	out, err := ingest.ScrubJSON(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, secret := range []string{"hunter2", "4242", `"k"`, `"t"`} {
		if strings.Contains(s, secret) {
			t.Errorf("scrubbed JSON still contains %s: %s", secret, s)
		}
	}
	for _, keep := range []string{"a@b.c", `"sku":"A1"`, `"ok":1`} {
		if !strings.Contains(s, keep) {
			t.Errorf("scrubbed JSON lost %s: %s", keep, s)
		}
	}
}

func TestScrubHeaders(t *testing.T) {
	t.Parallel()

	out := ingest.ScrubHeaders(map[string]string{
		"Authorization": "Bearer x", "Cookie": "s=1", "X-Faultbox-Key": "fbx_x",
		"X-Session-Id": "abc", "User-Agent": "curl", "Accept": "*/*",
	})
	for _, gone := range []string{"Authorization", "Cookie", "X-Faultbox-Key"} {
		if _, ok := out[gone]; ok {
			t.Errorf("%s header kept", gone)
		}
	}
	if out["X-Session-Id"] != ingest.Redacted || out["User-Agent"] != "curl" || out["Accept"] != "*/*" {
		t.Fatalf("headers = %v", out)
	}
}

func TestQueueIsBoundedByCountAndBytes(t *testing.T) {
	t.Parallel()

	q := ingest.NewQueue(3, 1000)
	small := func() *ingest.Event { return &ingest.Event{Size: 100} }
	for i := range 3 {
		if err := q.Enqueue(small()); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := q.Enqueue(small()); !errors.Is(err, ingest.ErrQueueFull) {
		t.Fatalf("4th event: err = %v, want ErrQueueFull (count limit)", err)
	}
	if q.Accepting() {
		t.Fatal("full queue reports Accepting")
	}

	b := ingest.NewQueue(100, 1000)
	if err := b.Enqueue(&ingest.Event{Size: 900}); err != nil {
		t.Fatal(err)
	}
	if err := b.Enqueue(&ingest.Event{Size: 200}); !errors.Is(err, ingest.ErrQueueFull) {
		t.Fatalf("over byte budget: err = %v, want ErrQueueFull", err)
	}
	if b.Bytes() != 900 {
		t.Fatalf("rejected event leaked into the byte count: %d", b.Bytes())
	}

	b.Close()
	if err := b.Enqueue(small()); !errors.Is(err, ingest.ErrQueueClosed) {
		t.Fatalf("closed queue: err = %v", err)
	}
	b.Close() // idempotent
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()

	clock := now
	l := ingest.NewRateLimiter(10, 20)
	l.SetClock(func() time.Time { return clock })

	for i := range 20 {
		if ok, _ := l.Allow(1); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	ok, wait := l.Allow(1)
	if ok || wait <= 0 || wait > 100*time.Millisecond {
		t.Fatalf("after burst: ok=%v wait=%v, want refused with ~100ms wait", ok, wait)
	}
	if ok, _ := l.Allow(2); !ok {
		t.Fatal("another project was limited")
	}

	clock = clock.Add(500 * time.Millisecond) // refills 5 tokens
	for i := range 5 {
		if ok, _ := l.Allow(1); !ok {
			t.Fatalf("refilled token %d refused", i)
		}
	}
	if ok, _ := l.Allow(1); ok {
		t.Fatal("more tokens than the refill allows")
	}
}
