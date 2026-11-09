// Package unit tests handlers and functions in isolation, in memory,
// without opening network sockets or touching the filesystem.
package unit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/server"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestHealthHandlerReturnsOK(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	server.New().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

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
	server.New().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}
