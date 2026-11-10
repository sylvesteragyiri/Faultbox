package feature

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// Failed Laravel notifications are issues: listed with the other errors,
// opened with their full message, and resolved like any issue.
func TestFailedNotificationsBecomeIssues(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	long := "Connection to smtp.example.com timed out after 30 seconds while sending the invoice. " +
		strings.Repeat("The relay refused the session. ", 8) + "END-OF-MESSAGE"
	failed := func() map[string]any {
		n := notification(`App\Notifications\InvoicePaid`, "mail", "failed")
		n["error"] = map[string]any{"type": `Symfony\Component\Mailer\Exception\TransportException`, "message": long}
		n["correlation_id"] = "req-42"
		return n
	}
	// Two batches, same class, channel and error type: one issue, two events.
	for range 2 {
		if resp := a.Post(notificationsPath, key, testutil.MustJSON(t, batch("production",
			failed(), notification(`App\Notifications\InvoicePaid`, "mail", "sent")))); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("accept: %d %s", resp.StatusCode, resp.Body)
		}
	}
	// A different channel is a different issue.
	a.Post(notificationsPath, key, testutil.MustJSON(t, batch("production", notification(`App\Notifications\InvoicePaid`, "slack", "failed"))))

	ctx := context.Background()
	page, err := a.Store().ListIssues(ctx, store.IssueFilter{Category: store.CategoryNotification})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 2 {
		t.Fatalf("want 2 notification issues (mail, slack), got %d", len(page.Issues))
	}
	var mail store.IssueSummary
	for _, is := range page.Issues {
		if strings.Contains(is.Culprit, "mail") {
			mail = is
		}
	}
	if mail.OccurrenceCount != 2 || mail.JobName != `App\Notifications\InvoicePaid` || mail.Status != store.IssueUnresolved {
		t.Fatalf("mail issue: %+v", mail)
	}

	a.LoginAdmin()
	// The default issue list (errors) includes them.
	if list := a.Get("/issues"); !contains(list.Body, `App\Notifications\InvoicePaid`) || !contains(list.Body, "Notification</span>") {
		t.Error("the issue list should show failed notifications with a Notification badge")
	}

	path := "/issues/" + strconv.FormatInt(mail.ID, 10)
	detail := a.Get(path)
	if detail.StatusCode != http.StatusOK {
		t.Fatalf("issue page: %d", detail.StatusCode)
	}
	for _, want := range []string{"END-OF-MESSAGE", "Failed notification", "TransportException", "Sent to", `App\Models\User`, "req-42"} {
		if !contains(detail.Body, want) {
			t.Errorf("issue page is missing %q", want)
		}
	}
	if contains(detail.Body, "<h3>Stack trace</h3>") {
		t.Error("a notification issue has no stack trace section")
	}

	// The App notifications page links each failure to its issue.
	deliveries := a.Get("/app-notifications")
	if !contains(deliveries.Body, `href="`+path+`?event=`) {
		t.Error("recent failures should link to their issue")
	}

	// Resolve it; the failure list shows that, and a new failure reopens it.
	if resp := a.PostForm(path+"/status", url.Values{"status": {"resolved"}}, a.CSRF(path)); resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: %d", resp.StatusCode)
	}
	if is, _ := a.Store().Issue(ctx, mail.ID); is.Status != store.IssueResolved {
		t.Fatalf("status after resolve: %s", is.Status)
	}
	if !contains(a.Get("/app-notifications").Body, `badge-ok">Resolved`) {
		t.Error("a resolved failure should say so on the App notifications page")
	}
	a.Post(notificationsPath, key, testutil.MustJSON(t, batch("production", failed())))
	if is, _ := a.Store().Issue(ctx, mail.ID); is.Status != store.IssueUnresolved || is.OccurrenceCount != 3 {
		t.Fatalf("a new failure should reopen the issue: %s, %d events", is.Status, is.OccurrenceCount)
	}
}
