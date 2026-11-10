package dashboard

import "net/http"

// requestPage is the v2 destination for request insight navigation. The
// Laravel SDK does not send request-duration telemetry yet, so this page
// deliberately renders empty states until M10 supplies real measurements.
func (h *Handler) requestPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "requests", "", h.newPage(r, "Requests", "requests", nil))
}

// jobsPage reserves the all-runs destination for M10 telemetry. Existing
// failed-job detail remains available through the link in the empty state.
func (h *Handler) jobsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "jobs", "", h.newPage(r, "Jobs", "jobs", nil))
}
