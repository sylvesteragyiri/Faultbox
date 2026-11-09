// Package feature tests user-visible behaviour end to end over real HTTP,
// against a full Faultbox instance with a temporary database.
package feature

import (
	"net/http"
	"testing"

	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestHealthAndReadyOverHTTP(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)

	for path, want := range map[string]string{"/health": "ok", "/ready": "ready"} {
		resp := a.API(http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, resp.StatusCode, resp.Body)
		}
		if !contains(resp.Body, `"status":"`+want+`"`) {
			t.Fatalf("GET %s: body %s, want status %q", path, resp.Body, want)
		}
	}
}

func TestReadyFailsWhenDatabaseIsClosed(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	if err := a.Store().Close(); err != nil {
		t.Fatal(err)
	}

	resp := a.API(http.MethodGet, "/ready", "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
	if !contains(resp.Body, `"database":"unavailable"`) {
		t.Fatalf("body %s", resp.Body)
	}
	if resp := a.API(http.MethodGet, "/health", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("liveness must not depend on the database: status %d", resp.StatusCode)
	}
}
