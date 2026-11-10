package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

// Failures stored before notification issues existed get an issue when the
// store opens, once.
func TestNotificationFailuresAreBackfilledIntoIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "faultbox.db")

	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := testutil.NewProjectKey(t, st, "Shop")
	now := time.Now().UTC()
	failure := func() store.NewAppNotification {
		return store.NewAppNotification{
			UUID: testutil.NewEventID(), Environment: "production", Notification: `App\Notifications\InvoicePaid`,
			Channel: "mail", Notifiable: `App\Models\User`, Failed: true,
			ErrorType: "TransportException", ErrorMessage: "timed out", OccurredAt: now,
		}
	}
	_, issues, err := st.IngestTelemetry(ctx, project.ID, store.TelemetryBatch{
		BatchID:       testutil.NewEventID(),
		Notifications: []store.NewAppNotification{failure(), failure()},
	})
	if err != nil || len(issues) != 2 || issues[0].IssueID != issues[1].IssueID || !issues[0].NewIssue {
		t.Fatalf("ingest: %v %+v", err, issues)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Put the database back in its pre-00010 state: failures without issues.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE app_notification_failures SET issue_id = NULL`,
		`DELETE FROM issues_fts`,
		`DELETE FROM daily_totals`,
		`DELETE FROM events`,
		`DELETE FROM issues`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = db.Close()

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	page, err := st.ListIssues(ctx, store.IssueFilter{Category: store.CategoryNotification})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 1 || page.Issues[0].OccurrenceCount != 2 || page.Issues[0].JobName != `App\Notifications\InvoicePaid` {
		t.Fatalf("backfilled issues: %+v", page.Issues)
	}
	failures, err := st.DeliveryFailures(ctx, store.NotificationDeliveries, store.TelemetryScope{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range failures {
		if f.IssueID != page.Issues[0].ID {
			t.Errorf("failure %s not linked: issue %d", f.UUID, f.IssueID)
		}
	}
}
