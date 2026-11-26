package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

func redisSamples(maxMemory int64, used ...int64) []store.RedisSample {
	start := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	out := make([]store.RedisSample, len(used))
	for i, u := range used {
		out[i] = store.RedisSample{SampledAt: start.Add(time.Duration(i) * 5 * time.Minute), UsedMemory: u, MaxMemory: maxMemory}
	}
	return out
}

func TestMakeRedisPlotEmpty(t *testing.T) {
	if p := makeRedisPlot(nil); !p.Empty || p.Line != "" {
		t.Fatalf("empty plot = %+v", p)
	}
}

func TestMakeRedisPlotScalesToLimitWhenClose(t *testing.T) {
	p := makeRedisPlot(redisSamples(100<<20, 40<<20, 90<<20, 80<<20))
	if p.Empty || !p.HasLimit || p.LimitOffScale || p.LimitY != p.PlotTop {
		t.Fatalf("limit should be the top of the scale: %+v", p)
	}
	if p.Peak != 90<<20 || p.Latest != 80<<20 || len(strings.Fields(p.Line)) != 3 {
		t.Fatalf("plot values = peak %d latest %d line %q", p.Peak, p.Latest, p.Line)
	}
	if !strings.HasPrefix(p.Area, "M70.0,182.0 L70.0,") || !strings.HasSuffix(p.Area, "L712.0,182.0 Z") {
		t.Fatalf("area path should close on the baseline: %q", p.Area)
	}
	if len(p.Ticks) != 3 || p.Ticks[0].Label != "0 B" || p.Ticks[2].Label != "100.0 MiB" {
		t.Fatalf("ticks = %+v", p.Ticks)
	}
	if p.StartLabel != "Nov 20 08:00" || p.EndLabel != "Nov 20 08:10" || !strings.Contains(p.Label, "peak 90.0 MiB") {
		t.Fatalf("labels = %q %q %q", p.StartLabel, p.EndLabel, p.Label)
	}
}

func TestMakeRedisPlotLeavesDistantLimitOffScale(t *testing.T) {
	p := makeRedisPlot(redisSamples(8<<30, 10<<20, 12<<20))
	if p.HasLimit || !p.LimitOffScale || p.LimitLabel != "8.0 GiB" {
		t.Fatalf("distant limit should be noted, not drawn: %+v", p)
	}
	if p.Ticks[2].Y != p.PlotTop || p.Ticks[2].Label != humanBytes(12<<20+12<<20/10) {
		t.Fatalf("scale should follow the peak with headroom: %+v", p.Ticks)
	}
}

func TestMakeRedisPlotSingleZeroSample(t *testing.T) {
	p := makeRedisPlot(redisSamples(0, 0))
	if p.Empty || p.HasLimit || p.LimitOffScale || p.LastX != p.PlotRight || p.LastY != p.PlotBottom {
		t.Fatalf("single zero sample = %+v", p)
	}
}
