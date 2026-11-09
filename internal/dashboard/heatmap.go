package dashboard

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/sylvesteragyiri/faultbox/internal/store"
)

// SparkBar is one column of a small bar sparkline (100×24 viewBox units).
type SparkBar struct {
	X, Y, W, H float64
	Peak       bool // the tallest bar, drawn in the strong tone
}

// sparkBars turns daily counts into columns for an issue card. Each column
// spans its slot minus a gap, with a 1-unit floor so quiet days still show.
func sparkBars(values []int64) []SparkBar {
	if len(values) == 0 {
		return nil
	}
	var peak int64
	for _, v := range values {
		peak = max(peak, v)
	}
	slot := 100.0 / float64(len(values))
	w := round1(slot * 0.72)
	out := make([]SparkBar, len(values))
	for i, v := range values {
		h := 1.5
		if peak > 0 && v > 0 {
			h = math.Max(3, float64(v)/float64(peak)*24)
		}
		out[i] = SparkBar{
			X: round1(float64(i)*slot + (slot-w)/2), Y: round1(24 - h), W: w, H: round1(h),
			Peak: peak > 0 && v == peak,
		}
	}
	return out
}

// Heatmap is a weekday × week activity grid (inline SVG), binned on a
// single-hue sequential ramp: level 0 is "no events", 1–5 are quintiles of
// the busiest day. Cells reuse the bar chart's hover tooltip and keyboard
// focus, and a table view carries the exact numbers.
type Heatmap struct {
	Label     string
	Width     float64
	Height    float64
	Cells     []HeatCell
	RowLabels []XLabel // Y in X field: weekday labels down the left
	ColLabels []XLabel // month labels along the top
	Weeks     []HeatWeek
	Empty     bool
	Total     int64
	CellSize  float64
	WeekCount int
}

// HeatCell is one day.
type HeatCell struct {
	X, Y   float64
	Level  int
	Tip    string
	Future bool
}

// HeatWeek is one row of the table view.
type HeatWeek struct {
	Start  string
	Counts []string // Monday..Sunday; "" for days after today
}

const (
	heatCell  = 20.0
	heatGap   = 4.0
	heatLeft  = 34.0
	heatTop   = 18.0
	heatLevel = 5
)

var weekdayNames = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// newHeatmap lays out the last `weeks` weeks of days, Monday first, ending
// with the week that contains the latest day in days.
func newHeatmap(days []store.DayCount, weeks int) Heatmap {
	h := Heatmap{Label: fmt.Sprintf("Events per day, last %d weeks", weeks), CellSize: heatCell, WeekCount: weeks}
	if len(days) == 0 {
		h.Empty = true
		return h
	}
	counts := make(map[string]int64, len(days))
	var peak int64
	for _, d := range days {
		n := d.Errors + d.QueueFailures
		counts[d.Day.Format(time.DateOnly)] = n
		peak = max(peak, n)
		h.Total += n
	}
	h.Empty = h.Total == 0

	last := days[len(days)-1].Day
	offset := (int(last.Weekday()) + 6) % 7 // days since Monday
	start := last.AddDate(0, 0, -offset-(weeks-1)*7)
	h.Width = heatLeft + float64(weeks)*(heatCell+heatGap) - heatGap
	h.Height = heatTop + 7*(heatCell+heatGap) - heatGap

	for r, name := range weekdayNames {
		if r%2 == 0 { // Mon, Wed, Fri, Sun keep the column readable
			h.RowLabels = append(h.RowLabels, XLabel{X: heatTop + float64(r)*(heatCell+heatGap) + heatCell/2, Label: name})
		}
	}
	prevMonth := time.Month(0)
	for w := range weeks {
		weekStart := start.AddDate(0, 0, w*7)
		x := heatLeft + float64(w)*(heatCell+heatGap)
		if weekStart.Month() != prevMonth {
			if w == 0 || weekStart.Day() <= 7 {
				h.ColLabels = append(h.ColLabels, XLabel{X: x, Label: weekStart.Format("Jan")})
			}
			prevMonth = weekStart.Month()
		}
		row := HeatWeek{Start: weekStart.Format("2 Jan")}
		for d := range 7 {
			day := weekStart.AddDate(0, 0, d)
			cell := HeatCell{X: x, Y: heatTop + float64(d)*(heatCell+heatGap)}
			if day.After(last) {
				cell.Future = true
				row.Counts = append(row.Counts, "")
			} else {
				n := counts[day.Format(time.DateOnly)]
				if n > 0 && peak > 0 {
					cell.Level = int(math.Ceil(float64(n) / float64(peak) * heatLevel))
				}
				noun := "events"
				if n == 1 {
					noun = "event"
				}
				cell.Tip = day.Format("Mon 2 Jan") + ": " + strconv.FormatInt(n, 10) + " " + noun
				row.Counts = append(row.Counts, strconv.FormatInt(n, 10))
			}
			h.Cells = append(h.Cells, cell)
		}
		h.Weeks = append(h.Weeks, row)
	}
	return h
}
