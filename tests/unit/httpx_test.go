package unit

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/httpx"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestClientIP(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")

	if got := httpx.ClientIP(r, false); got != "10.0.0.5" {
		t.Errorf("untrusted proxy: got %q, want the socket address", got)
	}
	if got := httpx.ClientIP(r, true); got != "203.0.113.9" {
		t.Errorf("trusted proxy: got %q, want the first forwarded address", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := httpx.ClientIP(r, true); got != "10.0.0.5" {
		t.Errorf("garbage header: got %q, want the socket address", got)
	}
}

func TestIsHTTPS(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if httpx.IsHTTPS(r, false) {
		t.Error("trusted X-Forwarded-Proto without TrustProxy")
	}
	if !httpx.IsHTTPS(r, true) {
		t.Error("ignored X-Forwarded-Proto with TrustProxy")
	}
	r.TLS = &tls.ConnectionState{}
	r.Header.Del("X-Forwarded-Proto")
	if !httpx.IsHTTPS(r, false) {
		t.Error("direct TLS not detected")
	}
}

func TestJSONMuxErrors(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /thing", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	h := httpx.JSONMuxErrors(mux)

	for _, tt := range []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/missing", http.StatusNotFound, "not_found"},
		{http.MethodPost, "/thing", http.StatusMethodNotAllowed, "method_not_allowed"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))
		if rec.Code != tt.status {
			t.Fatalf("%s %s: status %d, want %d", tt.method, tt.path, rec.Code, tt.status)
		}
		body := testutil.DecodeJSON[httpx.ErrorBody](t, rec.Body)
		if body.Error.Code != tt.code {
			t.Fatalf("%s %s: code %q, want %q", tt.method, tt.path, body.Error.Code, tt.code)
		}
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/thing", nil))
	if rec.Body.String() != "ok" {
		t.Fatalf("normal response altered: %q", rec.Body.String())
	}
}
