package dashboard

import (
	"fmt"
	"net/http"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// appNotifications shows the Laravel notifications the monitored apps sent,
// scoped by the project and environment switcher.
func (h *Handler) appNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := 30
	switch r.URL.Query().Get("range") {
	case "7d":
		days = 7
	case "14d":
		days = 14
	}
	names, _, err := h.projectNames(r)
	if err != nil {
		h.serverError(w, err)
		return
	}
	scope := scopeFrom(r)
	if _, ok := names[scope.ProjectID]; !ok {
		scope.ProjectID = 0
	}
	sc := store.AppNotificationScope{ProjectID: scope.ProjectID, Environment: scope.Env}

	summary, err := h.store.AppNotificationSummary(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	trend, err := h.store.AppNotificationTrend(ctx, sc, days)
	if err != nil {
		h.serverError(w, err)
		return
	}
	types, err := h.store.AppNotificationTypes(ctx, sc, days, 50)
	if err != nil {
		h.serverError(w, err)
		return
	}
	failures, err := h.store.AppNotificationFailures(ctx, sc, 20)
	if err != nil {
		h.serverError(w, err)
		return
	}
	h.render(w, http.StatusOK, "app_notifications", "", h.newPage(r, "App notifications", "app_notifications", map[string]any{
		"Range":     days,
		"Summary":   summary,
		"Chart":     newBarChart(fmt.Sprintf("Notifications per day, last %d days", days), trend, []string{"Sent", "Failed"}),
		"Types":     types,
		"Failures":  failures,
		"Names":     names,
		"ScopeName": names[scope.ProjectID],
		"Env":       scope.Env,
	}))
}
