package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"github.com/uros678/flightprice/internal/store"
)

// palette colours the origins in config order; it stays readable for most
// kinds of colour blindness.
var palette = []string{"#4e79a7", "#f28e2b", "#e15759", "#59a14f", "#b07aa1", "#76b7b2", "#edc948", "#9c755f"}

// Chart geometry, in SVG units (the chart scales to the page width).
const (
	chartW, chartH                     = 760, 260
	padLeft, padRight, padTop, padDown = 64, 16, 28, 30
)

// priceChart draws the price history of one option: a line per origin, the
// alert threshold as a dashed line. Everything written into the SVG is
// numbers, dates or validated airport codes.
func priceChart(hist map[string][]store.Observation, origins []string, threshold int64, currency string, loc *time.Location) template.HTML {
	var first, last time.Time
	lo, hi := int64(math.MaxInt64), int64(0)
	points := 0
	for _, h := range hist {
		for _, o := range h {
			if first.IsZero() || o.At.Before(first) {
				first = o.At
			}
			if o.At.After(last) {
				last = o.At
			}
			lo, hi = min(lo, o.PriceCents), max(hi, o.PriceCents)
			points++
		}
	}
	if points < 2 {
		return ""
	}
	if threshold > 0 && threshold > lo/2 && threshold < hi*2 {
		lo, hi = min(lo, threshold), max(hi, threshold)
	}
	// Some room above and below, and at least a day on the time axis.
	pad := max((hi-lo)/10, 1000)
	lo, hi = lo-pad, hi+pad
	if last.Sub(first) < 24*time.Hour {
		first = last.Add(-24 * time.Hour)
	}

	plotW, plotH := float64(chartW-padLeft-padRight), float64(chartH-padTop-padDown)
	x := func(t time.Time) float64 {
		return padLeft + plotW*float64(t.Sub(first))/float64(last.Sub(first))
	}
	y := func(c int64) float64 {
		return padTop + plotH*(1-float64(c-lo)/float64(hi-lo))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="Price history">`, chartW, chartH)

	// Horizontal grid with prices.
	for i := 0; i <= 4; i++ {
		c := lo + (hi-lo)*int64(i)/4
		yy := y(c)
		fmt.Fprintf(&b, `<line class="grid" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/>`, padLeft, chartW-padRight, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%d" y="%.1f" text-anchor="end">%s</text>`, padLeft-6, yy+4, money(c, currency))
	}
	// Dates at the start, middle and end.
	for i, t := range []time.Time{first, first.Add(last.Sub(first) / 2), last} {
		anchor := [...]string{"start", "middle", "end"}[i]
		fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="%d" text-anchor="%s">%s</text>`, x(t), chartH-8, anchor, t.In(loc).Format("2 Jan"))
	}
	if threshold > 0 && threshold >= lo && threshold <= hi {
		yy := y(threshold)
		fmt.Fprintf(&b, `<line class="threshold" x1="%d" x2="%d" y1="%.1f" y2="%.1f"/>`, padLeft, chartW-padRight, yy, yy)
		fmt.Fprintf(&b, `<text class="axis" x="%d" y="%.1f" text-anchor="end">alert</text>`, chartW-padRight, yy-4)
	}

	// One line per origin, with a dot per price; a legend on top.
	lx := float64(padLeft)
	for i, origin := range origins {
		h := hist[origin]
		color := palette[i%len(palette)]
		if len(h) == 0 {
			continue
		}
		var pts []string
		for _, o := range h {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(o.At), y(o.PriceCents)))
		}
		if len(pts) > 1 {
			fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`, color, strings.Join(pts, " "))
		}
		for j, o := range h {
			fmt.Fprintf(&b, `<circle cx="%s" r="3" fill="%s"><title>%s %s: %s</title></circle>`,
				strings.Replace(pts[j], ",", `" cy="`, 1), color, origin, o.At.In(loc).Format("2 Jan 15:04"), money(o.PriceCents, o.Currency))
		}
		fmt.Fprintf(&b, `<rect x="%.0f" y="6" width="10" height="10" fill="%s"/><text class="axis" x="%.0f" y="15">%s</text>`, lx, color, lx+14, origin)
		lx += 56
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
