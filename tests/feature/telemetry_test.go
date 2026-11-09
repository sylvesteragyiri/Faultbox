package feature

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

const telemetryPath = "/api/v1/telemetry"

func mailItem(mailable, mailer, status string, recipients int) map[string]any {
	m := map[string]any{
		"id": testutil.NewEventID(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"mailable": mailable, "mailer": mailer, "recipients": recipients, "status": status,
	}
	if status == "failed" {
		m["error"] = map[string]any{"type": `Symfony\Component\Mailer\Exception\TransportException`, "message": "Expected response code 250 but got 550"}
	}
	return m
}

func commandItem(command string, exit int, ms int64, errMsg string) map[string]any {
	c := map[string]any{
		"id": testutil.NewEventID(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"command": command, "exit_code": exit, "duration_ms": ms,
	}
	if errMsg != "" {
		c["error"] = map[string]any{"type": `Illuminate\Database\QueryException`, "message": errMsg}
	}
	return c
}

func telemetryBatch(env string, notifications, mails, commands []map[string]any) map[string]any {
	b := map[string]any{"batch_id": testutil.NewEventID(), "environment": env, "release": "1.4.2"}
	if notifications != nil {
		b["notifications"] = notifications
	}
	if mails != nil {
		b["mails"] = mails
	}
	if commands != nil {
		b["commands"] = commands
	}
	return b
}

func TestTelemetryMailAndCommands(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	b := telemetryBatch("production",
		[]map[string]any{notification(`App\Notifications\InvoicePaid`, "mail", "sent")},
		[]map[string]any{
			mailItem(`App\Mail\Auth\EmailOTP`, "smtp", "sent", 1),
			mailItem(`App\Mail\Auth\EmailOTP`, "smtp", "sent", 1),
			mailItem(`App\Mail\Auth\EmailOTP`, "smtp", "failed", 1),
			mailItem("", "log", "sent", 3), // Mail::raw
		},
		[]map[string]any{
			commandItem("app:sync-orders", 0, 1200, ""),
			commandItem("app:sync-orders", 0, 800, ""),
			commandItem("migrate", 1, 340, "SQLSTATE[42S01]: Base table or view already exists"),
		},
	)
	resp := a.Post(telemetryPath, key, testutil.MustJSON(t, b))
	if resp.StatusCode != http.StatusAccepted || !contains(resp.Body, `"count":8`) {
		t.Fatalf("accept: %d %s", resp.StatusCode, resp.Body)
	}
	if again := a.Post(telemetryPath, key, testutil.MustJSON(t, b)); !contains(again.Body, `"status":"duplicate"`) {
		t.Fatalf("retry: %s", again.Body)
	}
	a.LoginAdmin()

	mail := a.Get("/mail").Body
	for _, want := range []string{
		"<h1>Mail</h1>", "EmailOTP", `App\Mail\Auth`, ">smtp<", ">log<", "Raw mail", "Mail::raw or a closure",
		"Expected response code 250 but got 550", "1 recipient ", "25%", // 1 failure of 4 attempts
		`href="/mail" aria-current="page"`,
	} {
		if !contains(mail, want) {
			t.Errorf("mail page is missing %q", want)
		}
	}
	if !contains(mail, `<span class="stat-value">3</span>`) || !contains(mail, `<span class="stat-value">1</span>`) {
		t.Error("mail: want 3 sent and 1 failed, counted once despite the retry")
	}
	if contains(mail, "InvoicePaid") {
		t.Error("notification mail belongs under App notifications, not Mail")
	}

	cmds := a.Get("/commands").Body
	for _, want := range []string{
		"<h1>Commands</h1>", "app:sync-orders", "migrate", "exit 1", "exit 0",
		"Base table or view already exists", "Recent failed runs",
		"1 s", "340 ms", // average 1,000 ms for sync-orders; migrate took 340 ms
		`href="/commands" aria-current="page"`,
	} {
		if !contains(cmds, want) {
			t.Errorf("commands page is missing %q", want)
		}
	}
	if strings.Index(cmds, "migrate") > strings.Index(cmds, "app:sync-orders") {
		t.Error("the failing command should come first")
	}
	all := a.Get("/commands?runs=all").Body
	if strings.Count(all, `<span class="mono">app:sync-orders</span>`) != 2 {
		t.Error("the all-runs list should show both sync-orders runs")
	}
}

func TestTelemetryValidation(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	_, key := testutil.NewProjectKey(t, a.Store(), "Shop")

	noExit := commandItem("migrate", 0, 10, "")
	delete(noExit, "exit_code")
	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"empty batch", telemetryBatch("production", nil, nil, nil), "notifications"},
		{"command without exit code", telemetryBatch("production", nil, nil, []map[string]any{noExit}), "commands[0].exit_code"},
		{"bad command name", telemetryBatch("production", nil, nil, []map[string]any{commandItem("rm -rf /", 0, 1, "")}), "commands[0].command"},
		{"negative duration", telemetryBatch("production", nil, nil, []map[string]any{commandItem("migrate", 0, -5, "")}), "commands[0].duration_ms"},
		{"negative recipients", telemetryBatch("production", nil, []map[string]any{mailItem(`App\Mail\X`, "smtp", "sent", -1)}, nil), "mails[0].recipients"},
		{"bad mail status", telemetryBatch("production", nil, []map[string]any{mailItem(`App\Mail\X`, "smtp", "queued", 1)}, nil), "mails[0].status"},
	}
	for _, c := range cases {
		resp := a.Post(telemetryPath, key, testutil.MustJSON(t, c.body))
		if resp.StatusCode != http.StatusUnprocessableEntity || !contains(resp.Body, fmt.Sprintf(`"field":"%s"`, c.field)) {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, resp.Body)
		}
	}
}

func TestTelemetryPagesFollowTheScope(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	shop, shopKey := testutil.NewProjectKey(t, a.Store(), "Shop")
	_, adminKey := testutil.NewProjectKey(t, a.Store(), "Admin")
	a.Post(telemetryPath, shopKey, testutil.MustJSON(t, telemetryBatch("production", nil,
		[]map[string]any{mailItem(`App\Mail\ShopReceipt`, "smtp", "sent", 1)},
		[]map[string]any{commandItem("shop:reindex", 0, 50, "")})))
	a.Post(telemetryPath, adminKey, testutil.MustJSON(t, telemetryBatch("production", nil,
		[]map[string]any{mailItem(`App\Mail\AdminDigest`, "smtp", "sent", 1)},
		[]map[string]any{commandItem("admin:report", 0, 50, "")})))
	a.LoginAdmin()
	setScope(t, a, shop.ID, "", "/mail")

	if mail := a.Get("/mail").Body; !contains(mail, "ShopReceipt") || contains(mail, "AdminDigest") {
		t.Error("mail should follow the scoped project")
	}
	if cmds := a.Get("/commands").Body; !contains(cmds, "shop:reindex") || contains(cmds, "admin:report") {
		t.Error("commands should follow the scoped project")
	}
}
