package dashboard

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func formFor(query string) issueForm {
	return parseIssueForm(httptest.NewRequestWithContext(context.Background(), "GET", "/issues?"+query, nil))
}

func TestIssueStatusLabel(t *testing.T) {
	for query, want := range map[string]string{
		"":                               "Open",
		"status=resolved":                "Resolved",
		"status=ignored":                 "Ignored",
		"status=all":                     "All status",
		"assigned=mine":                  "Mine",
		"assigned=unassigned":            "Unassigned",
		"status=resolved&assigned=mine":  "Mine · Resolved",
		"status=bogus&assigned=whatever": "Open", // invalid values fall back to the default
	} {
		if got := formFor(query).StatusLabel(); got != want {
			t.Errorf("%q: label %q, want %q", query, got, want)
		}
	}
}

func TestIssueStatusOptionsMarkOneCurrent(t *testing.T) {
	for _, query := range []string{"", "status=resolved", "assigned=mine", "status=all&q=x"} {
		opts := formFor(query).StatusOptions()
		current := 0
		for _, o := range opts {
			if o.Current {
				current++
			}
		}
		if len(opts) != 6 || current != 1 {
			t.Errorf("%q: %d options, %d current; want 6 and 1", query, len(opts), current)
		}
	}
	// The menu keeps the search and filters, and resets the page.
	opts := formFor("q=boom&env=prod&page=3").StatusOptions()
	if got := opts[3].URL; got != "/issues?env=prod&q=boom&status=resolved" {
		t.Errorf("Resolved links to %s", got)
	}
}

func TestIssueActiveFilters(t *testing.T) {
	projects := []store.Project{{ID: 7, Name: "Shop"}}
	f := formFor(`project=7&env=staging&release=v2&type=App\Exceptions\Boom&range=7d&q=x&sort=count&page=2`)
	chips := f.ActiveFilters(projects)
	want := []filterChip{
		{"Project", "Shop", "/issues?env=staging&q=x&range=7d&release=v2&sort=count&type=App%5CExceptions%5CBoom"},
		{"Environment", "staging", "/issues?project=7&q=x&range=7d&release=v2&sort=count&type=App%5CExceptions%5CBoom"},
		{"Release", "v2", "/issues?env=staging&project=7&q=x&range=7d&sort=count&type=App%5CExceptions%5CBoom"},
		{"Type", "Boom", "/issues?env=staging&project=7&q=x&range=7d&release=v2&sort=count"},
		{"Seen in", "7 days", "/issues?env=staging&project=7&q=x&release=v2&sort=count&type=App%5CExceptions%5CBoom"},
	}
	if len(chips) != len(want) {
		t.Fatalf("got %d chips, want %d: %+v", len(chips), len(want), chips)
	}
	for i := range want {
		if chips[i] != want[i] {
			t.Errorf("chip %d = %+v, want %+v", i, chips[i], want[i])
		}
	}
	if n := f.FilterCount(); n != 5 {
		t.Errorf("filter count %d, want 5", n)
	}
	if got := f.ClearFiltersURL(); got != "/issues?q=x&sort=count" {
		t.Errorf("clear filters links to %s", got)
	}

	// Search, status and sort aren't Filters-menu filters.
	plain := formFor("q=x&status=all&sort=count")
	if len(plain.ActiveFilters(projects)) != 0 || plain.FilterCount() != 0 {
		t.Error("search, status and sort should not show as filter chips")
	}
	// "project=" overrides a scope to show every project.
	all := formFor("project=")
	all.scope = Scope{ProjectID: 7}
	if chips := all.ActiveFilters(projects); len(chips) != 1 || chips[0].Value != "All projects" {
		t.Errorf("project override chips: %+v", chips)
	}
}
