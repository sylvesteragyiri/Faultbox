package unit

import (
	"slices"
	"strings"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"My App":                "my-app",
		"  Billing -- API!! ":   "billing-api",
		"Ünïcode Shop":          "n-code-shop",
		"!!!":                   "project",
		"":                      "project",
		"v2.0 backend":          "v2-0-backend",
		strings.Repeat("a", 80): strings.Repeat("a", 60),
	}
	for in, want := range tests {
		if got := store.Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeEnvironments(t *testing.T) {
	t.Parallel()

	got, err := store.NormalizeEnvironments(" Production, staging  local,staging\n")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"local", "production", "staging"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	empty, err := store.NormalizeEnvironments("  ")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input: got %v, %v", empty, err)
	}

	for _, bad := range []string{"prod/eu", "pr od;", strings.Repeat("x", 65)} {
		if _, err := store.NormalizeEnvironments(bad); err == nil {
			t.Errorf("NormalizeEnvironments(%q) accepted invalid input", bad)
		}
	}

	var many []string
	for i := range store.MaxEnvironments + 1 {
		many = append(many, "env"+string(rune('a'+i)))
	}
	if _, err := store.NormalizeEnvironments(strings.Join(many, ",")); err == nil {
		t.Error("accepted more than MaxEnvironments")
	}
}

func TestNormalizeProjectName(t *testing.T) {
	t.Parallel()

	got, err := store.NormalizeProjectName("  Billing \t  API ")
	if err != nil || got != "Billing API" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := store.NormalizeProjectName("   "); err == nil {
		t.Error("blank name accepted")
	}
	if _, err := store.NormalizeProjectName(strings.Repeat("n", store.MaxProjectNameLen+1)); err == nil {
		t.Error("over-long name accepted")
	}
}
