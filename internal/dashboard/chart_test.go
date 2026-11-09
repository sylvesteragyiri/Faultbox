package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func days(n int, f func(i int) (int64, int64)) []store.DayCount {
	start := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	out := make([]store.DayCount, n)
	for i := range out {
		e, q := f(i)
		out[i] = store.DayCount{Day: start.AddDate(0, 0, i), Errors: e, QueueFailures: q}
	}
	return out
}

func TestNiceScale(t *testing.T) {
	for _, tt := range []struct{ max, top, step int64 }{
		{0, 4, 1}, {1, 1, 1}, {7, 8, 2}, {9, 10, 5}, {37, 40, 10}, {1234, 1500, 500}, {98000, 100000, 25000},
	} {
		top, step := niceScale(tt.max)
		if top < tt.max || top%step != 0 || top/step > 5 {
			t.Errorf("niceScale(%d) = %d, %d: top must cover max in at most 5 steps", tt.max, top, step)
		}
		if top != tt.top {
			t.Logf("niceScale(%d) = %d/%d (expected %d/%d)", tt.max, top, step, tt.top, tt.step)
		}
	}
}

func TestCompact(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1284: "1.3K", 2000: "2K", 12900: "12K", 4_200_000: "4.2M", 1_000_000: "1M"} {
		if got := compact(in); got != want {
			t.Errorf("compact(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBarChartGeometry(t *testing.T) {
	c := newBarChart("test", days(30, func(i int) (int64, int64) { return int64(i % 5), int64(i % 3) }), []string{"Errors", "Failed jobs"})

	if len(c.Bars) != 30 || len(c.Rows) != 30 || len(c.Series) != 2 {
		t.Fatalf("bars %d, rows %d, series %d", len(c.Bars), len(c.Rows), len(c.Series))
	}
	for _, b := range c.Bars {
		if len(b.Segments) > 0 && !strings.Contains(b.Segments[len(b.Segments)-1].Path, "q") {
			t.Errorf("top segment of %q is not rounded", b.Tip)
		}
		for _, s := range b.Segments[:max(0, len(b.Segments)-1)] {
			if strings.Contains(s.Path, "q") {
				t.Errorf("lower segment of %q is rounded; only the data end may be", b.Tip)
			}
		}
	}
	// Zero days draw nothing but stay hoverable.
	if len(c.Bars[0].Segments) != 0 || c.Bars[0].HitW <= 0 {
		t.Errorf("empty day: %+v", c.Bars[0])
	}
	// Date labels never crowd each other, and the latest day is labelled.
	for i := 1; i < len(c.XLabels); i++ {
		if gap := c.XLabels[i].X - c.XLabels[i-1].X; gap < 60 {
			t.Errorf("labels %q and %q are only %.0f units apart", c.XLabels[i-1].Label, c.XLabels[i].Label, gap)
		}
	}
	if last := c.XLabels[len(c.XLabels)-1].Label; last != "9 Nov" {
		t.Errorf("last label %q, want the latest day", last)
	}
	if !strings.Contains(c.Bars[29].Tip, "Errors: 4") || !strings.Contains(c.Bars[29].Tip, "Failed jobs: 2") {
		t.Errorf("tooltip = %q", c.Bars[29].Tip)
	}
}

func TestColumnWidthIsCapped(t *testing.T) {
	c := newBarChart("few days", days(3, func(int) (int64, int64) { return 5, 0 }), []string{"Events"})
	// "M<x> <y>v-..." – width comes from the h segment of a rounded path.
	for _, b := range c.Bars {
		p := b.Segments[0].Path
		if !strings.Contains(p, "h16") { // 24px column minus two 4px corners
			t.Errorf("column wider or narrower than the 24px cap: %s", p)
		}
	}
	if !newBarChart("empty", days(7, func(int) (int64, int64) { return 0, 0 }), []string{"Events"}).Empty {
		t.Error("all-zero chart not flagged empty")
	}
}

func TestBuildTraceCollapsesVendorFrames(t *testing.T) {
	tv := buildTrace(`{"frames":[
		{"file":"/var/www/vendor/laravel/framework/src/Database/Connection.php","line":825,"function":"run","class":"Illuminate\\Database\\Connection"},
		{"file":"/var/www/vendor/laravel/framework/src/Database/Query/Builder.php","line":3717,"function":"insert"},
		{"file":"/var/www/app/Actions/CreateUser.php","line":34,"function":"create","class":"App\\Actions\\CreateUser"},
		{"file":"/var/www/vendor/laravel/framework/src/Routing/Route.php","line":266,"function":"dispatch"}
	],"frames_omitted":12}`)

	if len(tv.Groups) != 3 || !tv.Groups[0].Collapsed || tv.Groups[1].Collapsed || !tv.Groups[2].Collapsed {
		t.Fatalf("groups = %+v", tv.Groups)
	}
	app := tv.Groups[1].Frames[0]
	if !app.InApp || app.Function != "App\\Actions\\CreateUser::create" || app.Location != "app/Actions/CreateUser.php:34" {
		t.Fatalf("app frame = %+v", app)
	}
	if tv.Omitted != 12 {
		t.Fatalf("omitted = %d", tv.Omitted)
	}

	// With no application frames at all, nothing is hidden.
	vendorOnly := buildTrace(`{"frames":[{"file":"/x/vendor/a.php","function":"a"},{"file":"/x/vendor/b.php","function":"b"}]}`)
	if len(vendorOnly.Groups) != 1 || vendorOnly.Groups[0].Collapsed {
		t.Fatalf("vendor-only trace = %+v", vendorOnly.Groups)
	}
	if !buildTrace("").Empty {
		t.Fatal("empty trace not flagged")
	}
}

func TestPrettyJSONAndRequest(t *testing.T) {
	if prettyJSON(`{}`) != "" || prettyJSON("null") != "" || prettyJSON("") != "" {
		t.Fatal("empty objects should render nothing")
	}
	if got := prettyJSON(`{"a":1}`); got != "{\n  \"a\": 1\n}" {
		t.Fatalf("prettyJSON = %q", got)
	}
	rq := buildRequest(`{"url":"https://x.test/a","method":"POST","status_code":500,"headers":{"b":"2","a":"1"}}`)
	if rq == nil || rq.Status != 500 || rq.Headers[0][0] != "a" {
		t.Fatalf("request = %+v", rq)
	}
}
