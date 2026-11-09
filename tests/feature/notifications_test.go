package feature

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/notify"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

var secretRe = regexp.MustCompile(`whsec_[0-9a-f]{64}`)

// receiver records webhook deliveries and checks their signatures once the
// secret is known.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	status   int
	bodies   [][]byte
	stamps   []int64
	sigs     []string
	triggers []string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	rc := &receiver{status: http.StatusOK}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(notify.TimestampHeader), 10, 64)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if rc.status == http.StatusOK {
			rc.bodies = append(rc.bodies, body)
			rc.stamps = append(rc.stamps, ts)
			rc.sigs = append(rc.sigs, r.Header.Get(notify.SignatureHeader))
			rc.triggers = append(rc.triggers, r.Header.Get(notify.EventHeader))
		}
		w.WriteHeader(rc.status)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) setStatus(code int) { rc.mu.Lock(); rc.status = code; rc.mu.Unlock() }

func (rc *receiver) got() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]string(nil), rc.triggers...)
}

func flushNotifications(t *testing.T, a *testutil.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
}

// createWebhook creates a webhook channel through the UI and returns its
// path and the secret shown once.
func createWebhook(t *testing.T, a *testutil.App, name, target string, extra url.Values) (string, string) {
	t.Helper()
	form := url.Values{
		"kind": {"webhook"}, "name": {name}, "enabled": {"1"}, "url": {target},
		"on_new_issue": {"1"}, "on_regression": {"1"}, "on_queue_failure": {"1"},
		"threshold_count": {"0"}, "threshold_window": {"60"}, "cooldown": {"30"},
	}
	for k, v := range extra {
		form[k] = v
	}
	page := a.PostForm("/notifications/channels", form, a.CSRF("/notifications/channels/new?kind=webhook"))
	if page.StatusCode != http.StatusOK || !contains(page.Body, "Copy this secret now") {
		t.Fatalf("create webhook: status %d", page.StatusCode)
	}
	secret := secretRe.FindString(page.Body)
	m := regexp.MustCompile(`href="(/notifications/channels/\d+)">Continue`).FindStringSubmatch(page.Body)
	if secret == "" || m == nil {
		t.Fatal("the creation page should show the secret and link to the channel")
	}
	return m[1], secret
}

func TestWebhookChannelEndToEnd(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")
	a.LoginAdmin()
	rc := newReceiver(t)
	path, secret := createWebhook(t, a, "Ops webhook", rc.URL, nil)

	// The secret is shown once, then never again, and isn't stored in the clear.
	if contains(a.Get(path).Body, secret) || contains(a.Get("/notifications").Body, secret) {
		t.Error("the webhook secret was shown again")
	}
	channels, _ := a.Store().Channels(context.Background())
	if len(channels) != 1 || bytes.Contains(channels[0].Secret, []byte(secret)) {
		t.Fatal("the secret must be stored sealed")
	}

	// An event through the API reaches the webhook, signed.
	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "type_error")))
	a.PostEvent(queuePath, key, testutil.Stamp(testutil.Fixture(t, "job_timeout")))
	flushNotifications(t, a)
	// Two workers deliver concurrently, so the order isn't fixed.
	got := rc.got()
	slices.Sort(got)
	if strings.Join(got, ",") != "new_issue,queue_failure" {
		t.Fatalf("webhook triggers %v", got)
	}
	var queueMsg *notify.Message
	for i, body := range rc.bodies {
		if rc.sigs[i] != notify.Sign([]byte(secret), rc.stamps[i], body) {
			t.Errorf("delivery %d: bad signature", i)
		}
		var m notify.Message
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		if m.Trigger == "queue_failure" {
			queueMsg = &m
		}
	}
	if queueMsg == nil || queueMsg.Issue == nil || queueMsg.Issue.JobName != `App\Jobs\SyncInvoiceToXero` {
		t.Errorf("queue failure payload: %+v", queueMsg)
	}

	// The history shows both deliveries.
	page := a.Get("/notifications").Body
	history := page[strings.Index(page, `id="history"`):]
	if strings.Count(history, "Delivered</span>") != 2 || !contains(page, "Ops webhook") {
		t.Error("the delivery history should list both deliveries as delivered")
	}

	// Send test, then rotate the secret: the old one stops verifying.
	test := a.PostForm(path+"/test", nil, a.CSRF(path))
	if !contains(test.Body, "Test notification queued") {
		t.Fatalf("send test: %d", test.StatusCode)
	}
	flushNotifications(t, a)
	if got := rc.got(); got[len(got)-1] != "test" {
		t.Fatalf("test delivery missing: %v", got)
	}
	rotated := a.PostForm(path+"/secret", nil, a.CSRF(path))
	newSecret := secretRe.FindString(rotated.Body)
	if newSecret == "" || newSecret == secret {
		t.Fatal("rotating should show a new secret once")
	}
	a.PostForm(path+"/test", nil, a.CSRF(path))
	flushNotifications(t, a)
	last := len(rc.bodies) - 1
	if rc.sigs[last] != notify.Sign([]byte(newSecret), rc.stamps[last], rc.bodies[last]) {
		t.Error("deliveries after rotation should be signed with the new secret")
	}
}

func TestFailedDeliveryShowsAndRetriesFromUI(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	rc := newReceiver(t)
	rc.setStatus(http.StatusNotFound) // a permanent failure
	path, _ := createWebhook(t, a, "Broken", rc.URL, nil)
	a.PostForm(path+"/test", nil, a.CSRF(path))
	flushNotifications(t, a)

	page := a.Get("/notifications?status=failed").Body
	if !contains(page, "Failed</span>") || !contains(page, "404") {
		t.Fatal("a failed delivery should show its status and error")
	}
	m := regexp.MustCompile(`action="(/notifications/history/\d+/retry)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no retry button")
	}
	rc.setStatus(http.StatusOK)
	if resp := a.PostForm(m[1], nil, a.CSRF("/notifications")); !contains(resp.Body, "Delivery queued again") {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
	flushNotifications(t, a)
	if got := rc.got(); len(got) != 1 {
		t.Errorf("retried delivery not received: %v", got)
	}
	if again := a.PostForm(m[1], nil, a.CSRF("/notifications")); again.StatusCode != http.StatusConflict {
		t.Errorf("retrying a delivered notification: %d, want 409", again.StatusCode)
	}
}

func TestEmailChannelKeepsPasswordSecret(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	form := url.Values{
		"kind": {"email"}, "name": {"On-call"}, "enabled": {"1"},
		"host": {"smtp.example.com"}, "port": {"587"}, "security": {"starttls"},
		"username": {"mailer"}, "password": {"hunter2-smtp"},
		"from": {"Faultbox <alerts@example.com>"}, "to": {"a@example.com, b@example.com"},
		"on_new_issue": {"1"}, "threshold_count": {"0"}, "threshold_window": {"60"}, "cooldown": {"30"},
	}
	created := a.PostForm("/notifications/channels", form, a.CSRF("/notifications/channels/new?kind=email"))
	if !contains(created.Body, "Channel saved") || !contains(created.Body, "a@example.com, b@example.com") {
		t.Fatalf("create email channel: %d", created.StatusCode)
	}
	channels, _ := a.Store().Channels(context.Background())
	path := "/notifications/channels/" + strconv.FormatInt(channels[0].ID, 10)
	edit := a.Get(path).Body
	if contains(edit, "hunter2-smtp") || !contains(edit, "Saved. Leave blank to keep it") {
		t.Fatal("the edit page must not show the password")
	}

	// Saving with a blank password keeps the stored one.
	form.Set("password", "")
	form.Set("name", "On-call team")
	a.PostForm(path, form, a.CSRF(path))
	after, _ := a.Store().Channel(context.Background(), channels[0].ID)
	if after.Name != "On-call team" || !bytes.Equal(after.Secret, channels[0].Secret) {
		t.Error("a blank password should keep the stored secret")
	}

	// Validation errors re-render the form with a message.
	form.Set("to", "not-an-address")
	bad := a.PostForm(path, form, a.CSRF(path))
	if bad.StatusCode != http.StatusUnprocessableEntity || !contains(bad.Body, "valid email address") {
		t.Errorf("bad recipient: %d", bad.StatusCode)
	}
	form.Set("to", "a@example.com")
	form.Del("on_new_issue")
	if none := a.PostForm(path, form, a.CSRF(path)); !contains(none.Body, "at least one trigger") {
		t.Error("a channel with no triggers should be rejected")
	}
}

func TestChannelToggleAndDelete(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")
	a.LoginAdmin()
	rc := newReceiver(t)
	path, _ := createWebhook(t, a, "Toggle me", rc.URL, nil)

	if bad := a.PostForm("/notifications/channels", url.Values{"kind": {"webhook"}, "name": {"x"}, "url": {"ftp://nope"},
		"on_new_issue": {"1"}, "cooldown": {"30"}}, a.CSRF("/notifications")); bad.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("non-http webhook URL: %d", bad.StatusCode)
	}

	off := a.PostForm(path+"/enabled", url.Values{"enabled": {"0"}}, a.CSRF("/notifications"))
	if !contains(off.Body, "Channel turned off") {
		t.Fatalf("turn off: %d", off.StatusCode)
	}
	a.PostEvent(eventsPath, key, testutil.Stamp(testutil.Fixture(t, "type_error")))
	flushNotifications(t, a)
	if got := rc.got(); len(got) != 0 {
		t.Errorf("a disabled channel delivered %v", got)
	}

	del := a.PostForm(path+"/delete", nil, a.CSRF(path))
	if !contains(del.Body, "Channel deleted") || a.Get(path).StatusCode != http.StatusNotFound {
		t.Error("the channel should be deleted")
	}
}
