package dashboard

import (
	"net/http"
	"strconv"
)

// requestPage is the v2 destination for request insight navigation. The
// Laravel SDK does not send request-duration telemetry yet, so this page
// deliberately renders empty states until M10 supplies real measurements.
func (h *Handler) requestPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "requests", "", h.newPage(r, "Requests", "requests", nil))
}

// jobsPage reserves the all-runs destination for M10 telemetry. Until
// successful runs are collected, only the Failed card has data: it reuses
// the Queue Failures rollup for the scoped project and links there.
func (h *Handler) jobsPage(w http.ResponseWriter, r *http.Request) {
	projectID := scopeFrom(r).ProjectID
	summary, err := h.store.QueueSummary(r.Context(), projectID, queueDays)
	if err != nil {
		h.serverError(w, err)
		return
	}
	project := ""
	if projectID > 0 {
		project = strconv.FormatInt(projectID, 10)
	}
	h.render(w, http.StatusOK, "jobs", "", h.newPage(r, "Jobs", "jobs", map[string]any{
		"Failed":    summary,
		"Days":      queueDays,
		"FailedURL": queueForm{Project: project}.url(),
	}))
}
