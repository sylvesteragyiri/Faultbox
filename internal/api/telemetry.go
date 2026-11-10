package api

import (
	"errors"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
)

// TelemetryResponse is the 202 body of POST /api/v1/telemetry.
type TelemetryResponse struct {
	Status string `json:"status"` // accepted, or duplicate for a batch already stored
	Count  int    `json:"count"`
}

// telemetryHandler serves POST /api/v1/telemetry (and its older alias
// POST /api/v1/notifications): the Laravel notifications, mail and artisan
// commands one request, job or command produced. It shares the events' rate
// limit, size limit and error format. A batch is a handful of counter
// updates, so it is written straight away in one short transaction instead
// of going through the event queue.
func (h *Handler) telemetryHandler() http.HandlerFunc {
	o := h.ingest
	return func(w http.ResponseWriter, r *http.Request) {
		project, _ := ProjectFrom(r.Context())

		if ok, wait := o.Limiter.Allow(project.ID); !ok {
			o.Metrics.Reject(ingest.RejectRateLimited)
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			httpx.WriteError(w, http.StatusTooManyRequests, ingest.RejectRateLimited,
				"This project is sending faster than its rate limit allows.")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			o.Metrics.Reject(ingest.RejectMediaType)
			httpx.WriteError(w, http.StatusUnsupportedMediaType, ingest.RejectMediaType,
				"Send the batch as JSON with 'Content-Type: application/json'.")
			return
		}
		body, err := readBody(w, r, o.MaxBody)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) || errors.Is(err, errBodyTooLarge) {
				o.Metrics.Reject(ingest.RejectTooLarge)
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, ingest.RejectTooLarge,
					"The batch is larger than "+strconv.FormatInt(o.MaxBody>>10, 10)+" KB after decompression.")
				return
			}
			o.Metrics.Reject(ingest.RejectInvalidJSON)
			httpx.WriteError(w, http.StatusBadRequest, ingest.RejectInvalidJSON, "The request body could not be read: "+err.Error())
			return
		}
		batch, err := ingest.DecodeTelemetry(strings.NewReader(body))
		if err != nil {
			o.Metrics.Reject(ingest.RejectInvalidJSON)
			httpx.WriteError(w, http.StatusBadRequest, ingest.RejectInvalidJSON, err.Error())
			return
		}
		valid, err := ingest.ValidateTelemetry(batch, project.Environments, o.Now())
		if err != nil {
			o.Metrics.Reject(ingest.RejectValidation)
			var ve *ingest.ValidationError
			var resp ValidationErrorBody
			resp.Error.Code = ingest.RejectValidation
			resp.Error.Message = "The telemetry batch failed validation."
			if errors.As(err, &ve) {
				resp.Error.Details = ve.Fields
			}
			httpx.WriteJSON(w, http.StatusUnprocessableEntity, resp)
			return
		}

		duplicate, issues, err := h.store.IngestTelemetry(r.Context(), project.ID, valid)
		if err != nil {
			h.logger.Error("store telemetry batch", "project_id", project.ID, "error", err)
			w.Header().Set("Retry-After", "5")
			httpx.WriteError(w, http.StatusServiceUnavailable, "store_failed",
				"The batch could not be stored. Retry the same batch (same batch_id) later.")
			return
		}
		if len(issues) > 0 && o.OnResults != nil {
			o.OnResults(issues)
		}
		resp := TelemetryResponse{Status: "accepted", Count: len(valid.Notifications) + len(valid.Mails) + len(valid.Commands) + len(valid.Cache)}
		if duplicate {
			resp.Status = "duplicate"
		}
		httpx.WriteJSON(w, http.StatusAccepted, resp)
	}
}
