// Package integration tests components against real dependencies, mainly
// a SQLite database file in a temporary directory, without HTTP.
package integration

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/auth"
	"github.com/sylvesteragyiri/faultbox/internal/store"
	"github.com/sylvesteragyiri/faultbox/tests/testutil"
)

func newKey(t *testing.T) ([]byte, string) {
	t.Helper()
	_, hash, hint, err := auth.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	return hash, hint
}

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "faultbox.db")

	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := st.SchemaVersion(ctx)
	if err != nil || v1 < 1 {
		t.Fatalf("schema version = %d, %v", v1, err)
	}
	hash, hint := newKey(t)
	if _, err := st.CreateProject(ctx, "Persisted", nil, hash, hint); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	v2, _ := st.SchemaVersion(ctx)
	if v2 != v1 {
		t.Fatalf("schema version changed on reopen: %d → %d", v1, v2)
	}
	projects, err := st.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Name != "Persisted" {
		t.Fatalf("data did not persist: %v, %v", projects, err)
	}
}

func TestProjectLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)

	hash, hint := newKey(t)
	p, err := st.CreateProject(ctx, "Billing API", []string{"production", "staging"}, hash, hint)
	if err != nil {
		t.Fatal(err)
	}
	if p.Slug != "billing-api" || !p.Active() || p.APIKeyHint != hint {
		t.Fatalf("unexpected project: %+v", p)
	}
	if !slices.Equal(p.Environments, []string{"production", "staging"}) {
		t.Fatalf("environments = %v", p.Environments)
	}

	got, err := st.ProjectByAPIKeyHash(ctx, hash)
	if err != nil || got.ID != p.ID {
		t.Fatalf("lookup by key hash: %+v, %v", got, err)
	}

	if err := st.UpdateProject(ctx, p.ID, "Billing", []string{"local"}, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Project(ctx, p.ID)
	if got.Name != "Billing" || got.Slug != "billing-api" || !slices.Equal(got.Environments, []string{"local"}) {
		t.Fatalf("after update: %+v (slug must not change)", got)
	}

	if err := st.SetProjectStatus(ctx, p.ID, store.ProjectDisabled); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.Project(ctx, p.ID); got.Active() {
		t.Fatal("project still active after disabling")
	}
	if active, disabled, _ := st.CountProjects(ctx); active != 0 || disabled != 1 {
		t.Fatalf("counts = %d active, %d disabled", active, disabled)
	}

	newHash, newHint := newKey(t)
	if err := st.RotateProjectKey(ctx, p.ID, newHash, newHint); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProjectByAPIKeyHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old key still resolves: %v", err)
	}
	if got, err := st.ProjectByAPIKeyHash(ctx, newHash); err != nil || got.APIKeyHint != newHint {
		t.Fatalf("new key: %+v, %v", got, err)
	}

	if err := st.SetProjectStatus(ctx, 9999, store.ProjectActive); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project: err = %v, want ErrNotFound", err)
	}
}

func TestProjectSlugsAreUnique(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)

	var slugs []string
	for range 3 {
		hash, hint := newKey(t)
		p, err := st.CreateProject(ctx, "Shop", nil, hash, hint)
		if err != nil {
			t.Fatal(err)
		}
		slugs = append(slugs, p.Slug)
	}
	if want := []string{"shop", "shop-2", "shop-3"}; !slices.Equal(slugs, want) {
		t.Fatalf("slugs = %v, want %v", slugs, want)
	}
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)

	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, hash, hint, err := auth.NewAPIKey()
			if err == nil {
				_, err = st.CreateProject(ctx, "Concurrent", []string{"production"}, hash, hint)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	projects, err := st.Projects(ctx)
	if err != nil || len(projects) != n {
		t.Fatalf("got %d projects, want %d (%v)", len(projects), n, err)
	}
}

func TestUsersAndSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := testutil.OpenStore(t)
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	st.SetClock(func() time.Time { return now })

	u, err := st.CreateUser(ctx, "Dev@Example.com", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, "dev@example.com", "hash"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate email (different case): err = %v, want ErrConflict", err)
	}
	if got, _, err := st.UserByEmail(ctx, "DEV@example.COM"); err != nil || got.ID != u.ID {
		t.Fatalf("case-insensitive lookup: %+v, %v", got, err)
	}

	_, tokenHash, _ := auth.NewToken()
	if _, err := st.CreateSession(ctx, u.ID, tokenHash, time.Hour); err != nil {
		t.Fatal(err)
	}
	sess, su, err := st.SessionUser(ctx, tokenHash)
	if err != nil || su.ID != u.ID {
		t.Fatalf("session lookup: %+v, %v", su, err)
	}
	if got, _, _ := st.UserByEmail(ctx, u.Email); !got.LastLoginAt.Equal(now) {
		t.Fatalf("last_login_at = %v, want %v", got.LastLoginAt, now)
	}

	now = now.Add(50 * time.Minute)
	if err := st.ExtendSession(ctx, sess.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute) // 100 min after login, 50 after the extension
	if _, _, err := st.SessionUser(ctx, tokenHash); err != nil {
		t.Fatalf("extended session expired early: %v", err)
	}
	now = now.Add(11 * time.Minute)
	if _, _, err := st.SessionUser(ctx, tokenHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session still valid: %v", err)
	}

	_, other, _ := auth.NewToken()
	if _, err := st.CreateSession(ctx, u.ID, other, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSession(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SessionUser(ctx, other); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted session still valid: %v", err)
	}
}
