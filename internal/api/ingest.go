package api

import (
	"compress/gzip"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/internal/ingest"
)

// IngestOptions wires the ingestion endpoints to the queue.
type IngestOptions struct {
	Queue   *ingest.Queue
	Metrics *ingest.Metrics
	Limiter *ingest.RateLimiter
	// MaxBody is the largest accepted request body after decompression (D14).
	MaxBody int64
	// Now is the clock used for timestamp validation. Defaults to time.Now.
	Now func() time.Time
	// OnResults receives the issue events a telemetry batch stored (failed
	// notifications), so they raise alerts like any other event. It must
	// not block.
	OnResults ingest.ResultHook
}

// AcceptedResponse is the 202 body. Duplicates get the same answer (D11).
type AcceptedResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// ValidationErrorBody is the 422 body: the standard envelope plus details.
type ValidationErrorBody struct {
	Error struct {
		Code    string              `json:"code"`
		Message string              `json:"message"`
		Details []ingest.FieldError `json:"details"`
	} `json:"error"`
}

// ingestHandler serves POST /api/v1/events and POST /api/v1/queue-failures.
// It validates and queues, then answers 202 before anything touches the
// database (PRD §4.2).
func (h *Handler) ingestHandler(kind string) http.HandlerFunc {
	o := h.ingest
	return func(w http.ResponseWriter, r *http.Request) {
		project, _ := ProjectFrom(r.Context())

		if ok, wait := o.Limiter.Allow(project.ID); !ok {
			o.Metrics.Reject(ingest.RejectRateLimited)
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			httpx.WriteError(w, http.StatusTooManyRequests, ingest.RejectRateLimited,
				"This project is sending events faster than its rate limit allows.")
			return
		}

		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			o.Metrics.Reject(ingest.RejectMediaType)
			httpx.WriteError(w, http.StatusUnsupportedMediaType, ingest.RejectMediaType,
				"Send the event as JSON with 'Content-Type: application/json'.")
			return
		}

		body, err := readBody(w, r, o.MaxBody)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) || errors.Is(err, errBodyTooLarge) {
				o.Metrics.Reject(ingest.RejectTooLarge)
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, ingest.RejectTooLarge,
					"The event is larger than "+strconv.FormatInt(o.MaxBody>>10, 10)+" KB after decompression.")
				return
			}
			o.Metrics.Reject(ingest.RejectInvalidJSON)
			httpx.WriteError(w, http.StatusBadRequest, ingest.RejectInvalidJSON, "The request body could not be read: "+err.Error())
			return
		}

		payload, err := ingest.Decode(strings.NewReader(body))
		if err != nil {
			o.Metrics.Reject(ingest.RejectInvalidJSON)
			httpx.WriteError(w, http.StatusBadRequest, ingest.RejectInvalidJSON, err.Error())
			return
		}

		ev, err := ingest.Validate(payload, kind, project.ID, project.Environments, o.Now(), len(body))
		if err != nil {
			o.Metrics.Reject(ingest.RejectValidation)
			var ve *ingest.ValidationError
			if !errors.As(err, &ve) {
				httpx.WriteError(w, http.StatusUnprocessableEntity, ingest.RejectValidation, err.Error())
				return
			}
			var resp ValidationErrorBody
			resp.Error.Code = ingest.RejectValidation
			resp.Error.Message = "The event failed validation."
			resp.Error.Details = ve.Fields
			httpx.WriteJSON(w, http.StatusUnprocessableEntity, resp)
			return
		}

		switch err := o.Queue.Enqueue(ev); {
		case errors.Is(err, ingest.ErrQueueFull):
			o.Metrics.Reject(ingest.RejectQueueFull)
			w.Header().Set("Retry-After", "5")
			httpx.WriteError(w, http.StatusServiceUnavailable, ingest.RejectQueueFull,
				"The server is busy. Retry the same event (same event_id) later.")
			return
		case errors.Is(err, ingest.ErrQueueClosed):
			o.Metrics.Reject(ingest.RejectShuttingDown)
			w.Header().Set("Retry-After", "5")
			httpx.WriteError(w, http.StatusServiceUnavailable, ingest.RejectShuttingDown,
				"The server is shutting down. Retry the same event later.")
			return
		case err != nil:
			h.logger.Error("enqueue event", "error", err)
			httpx.WriteError(w, http.StatusInternalServerError, "internal_error", "The event could not be queued.")
			return
		}

		o.Metrics.Accepted.Add(1)
		httpx.WriteJSON(w, http.StatusAccepted, AcceptedResponse{ID: ev.UUID, Status: "accepted"})
	}
}

var errBodyTooLarge = errors.New("body too large")

// readBody reads at most maxBody bytes, transparently decompressing
// "Content-Encoding: gzip". Both the compressed and the decompressed size
// are capped, so a small gzip bomb can't expand without limit.
func readBody(w http.ResponseWriter, r *http.Request, maxBody int64) (string, error) {
	src := io.Reader(http.MaxBytesReader(w, r.Body, maxBody))
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(src)
		if err != nil {
			return "", errors.New("invalid gzip data")
		}
		defer func() { _ = gz.Close() }()
		src = gz
	default:
		return "", errors.New("unsupported Content-Encoding " + strconv.Quote(enc) + "; use gzip or none")
	}
	b, err := io.ReadAll(io.LimitReader(src, maxBody+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxBody {
		return "", errBodyTooLarge
	}
	return string(b), nil
}
