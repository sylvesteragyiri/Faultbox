package testutil

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// FixturePath returns the path of a file under tests/fixtures.
func FixturePath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate testutil")
	}
	return filepath.Join(filepath.Dir(file), "..", "fixtures", name)
}

// FixtureBytes reads a fixture file.
func FixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(FixturePath(t, name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// Fixture loads a Laravel event fixture (tests/fixtures/laravel/<name>.json)
// as a mutable map. Each call returns a fresh copy.
func Fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(FixtureBytes(t, "laravel/"+name+".json"), &m); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return m
}

// FixtureNames lists every Laravel fixture.
var FixtureNames = []string{
	"unique_violation", "model_not_found", "undefined_array_key", "type_error", "job_timeout", "max_attempts",
}

// NewEventID returns a random UUID (version 4 layout).
func NewEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Stamp sets a fresh event_id and the current timestamp on a payload.
func Stamp(p map[string]any) map[string]any {
	p["event_id"] = NewEventID()
	p["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	return p
}

// MustJSON marshals v or fails the test.
func MustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Frames returns the exception frames of a fixture map, for mutation.
func Frames(p map[string]any) []any {
	return p["exception"].(map[string]any)["frames"].([]any)
}

// Exception returns the exception object of a fixture map, for mutation.
func Exception(p map[string]any) map[string]any {
	return p["exception"].(map[string]any)
}

// NewProjectKey creates a project directly in the store and returns its API key.
func NewProjectKey(t *testing.T, st *store.Store, name string, envs ...string) (store.Project, string) {
	t.Helper()
	key, hash, hint, err := auth.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(context.Background(), name, envs, hash, hint)
	if err != nil {
		t.Fatal(err)
	}
	return p, key
}

// Post sends a JSON body to an API path with a project key.
func (a *App) Post(path, key string, body []byte, headers ...string) Response {
	a.t.Helper()
	req, err := http.NewRequestWithContext(a.t.Context(), http.MethodPost, a.Server.URL+path, bytes.NewReader(body))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return Do(a.t, http.DefaultClient, req)
}

// PostEvent sends an event payload and fails unless it is accepted.
func (a *App) PostEvent(path, key string, payload map[string]any) Response {
	a.t.Helper()
	resp := a.Post(path, key, MustJSON(a.t, payload))
	if resp.StatusCode != http.StatusAccepted {
		a.t.Fatalf("POST %s: status %d, body %s", path, resp.StatusCode, resp.Body)
	}
	return resp
}

// WaitProcessed flushes the ingestion queue so stored data can be asserted.
func (a *App) WaitProcessed() {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Flush(ctx); err != nil {
		a.t.Fatalf("flush ingestion queue: %v", err)
	}
}

// PostRaw sends a pre-encoded event with the given client and returns only
// the status code. For load tests.
func (a *App) PostRaw(c *http.Client, path, key string, body []byte) int {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, a.Server.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}
