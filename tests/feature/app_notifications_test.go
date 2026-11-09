package feature

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

const notificationsPath = "/api/v1/notifications"

func notification(class, channel, status string) map[string]any {
	n := map[string]any{
		"id": testutil.NewEventID(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"notification": class, "channel": channel, "notifiable": `App\Models\User`, "status": status,
	}
	if status == "failed" {
		n["error"] = map[string]any{"type": `Symfony\Component\Mailer\Exception\TransportException`, "message": "Connection to smtp.example.com timed out"}
	}
	return n
}

func batch(env string, items ...map[string]any) map[string]any {
	return map[string]any{"batch_id": testutil.NewEventID(), "environment": env, "release": "1.4.2", "notifications": items}
}

func TestAppNotificationsAreStoredAndShown(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	b := batch("production",
		notification(`App\Notifications\InvoicePaid`, "mail", "sent"),
		notification(`App\Notifications\InvoicePaid`, "mail", "sent"),
		notification(`App\Notifications\InvoicePaid`, "database", "sent"),
		notification(`App\Notifications\InvoicePaid`, "mail", "failed"),
		notification(`App\Notifications\OrderShipped`, "vonage", "sent"),
	)
	resp := a.Post(notificationsPath, key, testutil.MustJSON(t, b))
	if resp.StatusCode != http.StatusAccepted || !contains(resp.Body, `"status":"accepted"`) || !contains(resp.Body, `"count":5`) {
		t.Fatalf("accept: %d %s", resp.StatusCode, resp.Body)
	}

	// A retried batch (same batch_id) is acknowledged but not counted again.
	again := a.Post(notificationsPath, key, testutil.MustJSON(t, b))
	if again.StatusCode != http.StatusAccepted || !contains(again.Body, `"status":"duplicate"`) {
		t.Fatalf("retry: %d %s", again.StatusCode, again.Body)
	}

	a.LoginAdmin()
	page := a.Get("/app-notifications")
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page: %d", page.StatusCode)
	}
	for _, want := range []string{
		"App notifications", "InvoicePaid", "OrderShipped", ">mail<", ">database<", ">vonage<",
		`class="chart-svg"`, "Sent", "Failed", // chart with both series
		"TransportException", "Connection to smtp.example.com timed out", "to User", // the failure and why
		"20%", // 1 failure of 5 attempts
	} {
		if !contains(page.Body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if !contains(page.Body, `<span class="stat-value">4</span>`) || !contains(page.Body, `<span class="stat-value">1</span>`) {
		t.Error("the retried batch was counted twice: want 4 sent and 1 failed")
	}
	// Failing classes come first in the table.
	if strings.Index(page.Body, "OrderShipped") < strings.Index(page.Body, "InvoicePaid") {
		t.Error("the class with failures should be listed first")
	}
	// The nav has the page under Monitor, and Faultbox's own settings are now "Alerts".
	if !contains(page.Body, `href="/app-notifications" aria-current="page"`) || !contains(page.Body, "<span>Alerts</span>") {
		t.Error("nav: App notifications should be current, and the alert settings labelled Alerts")
	}
}

func TestAppNotificationsValidation(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop", "production")

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"missing batch id", map[string]any{"environment": "production", "notifications": []any{notification(`App\X`, "mail", "sent")}}, "batch_id"},
		{"environment not allowed", batch("staging", notification(`App\X`, "mail", "sent")), "environment"},
		{"empty batch", batch("production"), "notifications"},
		{"bad status", batch("production", notification(`App\X`, "mail", "bounced")), "notifications[0].status"},
		{"bad class", batch("production", notification(`<script>`, "mail", "sent")), "notifications[0].notification"},
		{"old timestamp", batch("production", func() map[string]any {
			n := notification(`App\X`, "mail", "sent")
			n["timestamp"] = time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
			return n
		}()), "notifications[0].timestamp"},
	}
	for _, c := range cases {
		resp := a.Post(notificationsPath, key, testutil.MustJSON(t, c.body))
		if resp.StatusCode != http.StatusUnprocessableEntity || !contains(resp.Body, fmt.Sprintf(`"field":"%s"`, c.field)) {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, resp.Body)
		}
	}

	big := make([]map[string]any, 501)
	for i := range big {
		big[i] = notification(`App\X`, "mail", "sent")
	}
	if resp := a.Post(notificationsPath, key, testutil.MustJSON(t, batch("production", big...))); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("501 notifications: %d", resp.StatusCode)
	}
	if resp := a.Post(notificationsPath, "fbx_wrongkeywrongkeywrongkeywrongkeywrongkey12", testutil.MustJSON(t, batch("production", notification(`App\X`, "mail", "sent")))); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: %d", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	resp := a.Post(notificationsPath, key, []byte(`{"batch_id":`))
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil || resp.StatusCode != http.StatusBadRequest || body.Error.Code != "invalid_json" {
		t.Errorf("broken JSON: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestAppNotificationsFollowTheScope(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	shop, shopKey := testutil.NewProjectKey(t, a.Store(), "Shop")
	_, adminKey := testutil.NewProjectKey(t, a.Store(), "Admin")
	a.Post(notificationsPath, shopKey, testutil.MustJSON(t, batch("production", notification(`App\Notifications\InvoicePaid`, "mail", "sent"))))
	a.Post(notificationsPath, shopKey, testutil.MustJSON(t, batch("staging", notification(`App\Notifications\StagingOnly`, "mail", "sent"))))
	a.Post(notificationsPath, adminKey, testutil.MustJSON(t, batch("production", notification(`App\Notifications\AdminDigest`, "mail", "sent"))))
	a.LoginAdmin()

	all := a.Get("/app-notifications").Body
	if !contains(all, "InvoicePaid") || !contains(all, "AdminDigest") || !contains(all, "StagingOnly") {
		t.Fatal("without a scope, every project should show")
	}
	setScope(t, a, shop.ID, "production", "/app-notifications")
	scoped := a.Get("/app-notifications").Body
	if !contains(scoped, "InvoicePaid") || contains(scoped, "AdminDigest") || contains(scoped, "StagingOnly") {
		t.Error("the page should follow the scoped project and environment")
	}
	if !contains(scoped, "in Shop (production)") {
		t.Error("the subtitle should name the scope")
	}
}
