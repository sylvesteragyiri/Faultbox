package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/notify"
	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

var testKey = bytes.Repeat([]byte{7}, 32)

// hookServer records webhook deliveries and answers with the next status
// in codes (200 once they run out).
type hookServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []notify.Message
	sigs   []bool
	codes  []int
	hits   atomic.Int64
}

func newHookServer(t *testing.T, secret string, codes ...int) *hookServer {
	t.Helper()
	h := &hookServer{codes: codes}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(notify.TimestampHeader), 10, 64)
		h.mu.Lock()
		defer h.mu.Unlock()
		code := http.StatusOK
		if len(h.codes) > 0 {
			code, h.codes = h.codes[0], h.codes[1:]
		}
		if code == http.StatusOK {
			var m notify.Message
			_ = json.Unmarshal(body, &m)
			h.bodies = append(h.bodies, m)
			h.sigs = append(h.sigs, r.Header.Get(notify.SignatureHeader) == notify.Sign([]byte(secret), ts, body))
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookServer) triggers() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.bodies {
		out = append(out, m.Trigger)
	}
	return out
}

// recordingMailer captures email messages instead of sending them.
type recordingMailer struct {
	mu   sync.Mutex
	msgs []string
}

func (m *recordingMailer) Send(_ context.Context, _ store.ChannelConfig, _ string, _ string, _ []string, msg []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, string(msg))
	return nil
}

func (m *recordingMailer) subjects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.msgs {
		for _, line := range strings.Split(msg, "\r\n") {
			if s, ok := strings.CutPrefix(line, "Subject: "); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

type notifyRig struct {
	t      *testing.T
	st     *store.Store
	n      *notify.Notifier
	mailer *recordingMailer
	shop   store.Project
}

func newNotifyRig(t *testing.T) *notifyRig {
	t.Helper()
	st := testutil.OpenStore(t)
	shop, _ := testutil.NewProjectKey(t, st, "Shop")
	r := &notifyRig{t: t, st: st, mailer: &recordingMailer{}, shop: shop}
	r.start()
	return r
}

func (r *notifyRig) start() {
	r.n = notify.New(notify.Options{
		Store: r.st, Logger: testutil.Logger(r.t), Key: testKey, PublicURL: "https://errors.example.com",
		Version: "test", Mailer: r.mailer, SweepInterval: 10 * time.Millisecond,
		Backoff: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
	})
	if err := r.n.Start(); err != nil {
		r.t.Fatal(err)
	}
	n := r.n
	r.t.Cleanup(func() { _ = n.Shutdown(context.Background()) })
}

func (r *notifyRig) webhook(name, url, secret string, edit func(*store.Channel)) int64 {
	r.t.Helper()
	sealed, err := auth.Seal(testKey, []byte(secret))
	if err != nil {
		r.t.Fatal(err)
	}
	c := store.Channel{Name: name, Kind: store.ChannelWebhook, Enabled: true, Config: store.ChannelConfig{URL: url},
		Secret: sealed, OnNewIssue: true, OnRegression: true, OnQueueFailure: true, Cooldown: 30}
	if edit != nil {
		edit(&c)
	}
	if err := store.NormalizeChannel(&c); err != nil {
		r.t.Fatal(err)
	}
	id, err := r.st.CreateChannel(context.Background(), c)
	if err != nil {
		r.t.Fatal(err)
	}
	r.n.InvalidateChannels()
	return id
}

// ingest stores events like the processor does and hands the results to
// the notifier.
func (r *notifyRig) ingest(events ...store.NewEvent) []store.IngestResult {
	r.t.Helper()
	res, err := r.st.IngestBatch(context.Background(), events)
	if err != nil {
		r.t.Fatal(err)
	}
	r.n.Handle(res)
	return res
}

func (r *notifyRig) flush() {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.n.Flush(ctx); err != nil {
		r.t.Fatalf("flush notifier: %v", err)
	}
}

func (r *notifyRig) history(f store.NotificationFilter) []store.Notification {
	r.t.Helper()
	rows, err := r.st.ListNotifications(context.Background(), f)
	if err != nil {
		r.t.Fatal(err)
	}
	return rows
}

func jobEvent(projectID int64, fp, kind string) store.NewEvent {
	e := newEvent(projectID, fp, time.Now().UTC())
	e.Category, e.ExceptionType = store.CategoryQueue, "RuntimeException"
	e.Job = &store.NewQueueFailure{Kind: kind, Name: "App\\Jobs\\SyncInvoice", Queue: "integrations"}
	return e
}

func TestNotificationTriggersDeliverByWebhookAndEmail(t *testing.T) {
	t.Parallel()
	r := newNotifyRig(t)
	ctx := context.Background()
	hook := newHookServer(t, "whsec_test")
	r.webhook("Ops webhook", hook.URL, "whsec_test", func(c *store.Channel) { c.ThresholdCount, c.ThresholdWindow = 3, 60 })
	email := store.Channel{Name: "On-call", Kind: store.ChannelEmail, Enabled: true, Cooldown: 30,
		Config:     store.ChannelConfig{Host: "smtp.example.com", Port: 587, Security: store.SMTPStartTLS, From: "Faultbox <alerts@example.com>", To: []string{"oncall@example.com"}},
		OnNewIssue: true, OnRegression: true, OnQueueFailure: true}
	if err := store.NormalizeChannel(&email); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.CreateChannel(ctx, email); err != nil {
		t.Fatal(err)
	}
	r.n.InvalidateChannels()

	// New issue, then two more events: the third crosses the threshold of 3.
	res := r.ingest(newEvent(r.shop.ID, "fp-new", time.Now().UTC()))
	r.flush()
	r.ingest(newEvent(r.shop.ID, "fp-new", time.Now().UTC()), newEvent(r.shop.ID, "fp-new", time.Now().UTC()))
	r.flush()
	// Regression: resolve, then it happens again.
	if err := r.st.SetIssueStatus(ctx, res[0].IssueID, store.IssueResolved); err != nil {
		t.Fatal(err)
	}
	r.ingest(newEvent(r.shop.ID, "fp-new", time.Now().UTC()))
	// A retry attempt alerts nobody; a terminal failure does.
	r.ingest(jobEvent(r.shop.ID, "fp-job", "attempt"))
	r.ingest(jobEvent(r.shop.ID, "fp-job", "terminal"))
	r.flush()

	// Alerts raised together are delivered concurrently, so compare as sets.
	want := []string{store.TriggerNewIssue, store.TriggerQueueFailure, store.TriggerRegression, store.TriggerThreshold}
	got := hook.triggers()
	slices.Sort(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("webhook triggers %v, want %v", got, want)
	}
	for i, ok := range hook.sigs {
		if !ok {
			t.Errorf("webhook delivery %d has a bad signature", i)
		}
	}
	first := hook.bodies[0] // the new-issue alert was flushed on its own
	if first.Issue == nil || first.Issue.URL != "https://errors.example.com/issues/"+strconv.FormatInt(res[0].IssueID, 10) ||
		first.Project == nil || first.Project.Name != "Shop" || first.DeliveryID == 0 {
		t.Errorf("webhook payload %+v", first)
	}

	subjects := r.mailer.subjects()
	if len(subjects) != 3 { // email has no threshold
		t.Fatalf("email subjects %v", subjects)
	}
	for _, prefix := range []string{"[Faultbox] New issue in Shop (production)", "[Faultbox] Regression in Shop", "[Faultbox] Job failed in Shop"} {
		if !slices.ContainsFunc(subjects, func(s string) bool { return strings.HasPrefix(s, prefix) }) {
			t.Errorf("no email subject starts with %q: %v", prefix, subjects)
		}
	}
	if !strings.Contains(r.mailer.msgs[0], "Open in Faultbox: https://errors.example.com/issues/") {
		t.Error("email body should link to the issue")
	}

	for _, n := range r.history(store.NotificationFilter{Limit: 100}) {
		if n.Status != store.NotificationSent || n.Attempts != 1 {
			t.Errorf("history row %d: status %s, attempts %d", n.ID, n.Status, n.Attempts)
		}
	}
}

func TestFailedDeliveriesAreRetriedAndRecorded(t *testing.T) {
	t.Parallel()
	r := newNotifyRig(t)

	flaky := newHookServer(t, "s1", http.StatusInternalServerError, http.StatusBadGateway) // then 200
	flakyID := r.webhook("Flaky", flaky.URL, "s1", nil)
	broken := newHookServer(t, "s2", 500, 500, 500, 500, 500, 500)
	brokenID := r.webhook("Down", broken.URL, "s2", nil)
	rejecting := newHookServer(t, "s3", http.StatusBadRequest)
	rejectID := r.webhook("Rejects", rejecting.URL, "s3", nil)

	r.ingest(newEvent(r.shop.ID, "fp-retry", time.Now().UTC()))
	r.flush()

	check := func(channel int64, status string, attempts int, errPart string) {
		t.Helper()
		rows := r.history(store.NotificationFilter{ChannelID: channel})
		if len(rows) != 1 {
			t.Fatalf("channel %d: %d history rows", channel, len(rows))
		}
		n := rows[0]
		if n.Status != status || n.Attempts != attempts || !strings.Contains(n.LastError, errPart) {
			t.Errorf("channel %d: status %s, attempts %d, error %q; want %s, %d, %q",
				channel, n.Status, n.Attempts, n.LastError, status, attempts, errPart)
		}
	}
	check(flakyID, store.NotificationSent, 3, "")
	check(brokenID, store.NotificationFailed, 4, "500") // 1 try + 3 retries
	check(rejectID, store.NotificationFailed, 1, "400") // 4xx is permanent
	if broken.hits.Load() != 4 || rejecting.hits.Load() != 1 {
		t.Errorf("attempts: down %d, rejecting %d", broken.hits.Load(), rejecting.hits.Load())
	}

	// A failed delivery can be retried by hand.
	failed := r.history(store.NotificationFilter{ChannelID: rejectID})[0]
	if err := r.n.Retry(context.Background(), failed.ID); err != nil {
		t.Fatal(err)
	}
	r.flush()
	check(rejectID, store.NotificationSent, 2, "")
}

func TestBurstProducesBoundedAlerts(t *testing.T) {
	t.Parallel()
	r := newNotifyRig(t)
	hook := newHookServer(t, "s")
	r.webhook("Burst", hook.URL, "s", func(c *store.Channel) { c.ThresholdCount, c.ThresholdWindow = 50, 60 })

	// 1,000 events on one issue in batches of 100, as the processor writes them.
	for range 10 {
		batch := make([]store.NewEvent, 100)
		for i := range batch {
			batch[i] = newEvent(r.shop.ID, "fp-burst", time.Now().UTC())
		}
		r.ingest(batch...)
	}
	r.flush()
	got := hook.triggers()
	if strings.Join(got, ",") != store.TriggerNewIssue+","+store.TriggerThreshold {
		t.Errorf("a 1,000-event burst produced %d alerts %v, want new_issue and threshold", len(got), got)
	}
	if r.n.Stats().Raised != 2 {
		t.Errorf("stats %+v", r.n.Stats())
	}
}

func TestNotificationScope(t *testing.T) {
	t.Parallel()
	r := newNotifyRig(t)
	ctx := context.Background()
	other, _ := testutil.NewProjectKey(t, r.st, "Other")
	hook := newHookServer(t, "s")
	r.webhook("Shop production", hook.URL, "s", func(c *store.Channel) {
		c.ProjectID, c.Environments = r.shop.ID, []string{"production"}
	})
	offID := r.webhook("Off", hook.URL, "s", nil)
	if err := r.st.SetChannelEnabled(ctx, offID, false); err != nil {
		t.Fatal(err)
	}
	r.n.InvalidateChannels()

	staging := newEvent(r.shop.ID, "fp-staging", time.Now().UTC())
	staging.Environment = "staging"
	r.ingest(staging, newEvent(other.ID, "fp-other", time.Now().UTC()))
	ignored := r.ingest(newEvent(r.shop.ID, "fp-ignored", time.Now().UTC()))
	r.flush()
	if got := hook.triggers(); len(got) != 1 {
		t.Fatalf("deliveries %v, want only the in-scope new issue", got)
	}

	// An ignored issue stays quiet, even when it recurs.
	if err := r.st.SetIssueStatus(ctx, ignored[0].IssueID, store.IssueIgnored); err != nil {
		t.Fatal(err)
	}
	r.ingest(newEvent(r.shop.ID, "fp-ignored", time.Now().UTC()))
	r.flush()
	if got := hook.triggers(); len(got) != 1 {
		t.Errorf("an ignored issue alerted: %v", got)
	}
}

func TestPendingAlertsSurviveRestartAndCooldownsPersist(t *testing.T) {
	t.Parallel()
	r := newNotifyRig(t)
	hook := newHookServer(t, "s", http.StatusServiceUnavailable) // first attempt fails
	r.webhook("Hook", hook.URL, "s", func(c *store.Channel) { c.OnNewIssue, c.OnRegression = true, true })

	res := r.ingest(newEvent(r.shop.ID, "fp-restart", time.Now().UTC()))
	r.flush()
	if got := hook.triggers(); len(got) != 1 {
		t.Fatalf("deliveries before restart %v", got)
	}

	// Stop, then record a pending alert directly, as if the process died
	// before delivering it.
	if err := r.n.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	channels, _ := r.st.Channels(context.Background())
	if _, err := r.st.InsertNotification(context.Background(), store.Notification{
		ChannelID: channels[0].ID, Trigger: store.TriggerTest, Subject: "left behind", Payload: `{"trigger":"test"}`,
	}); err != nil {
		t.Fatal(err)
	}
	r.start()
	r.flush()
	if got := hook.triggers(); len(got) != 2 || got[1] != store.TriggerTest {
		t.Fatalf("pending alert not delivered after restart: %v", got)
	}

	// The cooldown for the first issue was restored from history: a
	// regression within it is suppressed.
	if err := r.st.SetIssueStatus(context.Background(), res[0].IssueID, store.IssueResolved); err != nil {
		t.Fatal(err)
	}
	r.ingest(newEvent(r.shop.ID, "fp-restart", time.Now().UTC()))
	r.flush()
	if got := hook.triggers(); len(got) != 3 {
		// Regression is a different trigger, so it isn't covered by the
		// new-issue cooldown.
		t.Fatalf("regression after restart: %v", got)
	}
	if err := r.st.SetIssueStatus(context.Background(), res[0].IssueID, store.IssueResolved); err != nil {
		t.Fatal(err)
	}
	r.ingest(newEvent(r.shop.ID, "fp-restart", time.Now().UTC()))
	r.flush()
	if got := hook.triggers(); len(got) != 3 || r.n.Stats().Suppressed != 1 {
		t.Errorf("a second regression within the cooldown should be suppressed: %v, %+v", got, r.n.Stats())
	}
}

func TestSecretsAreSealed(t *testing.T) {
	t.Parallel()
	sealed, err := auth.Seal(testKey, []byte("whsec_abc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("whsec_abc")) {
		t.Fatal("sealed value contains the plaintext")
	}
	if plain, err := auth.Unseal(testKey, sealed); err != nil || string(plain) != "whsec_abc" {
		t.Fatalf("unseal: %q, %v", plain, err)
	}
	if _, err := auth.Unseal(bytes.Repeat([]byte{8}, 32), sealed); err == nil {
		t.Error("unsealing with another key should fail")
	}
	again, _ := auth.Seal(testKey, []byte("whsec_abc"))
	if bytes.Equal(again, sealed) {
		t.Error("sealing twice should use a fresh nonce")
	}
}

// TestSMTPMailer sends through a minimal SMTP server, checking the real
// client conversation (no TLS, no auth: the "local relay" setting).
func TestSMTPMailer(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		rd := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 test ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					say("250 queued")
					got <- data.String()
					continue
				}
				data.WriteString(line)
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250 test")
			case strings.HasPrefix(cmd, "DATA"):
				inData = true
				say("354 go ahead")
			case strings.HasPrefix(cmd, "QUIT"):
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	cfg := store.ChannelConfig{Host: host, Port: p, Security: store.SMTPNone}
	msg := []byte("Subject: hello\r\n\r\nbody line\r\n")
	if err := (notify.SMTPMailer{Timeout: 5 * time.Second}).Send(context.Background(), cfg, "", "alerts@example.com", []string{"a@example.com"}, msg); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-got:
		if !strings.Contains(data, "Subject: hello") || !strings.Contains(data, "body line") {
			t.Errorf("server received %q", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the SMTP server got no message")
	}
}
