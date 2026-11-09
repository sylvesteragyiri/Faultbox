package dashboard

import (
	"fmt"
	"net/http"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// Telemetry pages: Laravel notifications, mail and artisan commands the
// monitored apps send and run, scoped by the project and environment
// switcher.

// telemetryScope reads the switcher and the range tabs (7, 14 or 30 days).
func (h *Handler) telemetryScope(r *http.Request) (store.TelemetryScope, map[int64]string, int, error) {
	days := 30
	switch r.URL.Query().Get("range") {
	case "7d":
		days = 7
	case "14d":
		days = 14
	}
	names, _, err := h.projectNames(r)
	if err != nil {
		return store.TelemetryScope{}, nil, days, err
	}
	scope := scopeFrom(r)
	if _, ok := names[scope.ProjectID]; !ok {
		scope.ProjectID = 0
	}
	return store.TelemetryScope{ProjectID: scope.ProjectID, Environment: scope.Env}, names, days, nil
}

// deliveryPage describes one of the two delivery pages.
type deliveryPage struct {
	Kind      store.DeliveryKind
	Nav       string
	Path      string
	Title     string
	Lead      string // after "… your app sent"
	Icon      string
	NameLabel string // column header: Notification / Mailable
	ChanLabel string // column header: Channel / Mailer
	NameNoun  string // stat label: Notification classes / Mailables
	Extra     string // recent failures: "to" (notifiable) or "recipients"
	EmptyText string
}

var (
	notificationsPage = deliveryPage{
		Kind: store.NotificationDeliveries, Nav: "app_notifications", Path: "/app-notifications",
		Title: "App notifications", Lead: "Laravel notifications your app sent", Icon: "send",
		NameLabel: "Notification", ChanLabel: "Channel", NameNoun: "Notification classes", Extra: "to",
		EmptyText: "Update the Laravel package and notifications are reported automatically, with no changes to your code. Each channel (mail, database, Slack, SMS) is counted separately, and failures come with the reason.",
	}
	mailPage = deliveryPage{
		Kind: store.MailDeliveries, Nav: "mail", Path: "/mail",
		Title: "Mail", Lead: "Email your app sent directly (Mailables and Mail::raw; mail sent by notifications is under App notifications)", Icon: "mail",
		NameLabel: "Mailable", ChanLabel: "Mailer", NameNoun: "Mailables", Extra: "recipients",
		EmptyText: "Mail sent with Mail::to()->send(), Mailables and Mail::raw is reported automatically. Only the number of recipients is stored, never their addresses.",
	}
)

func (h *Handler) appNotifications(w http.ResponseWriter, r *http.Request) {
	h.deliveries(w, r, notificationsPage)
}

func (h *Handler) mail(w http.ResponseWriter, r *http.Request) { h.deliveries(w, r, mailPage) }

func (h *Handler) deliveries(w http.ResponseWriter, r *http.Request, p deliveryPage) {
	ctx := r.Context()
	sc, names, days, err := h.telemetryScope(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	summary, err := h.store.DeliverySummary(ctx, p.Kind, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.DeliveryTrend(ctx, p.Kind, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	stats, err := h.store.DeliveryStats(ctx, p.Kind, sc, days, 50)
	if err != nil {
		h.serverError(w, err)
		return
	}
	failures, err := h.store.DeliveryFailures(ctx, p.Kind, sc, 20)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "deliveries", "", h.newPage(r, p.Title, p.Nav, map[string]any{
		"Page":      p,
		"Range":     days,
		"Summary":   summary,
		"Chart":     newBarChart(fmt.Sprintf("%s per day, last %d days", p.Title, days), trend, []string{"Sent", "Failed"}),
		"Stats":     stats,
		"Failures":  failures,
		"Names":     names,
		"ScopeName": names[sc.ProjectID],
		"Env":       sc.Environment,
	}))
}

func (h *Handler) commands(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, names, days, err := h.telemetryScope(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	summary, err := h.store.CommandSummary(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.CommandTrend(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	stats, err := h.store.CommandStats(ctx, sc, days, 50)
	if err != nil {
		h.serverError(w, err)
		return
	}
	failedOnly := r.URL.Query().Get("runs") != "all"
	runs, err := h.store.RecentCommandRuns(ctx, sc, failedOnly, 25)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "commands", "", h.newPage(r, "Commands", "commands", map[string]any{
		"Range":      days,
		"Summary":    summary,
		"Chart":      newBarChart(fmt.Sprintf("Command runs per day, last %d days", days), trend, []string{"Succeeded", "Failed"}),
		"Stats":      stats,
		"Runs":       runs,
		"FailedOnly": failedOnly,
		"Names":      names,
		"ScopeName":  names[sc.ProjectID],
		"Env":        sc.Environment,
	}))
}

func (h *Handler) cache(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sc, names, days, err := h.telemetryScope(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	sort := store.CacheByOperations
	switch store.CacheSort(r.URL.Query().Get("sort")) {
	case store.CacheByMisses:
		sort = store.CacheByMisses
	case store.CacheByFailures:
		sort = store.CacheByFailures
	}
	summary, err := h.store.CacheSummary(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.CacheTrend(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	stores, err := h.store.CacheStores(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	stats, err := h.store.CacheStats(ctx, sc, days, sort, 100)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "cache", "", h.newPage(r, "Cache", "cache", map[string]any{
		"Range":     days,
		"Sort":      string(sort),
		"Summary":   summary,
		"Chart":     newBarChart(fmt.Sprintf("Cache reads per day, last %d days", days), trend, []string{"Hits", "Misses"}),
		"Stores":    stores,
		"Stats":     stats,
		"OtherKey":  store.CacheOtherKey,
		"ScopeName": names[sc.ProjectID],
		"Env":       sc.Environment,
	}))
}
