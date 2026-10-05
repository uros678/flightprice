package web

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/plan"
	"github.com/uros678/flightprice/internal/store"
)

// fresh is how old an airport's last price may be to count for "best now".
const fresh = 7 * 24 * time.Hour

type pageData struct {
	Version     string
	Passengers  string
	Route       string
	Constraints string
	Now         string
	Message     string
	Busy        bool
	Options     []optionView
	Origins     []string
	Rows        []originRow
	Budget      budgetView
	Runs        []runView
	CanSearch   bool
	SearchNote  string
}

type optionView struct {
	Name   string
	Dates  string
	HomeBy string
	State  string // on sale, probing or waiting
	OnSale bool
	Best   *priceView // cheapest of each airport's latest price
	Lowest *priceView // lowest ever
	Chart  template.HTML
	Offers []offerView
}

type priceView struct {
	Price    string
	Origin   string
	When     string
	Detail   string // stops, duration, airlines
	Level    string // Google's price level
	Change   string // against the airport's previous price
	Down, Up bool
}

type originRow struct {
	Origin  string
	Weight  int
	Checked string
	Cells   []originCell
}

type originCell struct {
	Latest string
	When   string
	Lowest string
	Best   bool
}

type budgetView struct {
	Used     int
	Monthly  int
	Renews   string
	DaysLeft int
	Today    int
	NextRun  string
	Next     string // what the next run will search
}

type runView struct {
	When, Option, Origin, Source, Status, Price, Error string
	Bad                                                bool
}

type offerView struct {
	Price, Origin, Stops, Duration, Route, Airlines, Flights, Depart string
	OK                                                               bool
}

// page collects everything the report page shows.
func (s *Server) page(ctx context.Context, msg string) (*pageData, error) {
	cfg, now := s.cfg, s.now()
	obs, err := s.store.Observations(ctx, "", "", time.Time{})
	if err != nil {
		return nil, err
	}
	p, err := s.planAt(ctx, now)
	if err != nil {
		return nil, err
	}
	d := &pageData{
		Version:     s.Version,
		Passengers:  passengers(cfg.Passengers),
		Route:       strings.Join(cfg.Route.Origins, ", ") + " → " + strings.Join(cfg.Route.Destination, ", "),
		Constraints: constraints(cfg.Constraints),
		Now:         now.Format("Mon 2 Jan 2006, 15:04"),
		Message:     msg,
		Busy:        s.Busy(),
		Origins:     cfg.Route.Origins,
	}

	// Price history per option and origin, oldest first.
	byOption := map[string]map[string][]store.Observation{}
	for _, o := range obs {
		if byOption[o.Option] == nil {
			byOption[o.Option] = map[string][]store.Observation{}
		}
		byOption[o.Option][o.Origin] = append(byOption[o.Option][o.Origin], o)
	}

	cur := cfg.Route.Currency
	for _, opt := range cfg.Options {
		v := optionView{Name: opt.Name, Dates: day(opt.Depart.Time) + " – " + day(opt.Return.Time)}
		if !opt.HomeBy.IsZero() {
			v.HomeBy = day(opt.HomeBy.Time)
		}
		hist := byOption[opt.Name]
		v.OnSale = len(hist) > 0
		switch {
		case v.OnSale:
			v.State = "on sale"
		case plan.InWindow(opt, cfg.Budget.MaxDaysAhead, now):
			v.State = "not on sale yet, checked once a day"
		default:
			v.State = "not searched before " + day(plan.OpensOn(opt, cfg.Budget.MaxDaysAhead))
		}

		var latestRuns []int64
		var best, lowest *store.Observation
		var bestPrev *store.Observation
		for _, origin := range cfg.Route.Origins {
			h := hist[origin]
			if len(h) == 0 {
				continue
			}
			last := h[len(h)-1]
			if now.Sub(last.At) <= fresh {
				latestRuns = append(latestRuns, last.RunID)
				if best == nil || last.PriceCents < best.PriceCents {
					best, bestPrev = &h[len(h)-1], nil
					if len(h) > 1 {
						bestPrev = &h[len(h)-2]
					}
				}
			}
			for i := range h {
				if lowest == nil || h[i].PriceCents < lowest.PriceCents {
					lowest = &h[i]
				}
			}
		}
		if best != nil {
			v.Best = s.priceView(*best, now, cur)
			if bestPrev != nil {
				diff := best.PriceCents - bestPrev.PriceCents
				switch {
				case diff < 0:
					v.Best.Change, v.Best.Down = "↓ "+money(-diff, cur)+" since "+ago(bestPrev.At, now), true
				case diff > 0:
					v.Best.Change, v.Best.Up = "↑ "+money(diff, cur)+" since "+ago(bestPrev.At, now), true
				default:
					v.Best.Change = "unchanged since " + ago(bestPrev.At, now)
				}
			}
		}
		if lowest != nil {
			v.Lowest = s.priceView(*lowest, now, cur)
		}
		v.Chart = priceChart(hist, cfg.Route.Origins, cfg.Alert.TotalBelowCents(), cur, s.cfg.Schedule.Location())

		offers, err := s.store.Offers(ctx, latestRuns)
		if err != nil {
			return nil, err
		}
		maxMin := cfg.Constraints.MaxDurationMin()
		for _, o := range offers[:min(len(offers), 12)] {
			v.Offers = append(v.Offers, offerView{
				Price: money(o.PriceCents, o.Currency), Origin: o.Origin, Stops: stops(o.Stops),
				Duration: duration(o.DurationMin), Route: strings.Join(o.Route, " › "),
				Airlines: strings.Join(o.Airlines, ", "), Flights: strings.Join(o.FlightNumbers, ", "),
				Depart: o.DepartTime,
				OK:     o.Stops <= cfg.Constraints.MaxStops && (maxMin == 0 || o.DurationMin <= maxMin),
			})
		}
		d.Options = append(d.Options, v)
	}

	// One row per airport, one cell per option.
	last, err := s.store.LastChecked(ctx)
	if err != nil {
		return nil, err
	}
	for _, origin := range cfg.Route.Origins {
		row := originRow{Origin: origin, Weight: cfg.Weight(origin), Checked: "never"}
		if t, ok := last[origin]; ok {
			row.Checked = ago(t, now)
		}
		for i, opt := range cfg.Options {
			h := byOption[opt.Name][origin]
			c := originCell{Latest: "–"}
			if len(h) > 0 {
				latest := h[len(h)-1]
				c.Latest, c.When = money(latest.PriceCents, latest.Currency), ago(latest.At, now)
				low := slices.MinFunc(h, func(a, b store.Observation) int { return cmp.Compare(a.PriceCents, b.PriceCents) })
				c.Lowest = money(low.PriceCents, low.Currency)
				c.Best = d.Options[i].Best != nil && d.Options[i].Best.Origin == origin
			}
			row.Cells = append(row.Cells, c)
		}
		d.Rows = append(d.Rows, row)
	}

	// Budget and what comes next.
	today, err := s.store.CountRuns(ctx, p.Day, SourceScheduled, SourceProbe, SourceManual, SourceRetry)
	if err != nil {
		return nil, err
	}
	next := s.nextRun(now)
	np, err := s.planAt(ctx, next)
	if err != nil {
		return nil, err
	}
	d.Budget = budgetView{
		Used: p.Used, Monthly: cfg.Budget.MonthlyCalls, Renews: day(p.CycleEnd), DaysLeft: p.DaysLeft,
		Today: today, NextRun: next.Format("Mon 2 Jan, 15:04"), Next: describe(np),
	}

	runs, err := s.store.RecentRuns(ctx, 30)
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		rv := runView{When: r.At.In(now.Location()).Format("2 Jan 15:04"), Option: r.Option, Origin: r.Origin,
			Source: r.Source, Status: strings.ToLower(strings.ReplaceAll(r.Status, "_", " ")), Error: r.Error,
			Bad: r.Status != "SUCCESS" && r.Status != "NO_RESULTS"}
		if r.PriceCents > 0 {
			rv.Price = money(r.PriceCents, cur)
		}
		d.Runs = append(d.Runs, rv)
	}

	switch searchable := s.searchable(); {
	case len(searchable) == 0:
		d.SearchNote = "No date option is on sale yet."
	case p.Used+len(searchable) > cfg.Budget.MonthlyCalls:
		d.SearchNote = "The monthly budget is used up."
	default:
		d.CanSearch = true
		d.SearchNote = fmt.Sprintf("Checks options %s from one airport: %d searches from the budget.",
			strings.Join(searchable, " and "), len(searchable))
	}
	return d, nil
}

func (s *Server) priceView(o store.Observation, now time.Time, cur string) *priceView {
	return &priceView{
		Price:  money(o.PriceCents, o.Currency),
		Origin: o.Origin,
		When:   ago(o.At, now),
		Detail: stops(o.Stops) + " · " + duration(o.DurationMin) + " · " + strings.Join(o.Airlines, ", "),
		Level:  o.Level,
	}
}

// describe says what a plan will search, for the "next run" line.
func describe(p plan.Plan) string {
	var parts []string
	if checks := p.Checks(); len(checks) > 0 {
		parts = append(parts, strings.Join(checks, ", "))
	}
	for _, s := range p.Searches {
		if s.Probe {
			parts = append(parts, "a check whether option "+s.Option+" is on sale ("+s.Origin+")")
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, "; ") + fmt.Sprintf(" (%d searches)", p.Calls)
}

func passengers(p config.Passengers) string {
	s := plural(p.Adults, "adult")
	if p.Children > 0 {
		s += " + " + plural(p.Children, "child")
	}
	return s
}

func constraints(c config.Constraints) string {
	s := "max " + plural(c.MaxStops, "stop")
	if c.MaxStops == 0 {
		s = "direct flights only"
	}
	if c.MaxDurationH > 0 {
		s += fmt.Sprintf(", max %d h outbound", c.MaxDurationH)
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if word == "child" {
		return strconv.Itoa(n) + " children"
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func stops(n int) string {
	if n == 0 {
		return "direct"
	}
	return plural(n, "stop")
}

func duration(min int) string { return fmt.Sprintf("%dh%02d", min/60, min%60) }

func day(t time.Time) string { return t.Format("Mon 2 Jan 2006") }

// ago is how long ago t was, in calendar days of now's zone.
func ago(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	days := int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
	switch days {
	case 0:
		return "today " + t.Format("15:04")
	case 1:
		return "yesterday"
	default:
		return strconv.Itoa(days) + " days ago"
	}
}

// money is a price in whole units with thousands separators, e.g. €3,163.
func money(cents int64, currency string) string {
	units := (cents + 50) / 100
	digits := strconv.FormatInt(units, 10)
	var b strings.Builder
	for i, c := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	switch currency {
	case "EUR":
		return "€" + b.String()
	case "USD":
		return "$" + b.String()
	case "GBP":
		return "£" + b.String()
	}
	return currency + " " + b.String()
}
