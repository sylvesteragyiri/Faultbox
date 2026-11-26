package feature

import (
	"bufio"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func TestRedisDashboardAddsNamedServersWithOptionalAlerts(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	page := a.Get("/redis")
	if page.StatusCode != http.StatusOK || !strings.Contains(page.Body, `href="/redis/new"`) || strings.Contains(page.Body, `name="endpoint"`) {
		t.Fatalf("Redis page: %d", page.StatusCode)
	}
	newPage := a.Get("/redis/new")
	if newPage.StatusCode != http.StatusOK || !strings.Contains(newPage.Body, "Add Redis server") ||
		!strings.Contains(newPage.Body, `action="/redis"`) || !strings.Contains(newPage.Body, `name="enabled" value="1" checked`) ||
		!strings.Contains(newPage.Body, `formaction="/redis/test"`) || !strings.Contains(newPage.Body, `hx-target="#redis-test-result"`) ||
		strings.Contains(newPage.Body, `formtarget="_blank"`) {
		t.Fatalf("new Redis page: %d", newPage.StatusCode)
	}
	// Pressing Enter submits the first submit button, so saving must come
	// before the connection test.
	if save, test := strings.Index(newPage.Body, ">Add Redis server</button>"), strings.Index(newPage.Body, `formaction="/redis/test"`); save < 0 || save > test {
		t.Fatalf("save button must precede the test button: save=%d test=%d", save, test)
	}
	post := func(name, endpoint, alerts string) testutil.Response {
		return a.PostForm("/redis", url.Values{"name": {name}, "endpoint": {endpoint}, "enabled": {"1"}, "alert_enabled": {alerts}}, a.CSRF("/redis/new"))
	}
	resp := post("Production cache", "redis://:highly-secret-password@localhost:6379/0", "1")
	if !strings.HasPrefix(resp.Request.URL.Path, "/redis/") || strings.Contains(resp.Body, "highly-secret-password") ||
		!strings.Contains(resp.Body, "Settings") || !strings.Contains(resp.Body, `formaction="/redis/1/test"`) ||
		!strings.Contains(resp.Body, "Waiting for the first check") || !strings.Contains(resp.Body, `class="card danger-zone"`) ||
		strings.Contains(resp.Body, `formtarget="_blank"`) {
		t.Fatalf("saved Redis target response URL=%s contains expected name/secret policy: %v", resp.Request.URL, resp.Body)
	}
	resp = a.Get("/redis")
	if !strings.Contains(resp.Body, "Production cache") || !strings.Contains(resp.Body, ">Waiting</span>") {
		t.Fatal("created Redis target was not listed on the overview as waiting")
	}
	resp = post("Queue cache", "redis://localhost:6379/1", "")
	if !strings.Contains(resp.Body, "Queue cache") || !strings.Contains(a.Get("/redis").Body, "Queue cache") {
		t.Fatal("second Redis target was not added and listed")
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

func TestRedisOverviewShowsStatusAndMemoryUse(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	ctx := t.Context()
	st := a.Store()
	healthy, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Busy cache", Endpoint: []byte("sealed"), Enabled: true, AlertEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	failing, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Broken cache", Endpoint: []byte("sealed"), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Paused cache", Endpoint: []byte("sealed")}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.RecordRedisCheck(ctx, store.RedisSample{TargetID: healthy, SampledAt: now, UsedMemory: 85 << 20, MaxMemory: 100 << 20, ConnectedClients: 7, KeyCount: 1200, UptimeSeconds: 3600}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordRedisCheck(ctx, store.RedisSample{TargetID: failing}, "Redis refused the connection; check the host and port."); err != nil {
		t.Fatal(err)
	}
	body := a.Get("/redis").Body
	for _, want := range []string{
		">Healthy</span>", ">Failing</span>", ">Paused</span>",
		"85.0 MiB", "100.0 MiB", `class="warn-text">85%`, `redis-meter is-warn`, `width="85"`,
		"Redis refused the connection; check the host and port.",
		`<span class="stat-label">Failing</span><span class="stat-value">1</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview is missing %q:\n%s", want, body)
		}
	}
	// The label and the stored reason must not repeat each other.
	if strings.Contains(body, "Check failed") {
		t.Fatal("overview still shows the redundant 'Check failed' label")
	}
}

func TestRedisTargetPageShowsChartAndLastFailure(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	ctx := t.Context()
	st := a.Store()
	id, err := st.SaveRedisTarget(ctx, store.RedisTarget{Name: "Main", Endpoint: []byte("sealed"), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	path := "/redis/" + strconv.FormatInt(id, 10)
	if body := a.Get(path).Body; !strings.Contains(body, "Memory history appears after the first successful check") || strings.Contains(body, `class="redis-line"`) {
		t.Fatalf("empty target page should explain the missing chart:\n%s", body)
	}
	start := time.Now().UTC().Add(-time.Hour)
	for i, used := range []int64{40, 60, 90} {
		sample := store.RedisSample{TargetID: id, SampledAt: start.Add(time.Duration(i) * 10 * time.Minute), UsedMemory: used << 20, MaxMemory: 100 << 20, ConnectedClients: 3, KeyCount: 50, UptimeSeconds: 600}
		if err := st.RecordRedisCheck(ctx, sample, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordRedisCheck(ctx, store.RedisSample{TargetID: id}, "Redis did not respond in time."); err != nil {
		t.Fatal(err)
	}
	body := a.Get(path).Body
	for _, want := range []string{
		`class="redis-line"`, `class="redis-limit"`, "maxmemory 100.0 MiB", "Peak 90.0 MiB, latest 90.0 MiB.",
		"Last check failed.", "Redis did not respond in time.", "The figures below are from the last successful check",
		`class="error-text">90%`, `redis-meter is-danger`, ">Failing</span>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("target page is missing %q:\n%s", want, body)
		}
	}
}

func TestRedisConnectionTestDoesNotSaveTarget(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	_ = a.Get("/redis/new")
	resp := a.PostForm("/redis/test", url.Values{
		"name": {"Temporary"}, "endpoint": {"http://localhost"}, "enabled": {"1"},
	}, a.CSRF("/redis/new"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, "Connection failed") ||
		!strings.Contains(resp.Body, "does not save") || !strings.Contains(resp.Body, "Enter a Redis URL such as") {
		t.Fatalf("connection test response: %d %s", resp.StatusCode, resp.Body)
	}
	targets, err := a.Store().RedisTargets(t.Context())
	if err != nil || len(targets) != 0 {
		t.Fatalf("connection test persisted a target: %+v, %v", targets, err)
	}
}

func TestRedisConnectionTestExplainsServerRefusal(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	listener := replyOnceServer(t, "-NOAUTH Authentication required.\r\n")
	_ = a.Get("/redis/new")
	resp := a.PostForm("/redis/test", url.Values{
		"name": {"Temporary"}, "endpoint": {"redis://" + listener.Addr().String()},
	}, a.CSRF("/redis/new"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Body, "Redis requires a password; add it to the URL.") ||
		strings.Contains(resp.Body, "Authentication required") {
		t.Fatalf("connection test should explain NOAUTH without echoing server text: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestRedisDashboardRejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	_ = a.Get("/redis/new")
	resp := a.PostForm("/redis", url.Values{"name": {"Invalid name kept"}, "endpoint": {"http://:secret-value@localhost"}, "alert_enabled": {"1"}}, a.CSRF("/redis/new"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(resp.Body, "Redis URL") {
		t.Fatalf("invalid endpoint response: %d %s", resp.StatusCode, resp.Body)
	}
	// The form is shown again with the error inline and the safe values kept,
	// but the URL (which may hold a password) is never echoed back.
	if !strings.Contains(resp.Body, `class="alert alert-error"`) || !strings.Contains(resp.Body, `value="Invalid name kept"`) ||
		!strings.Contains(resp.Body, `name="alert_enabled" value="1" checked`) || strings.Contains(resp.Body, "secret-value") {
		t.Fatalf("invalid endpoint should re-render the form inline:\n%s", resp.Body)
	}
	if resp := a.PostForm("/redis", url.Values{"name": {"  "}, "endpoint": {"redis://localhost"}}, a.CSRF("/redis/new")); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(resp.Body, "Server name is required") {
		t.Fatalf("empty name response: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestRedisDashboardRejectsDuplicateNameAndBadEdit(t *testing.T) {
	t.Parallel()
	a := testutil.NewApp(t)
	a.LoginAdmin()
	id, err := a.Store().SaveRedisTarget(t.Context(), store.RedisTarget{Name: "Main", Endpoint: []byte("sealed"), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	resp := a.PostForm("/redis", url.Values{"name": {"Main"}, "endpoint": {"redis://localhost"}}, a.CSRF("/redis/new"))
	if resp.StatusCode != http.StatusConflict || !strings.Contains(resp.Body, "already exists") || !strings.Contains(resp.Body, "Add Redis server") {
		t.Fatalf("duplicate name response: %d %s", resp.StatusCode, resp.Body)
	}
	path := "/redis/" + strconv.FormatInt(id, 10)
	resp = a.PostForm(path, url.Values{"name": {"Renamed"}, "endpoint": {"redis://localhost:99999"}, "enabled": {"1"}}, a.CSRF(path))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(resp.Body, "port must be between") ||
		!strings.Contains(resp.Body, `value="Renamed"`) || !strings.Contains(resp.Body, "Memory history") {
		t.Fatalf("bad edit response: %d %s", resp.StatusCode, resp.Body)
	}
	if target, err := a.Store().RedisTarget(t.Context(), id); err != nil || target.Name != "Main" {
		t.Fatalf("bad edit changed the target: %+v, %v", target, err)
	}
	if resp := a.PostForm("/redis/9999", url.Values{"name": {"Ghost"}}, a.CSRF(path)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("editing a missing target: %d", resp.StatusCode)
	}
}

// replyOnceServer accepts one connection, reads the first RESP command, and
// writes reply.
func replyOnceServer(t *testing.T, reply string) net.Listener {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		header, err := r.ReadString('\n')
		if err != nil || !strings.HasPrefix(header, "*") {
			return
		}
		n, _ := strconv.Atoi(strings.TrimSpace(header[1:]))
		for range n * 2 {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
		}
		_, _ = conn.Write([]byte(reply))
	}()
	return listener
}
