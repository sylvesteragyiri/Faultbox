package dashboard

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// BarChart is a server-rendered daily column chart (inline SVG), stacked
// when it has two series. It follows the dataviz rules: columns at most
// 24px wide with a 4px rounded top and a square baseline, a 2px surface
// gap between stacked segments, one y-axis with recessive gridlines, a
// legend for two series, a hover tooltip, and a table view.
type BarChart struct {
	Label       string // accessible name
	Width       float64
	Height      float64
	PlotLeft    float64
	PlotTop     float64
	PlotBottom  float64
	PlotRight   float64
	PlotH       float64
	TickX       float64
	Bars        []Bar
	Ticks       []Tick
	XLabels     []XLabel
	Series      []Series
	Rows        []TableRow
	Empty       bool
	TotalLatest int64
}

// Series is one legend entry.
type Series struct {
	Name  string
	Class string // CSS class carrying the series color
}

// Bar is one day's column.
type Bar struct {
	HitX, HitW float64
	Segments   []Segment
	Tip        string
}

// Segment is one stacked part of a column, drawn as an SVG path.
type Segment struct {
	Path  string
	Class string
}

// Tick is a horizontal gridline with its value label.
type Tick struct {
	Y     float64
	Label string
}

// XLabel is a date label under the axis.
type XLabel struct {
	X     float64
	Label string
}

// TableRow is one row of the table view.
type TableRow struct {
	Day    string
	Values []int64
}

const (
	chartW      = 720.0
	chartH      = 200.0
	padLeft     = 40.0
	padRight    = 8.0
	padTop      = 10.0
	padBottom   = 24.0
	maxBarWidth = 24.0
	segmentGap  = 2.0
	topRadius   = 4.0
)

// newBarChart builds a chart from daily counts. With names of length 2 the
// columns stack Errors (bottom) and QueueFailures (top).
func newBarChart(label string, days []store.DayCount, names []string) BarChart {
	c := BarChart{
		Label: label, Width: chartW, Height: chartH,
		PlotLeft: padLeft, PlotTop: padTop, PlotBottom: chartH - padBottom, PlotRight: chartW - padRight,
	}
	classes := []string{"series-1", "series-2"}
	for i, n := range names {
		c.Series = append(c.Series, Series{Name: n, Class: classes[i]})
	}

	var maxTotal int64
	for _, d := range days {
		maxTotal = max(maxTotal, d.Errors+d.QueueFailures)
	}
	c.Empty = maxTotal == 0
	top, step := niceScale(maxTotal)
	plotH := c.PlotBottom - c.PlotTop
	c.PlotH, c.TickX = plotH, padLeft-6
	scale := func(v int64) float64 { return float64(v) / float64(top) * plotH }

	for v := int64(0); v <= top; v += step {
		c.Ticks = append(c.Ticks, Tick{Y: round1(c.PlotBottom - scale(v)), Label: compact(v)})
	}

	slot := (c.PlotRight - c.PlotLeft) / float64(max(len(days), 1))
	barW := math.Min(maxBarWidth, math.Floor(slot*0.62))
	for i, d := range days {
		x := c.PlotLeft + float64(i)*slot + (slot-barW)/2
		values := []int64{d.Errors}
		if len(names) > 1 {
			values = append(values, d.QueueFailures)
		}

		bar := Bar{HitX: round1(c.PlotLeft + float64(i)*slot), HitW: round1(slot)}
		// Find the topmost non-zero segment: only it gets the rounded end.
		topIdx := -1
		for j := len(values) - 1; j >= 0; j-- {
			if values[j] > 0 {
				topIdx = j
				break
			}
		}
		base := c.PlotBottom
		for j, v := range values {
			if v == 0 {
				continue
			}
			h := scale(v)
			y := base - h
			segH := h
			if j > 0 {
				segH -= segmentGap // surface gap between stacked parts
			}
			if segH < 1 {
				segH = 1
			}
			// The segment spans y..y+segH; for upper segments the missing
			// 2px at the bottom is the surface gap above the part below.
			bar.Segments = append(bar.Segments, Segment{
				Path:  columnPath(x, y, barW, segH, j == topIdx),
				Class: classes[j],
			})
			base = y
		}

		tip := []string{d.Day.Format("Mon 2 Jan")}
		row := TableRow{Day: d.Day.Format("2006-01-02")}
		for j, v := range values {
			name := "Events"
			if j < len(names) {
				name = names[j]
			}
			tip = append(tip, fmt.Sprintf("%s: %d", name, v))
			row.Values = append(row.Values, v)
		}
		bar.Tip = strings.Join(tip, " · ")
		c.Bars = append(c.Bars, bar)
		c.Rows = append(c.Rows, row)

		// Weekly labels counted back from the latest day, so they never crowd.
		if (len(days)-1-i)%7 == 0 {
			c.XLabels = append(c.XLabels, XLabel{X: round1(x + barW/2), Label: d.Day.Format("2 Jan")})
		}
	}
	if n := len(days); n > 0 {
		c.TotalLatest = days[n-1].Errors + days[n-1].QueueFailures
	}
	return c
}

// columnPath draws a column of height h whose bottom sits at y+h. A rounded
// column has 4px top corners and square bottom corners.
func columnPath(x, y, w, h float64, rounded bool) string {
	f := func(v float64) string { return strconv.FormatFloat(round1(v), 'f', -1, 64) }
	if !rounded || h < topRadius*2 || w < topRadius*2 {
		return fmt.Sprintf("M%s %sh%sv%sh-%sz", f(x), f(y), f(w), f(h), f(w))
	}
	r := topRadius
	return fmt.Sprintf("M%s %sv-%sq0 -%s %s -%sh%sq%s 0 %s %sv%sz",
		f(x), f(y+h), f(h-r), f(r), f(r), f(r), f(w-2*r), f(r), f(r), f(r), f(h-r))
}

// niceScale picks an axis maximum and step (1, 2 or 5 × 10^n) with 3–5 ticks.
func niceScale(maxVal int64) (top, step int64) {
	if maxVal <= 0 {
		return 4, 1
	}
	raw := float64(maxVal) / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if s := m * mag; s >= raw {
			step = int64(math.Max(1, math.Ceil(s)))
			break
		}
	}
	top = ((maxVal + step - 1) / step) * step
	return top, step
}

// compact formats 1284 as 1.3K, like the stat tiles.
func compact(v int64) string {
	switch {
	case v >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(v)/1e6, 'f', 1, 64), ".0") + "M"
	case v >= 10_000:
		return strconv.FormatInt(v/1000, 10) + "K"
	case v >= 1_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(v)/1e3, 'f', 1, 64), ".0") + "K"
	default:
		return strconv.FormatInt(v, 10)
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// sparkline returns SVG polyline points for a small trend, 100×20 units.
func sparkline(values []int64) string {
	if len(values) == 0 {
		return ""
	}
	var peak int64 = 1
	for _, v := range values {
		peak = max(peak, v)
	}
	pts := make([]string, len(values))
	for i, v := range values {
		x := 0.0
		if len(values) > 1 {
			x = float64(i) / float64(len(values)-1) * 100
		}
		y := 19 - float64(v)/float64(peak)*17
		pts[i] = strconv.FormatFloat(round1(x), 'f', -1, 64) + "," + strconv.FormatFloat(round1(y), 'f', -1, 64)
	}
	return strings.Join(pts, " ")
}
