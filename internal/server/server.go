// Package server mounts the API, the dashboard and the health probes on one
// standard-library mux (D3).
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/httpx"
)

// ReadyCheck reports whether one dependency can serve traffic.
type ReadyCheck struct {
	Name  string
	Check func(context.Context) error
}

// Options are the pieces the router mounts.
type Options struct {
	Logger    *slog.Logger
	API       http.Handler
	Dashboard http.Handler
	Ready     []ReadyCheck
}

// New returns the root HTTP handler.
func New(opts Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /ready", readyHandler(opts.Logger, opts.Ready))
	if opts.API != nil {
		mux.Handle("/api/", opts.API)
	}
	if opts.Dashboard != nil {
		mux.Handle("/", opts.Dashboard)
	}

	var h http.Handler = mux
	h = httpx.SecurityHeaders(h)
	h = httpx.LogRequests(opts.Logger)(h)
	h = httpx.Recover(opts.Logger)(h)
	return h
}

// handleHealth is the liveness probe: the process is up and serving HTTP.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyHandler is the readiness probe: every dependency answers in time.
func readyHandler(logger *slog.Logger, checks []ReadyCheck) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, code := "ready", http.StatusOK
		results := make(map[string]string, len(checks))
		for _, c := range checks {
			if err := c.Check(ctx); err != nil {
				logger.Warn("readiness check failed", "check", c.Name, "error", err)
				results[c.Name] = "unavailable"
				status, code = "not_ready", http.StatusServiceUnavailable
				continue
			}
			results[c.Name] = "ok"
		}
		httpx.WriteJSON(w, code, map[string]any{"status": status, "checks": results})
	}
}
