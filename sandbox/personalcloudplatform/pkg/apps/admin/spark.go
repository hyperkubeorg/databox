// spark.go — inline SVG sparklines rendered from stored §11.3 samples.
// No JS, no chart library: a polyline whose points are computed here.
// Safe as template.HTML because every byte is printf'd from numbers.
package admin

import (
	"fmt"
	"html/template"
	"strings"
)

// sparkline renders vals (OLDEST first) as a small polyline; flat-zero
// series render a baseline so "nothing happening" still looks alive.
func sparkline(vals []int64) template.HTML {
	const w, h = 160, 34
	if len(vals) == 0 {
		return ""
	}
	maxV := int64(1)
	for _, v := range vals {
		if v > maxV {
			maxV = v
		}
	}
	var pts strings.Builder
	step := float64(w)
	if len(vals) > 1 {
		step = float64(w) / float64(len(vals)-1)
	}
	for i, v := range vals {
		x := float64(i) * step
		y := float64(h-3) - float64(v)*float64(h-6)/float64(maxV)
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" width="%d" height="%d" viewBox="0 0 %d %d" preserveAspectRatio="none" role="img" aria-label="history"><polyline points="%s" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>`,
		w, h, w, h, pts.String()))
}

// SparkSet is one labeled sparkline (worker pages render several).
type SparkSet struct {
	Label  string
	Latest int64
	SVG    template.HTML
}

// sparkFrom builds a SparkSet from newest-first samples via pick.
func sparkFrom(label string, newestFirst int, pick func(i int) int64) SparkSet {
	vals := make([]int64, newestFirst)
	for i := 0; i < newestFirst; i++ {
		// Reverse: sample 0 is newest, sparkline wants oldest first.
		vals[newestFirst-1-i] = pick(i)
	}
	s := SparkSet{Label: label, SVG: sparkline(vals)}
	if len(vals) > 0 {
		s.Latest = vals[len(vals)-1]
	}
	return s
}
