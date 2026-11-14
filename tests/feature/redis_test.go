package feature

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestRedisDashboardAddsNamedServersWithOptionalAlerts(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	page := a.Get("/redis")
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Body, "Add a Redis server") {
		t.Fatalf("Redis page: %d", page.StatusCode)
	}
	post := func(name, endpoint, alerts string) testutil.Response {
		return a.PostForm("/redis", url.Values{"name": {name}, "endpoint": {endpoint}, "enabled": {"1"}, "alert_enabled": {alerts}}, a.CSRF("/redis"))
	}
	resp := post("Production cache", "redis://:highly-secret-password@localhost:6379/0", "1")
	if resp.Request.URL.Path != "/redis" || !strings.Contains(resp.Body, "Production cache") || strings.Contains(resp.Body, "highly-secret-password") {
		t.Fatalf("saved Redis target response URL=%s contains expected name/secret policy: %v", resp.Request.URL, resp.Body)
	}
	resp = post("Queue cache", "redis://localhost:6379/1", "")
	if !strings.Contains(resp.Body, "Queue cache") {
		t.Fatal("second Redis target was not listed")
	}
	items, err := a.Store().RedisTargets(t.Context())
	if err != nil || len(items) != 2 {
		t.Fatalf("targets = %d, %v", len(items), err)
	}
	keys, err := auth.DeriveKeys([]byte(testutil.Secret))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := auth.Unseal(keys.Encryption, items[0].Endpoint)
	if err != nil || strings.Contains(string(items[0].Endpoint), "highly-secret-password") || !strings.Contains(string(plain), "highly-secret-password") {
		t.Fatalf("endpoint encryption failed: %q, %v", plain, err)
	}
}

func TestRedisDashboardRejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	resp := a.PostForm("/redis", url.Values{"name": {"Invalid"}, "endpoint": {"http://localhost"}}, a.CSRF("/redis"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(resp.Body, "Redis URL") {
		t.Fatalf("invalid endpoint response: %d %s", resp.StatusCode, resp.Body)
	}
}
