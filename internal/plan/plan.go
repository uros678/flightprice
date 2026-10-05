// Package plan decides which searches run today. It keeps no state: the
// caller passes in what the store knows, and the same input always gives
// the same plan, so a whole quota cycle can be tested day by day.
//
// The rules (design section 11):
//   - The calls left in the quota cycle, minus a reserve for manual searches
//     and retries, are spread evenly over the days left, rounded to the
//     nearest whole airport check (never past what is left). The plan is
//     made again every day, so a day that got a little more is evened out by
//     the next. Rounding down would leave calls unused (a day cannot take
//     more than every airport once); rounding up would spend a small rest of
//     the budget in the first days and leave the rest of the cycle empty. Over the cycle's last 7 days the
//     reserve is released step by step, so nothing is wasted.
//   - An option is not searched before its return date is within
//     max_days_ahead: Google has no flights that far out, and an empty search
//     still costs a call.
//   - An option in that window that has never had offers gets one probe a
//     day, from the first origin, until its fares appear.
//   - Options on sale are searched together: one airport check is one search
//     per option. The airports checked are those overdue the longest
//     (days since the last check × weight).
package plan

import (
	"cmp"
	"math"
	"slices"
	"time"

	"github.com/uros678/flightprice/internal/config"
)

// releaseDays is how many days before the renewal the reserve starts to be
// released.
const releaseDays = 7

// Search is one planned SerpApi search.
type Search struct {
	Origin string
	Option string
	Probe  bool // the option is not on sale yet; this search looks whether it is
}

// Waiting is an option that is not searched yet.
type Waiting struct {
	Option  string
	OpensOn time.Time // first day it is searched
}

// Plan is today's share of the budget and how it is spent.
type Plan struct {
	Day        time.Time // today, midnight in the schedule's zone
	CycleStart time.Time // the day the quota renewed
	CycleEnd   time.Time // the next renewal
	DaysLeft   int       // days left in the cycle, today included
	Used       int       // calls used this cycle before the plan
	Share      float64   // today's even share of the calls left
	Calls      int       // calls planned today (len(Searches))
	Searches   []Search
	Waiting    []Waiting
}

// Checks is the origins the plan checks for all options on sale, in order
// (probes not counted).
func (p Plan) Checks() []string {
	var origins []string
	for _, s := range p.Searches {
		if !s.Probe && !slices.Contains(origins, s.Origin) {
			origins = append(origins, s.Origin)
		}
	}
	return origins
}

// Cycle is the quota cycle that contains now: from the last renewal on
// resetDay to the next one. A reset day past the end of a short month
// falls on its last day.
func Cycle(now time.Time, resetDay int) (start, end time.Time) {
	loc := now.Location()
	renewal := func(year int, month time.Month) time.Time {
		last := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
		return time.Date(year, month, min(resetDay, last), 0, 0, 0, 0, loc)
	}
	y, m, d := now.Date()
	start = renewal(y, m)
	if d < start.Day() {
		start = renewal(y, m-1)
	}
	return start, renewal(start.Year(), start.Month()+1)
}

// OpensOn is the first day an option is searched: max_days_ahead days
// before its return date.
func OpensOn(o config.Option, maxDaysAhead int) time.Time {
	return o.Return.AddDate(0, 0, -maxDaysAhead)
}

// InWindow reports whether an option is searched on day (a date in any
// zone; only the calendar date counts).
func InWindow(o config.Option, maxDaysAhead int, day time.Time) bool {
	return !date(day).Before(OpensOn(o, maxDaysAhead))
}

// Make is the plan for the day of now. used is the number of calls already
// used since the cycle started (see Cycle), lastChecked the time of each
// origin's last working search and onSale which options have ever had
// offers.
func Make(cfg *config.Config, now time.Time, used int, lastChecked map[string]time.Time, onSale map[string]bool) Plan {
	b := cfg.Budget
	start, end := Cycle(now, b.ResetDay)
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	p := Plan{Day: today, CycleStart: start, CycleEnd: end, DaysLeft: days(today, end), Used: used}

	// The reserve is held in full until a week before the renewal, then a
	// seventh less each day, none on the last day.
	left := b.MonthlyCalls - used - b.Reserve*min(max(p.DaysLeft-1, 0), releaseDays)/releaseDays
	if left > 0 && p.DaysLeft > 0 {
		p.Share = float64(left) / float64(p.DaysLeft)
	}
	left = max(left, 0)

	var probes, active []string
	for _, o := range cfg.Options {
		switch {
		case !InWindow(o, b.MaxDaysAhead, today):
			p.Waiting = append(p.Waiting, Waiting{Option: o.Name, OpensOn: OpensOn(o, b.MaxDaysAhead)})
		case onSale[o.Name]:
			active = append(active, o.Name)
		default:
			probes = append(probes, o.Name)
		}
	}

	nProbes := min(len(probes), int(math.Round(p.Share)), left)
	origins := ranked(cfg, now, lastChecked)
	if len(active) > 0 {
		checks := int(math.Round((p.Share - float64(nProbes)) / float64(len(active))))
		checks = min(max(checks, 0), len(origins), (left-nProbes)/len(active))
		for _, origin := range origins[:checks] {
			for _, opt := range active {
				p.Searches = append(p.Searches, Search{Origin: origin, Option: opt})
			}
		}
	}
	// Probes go to the origin with the highest weight (the first in the
	// config on a tie): if that one has no flights, the others will not
	// have them earlier either.
	probeOrigin := cfg.Route.Origins[0]
	for _, o := range cfg.Route.Origins {
		if cfg.Weight(o) > cfg.Weight(probeOrigin) {
			probeOrigin = o
		}
	}
	for _, opt := range probes[:nProbes] {
		p.Searches = append(p.Searches, Search{Origin: probeOrigin, Option: opt, Probe: true})
	}
	p.Calls = len(p.Searches)
	return p
}

// ranked is the origins, most overdue first. Never-checked origins come
// first; ties keep the config order.
func ranked(cfg *config.Config, now time.Time, lastChecked map[string]time.Time) []string {
	score := func(origin string) float64 {
		last, ok := lastChecked[origin]
		if !ok {
			return math.Inf(1)
		}
		return now.Sub(last).Hours() / 24 * float64(cfg.Weight(origin))
	}
	origins := slices.Clone(cfg.Route.Origins)
	slices.SortStableFunc(origins, func(a, b string) int { return cmp.Compare(score(b), score(a)) })
	return origins
}

// date is the calendar date of t at midnight UTC, comparable with
// config.Date.
func date(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// days is the number of calendar days from a to b.
func days(a, b time.Time) int {
	return int(date(b).Sub(date(a)).Hours() / 24)
}
