// Package unit tests handlers and functions in isolation, in memory,
// without opening network sockets or touching the filesystem.
package unit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/server"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func newRouter(t *testing.T, checks ...server.ReadyCheck) http.Handler {
	t.Helper()
	return server.New(server.Options{Logger: testutil.Logger(t), Ready: checks})
}

func TestHealthHandlerReturnsOK(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newRouter(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	body := testutil.DecodeJSON[map[string]string](t, rec.Body)
	if body["status"] != "ok" {
		t.Fatalf("status field = %q, want ok", body["status"])
	}
}

func TestHealthHandlerRejectsWrongMethod(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newRouter(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestReadyReportsEachCheck(t *testing.T) {
	t.Parallel()

	ok := server.ReadyCheck{Name: "database", Check: func(context.Context) error { return nil }}
	broken := server.ReadyCheck{Name: "queue", Check: func(context.Context) error { return errors.New("full") }}

	tests := []struct {
		name       string
		checks     []server.ReadyCheck
		wantStatus int
		wantBody   string
	}{
		{"all ok", []server.ReadyCheck{ok}, http.StatusOK, "ready"},
		{"one failing", []server.ReadyCheck{ok, broken}, http.StatusServiceUnavailable, "not_ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			newRouter(t, tt.checks...).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ready", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			body := testutil.DecodeJSON[struct {
				Status string            `json:"status"`
				Checks map[string]string `json:"checks"`
			}](t, rec.Body)
			if body.Status != tt.wantBody {
				t.Fatalf("status field = %q, want %q", body.Status, tt.wantBody)
			}
			if body.Checks["database"] != "ok" {
				t.Fatalf("database check = %q, want ok", body.Checks["database"])
			}
			if len(tt.checks) > 1 && body.Checks["queue"] != "unavailable" {
				t.Fatalf("queue check = %q, want unavailable", body.Checks["queue"])
			}
		})
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newRouter(t).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil))

	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing %s header", h)
		}
	}
}
