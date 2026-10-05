package plan

import (
	"slices"
	"testing"
	"time"

	"github.com/uros678/flightprice/internal/config"
)

func testConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse(`
[passengers]
adults = 2
[route]
origins = ["FRA", "MUC", "BER", "HAM", "DUS"]
destination = ["JFK"]
[[option]]
name = "A"
depart = 2027-05-10
return = 2027-05-24
[[option]]
name = "B"
depart = 2027-06-07
return = 2027-06-21
[alert]
ntfy_topic = "t"
` + extra)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var berlin, _ = time.LoadLocation("Europe/Berlin")

func at(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 7, 0, 0, 0, berlin) }

func TestCycle(t *testing.T) {
	for _, tc := range []struct {
		now        time.Time
		reset      int
		start, end string
	}{
		{at(2026, 10, 5), 5, "2026-10-05", "2026-11-05"},
		{at(2026, 10, 4), 5, "2026-09-05", "2026-10-05"},
		{at(2026, 12, 20), 5, "2026-12-05", "2027-01-05"},
		{at(2027, 1, 2), 5, "2026-12-05", "2027-01-05"},
		{at(2027, 2, 28), 31, "2027-02-28", "2027-03-31"},
		{at(2027, 3, 15), 31, "2027-02-28", "2027-03-31"},
		{at(2027, 3, 31), 31, "2027-03-31", "2027-04-30"},
	} {
		start, end := Cycle(tc.now, tc.reset)
		if start.Format(config.DateLayout) != tc.start || end.Format(config.DateLayout) != tc.end {
			t.Errorf("%s reset %d: got %s - %s, want %s - %s", tc.now.Format(config.DateLayout), tc.reset,
				start.Format(config.DateLayout), end.Format(config.DateLayout), tc.start, tc.end)
		}
	}
}

// simulate runs every day of the cycle that starts on first, with every
// planned search succeeding, and returns per-day calls and per-origin
// check days.
func simulate(t *testing.T, cfg *config.Config, first time.Time) (perDay []int, checks map[string][]int) {
	t.Helper()
	start, end := Cycle(first, cfg.Budget.ResetDay)
	onSale := map[string]bool{"A": true, "B": true}
	last := map[string]time.Time{}
	checks = map[string][]int{}
	used := 0
	for day, now := 0, start.Add(7*time.Hour); now.Before(end); day, now = day+1, now.AddDate(0, 0, 1) {
		p := Make(cfg, now, used, last, onSale)
		if !p.CycleStart.Equal(start) {
			t.Fatalf("day %d: cycle start %v, want %v", day, p.CycleStart, start)
		}
		perDay = append(perDay, len(p.Searches))
		used += len(p.Searches)
		for i, origin := range p.Checks() {
			last[origin] = now.Add(time.Duration(i) * time.Minute)
			checks[origin] = append(checks[origin], day)
		}
	}
	if used > cfg.Budget.MonthlyCalls {
		t.Errorf("used %d of %d calls", used, cfg.Budget.MonthlyCalls)
	}
	return perDay, checks
}

func maxGap(days []int, total int) int {
	gap, prev := 0, -1
	for _, d := range append(days, total) {
		gap = max(gap, d-prev-1)
		prev = d
	}
	return gap
}

func TestThirtyDayCycle(t *testing.T) {
	cfg := testConfig(t, "[budget]\nreset_day = 5\n")
	perDay, checks := simulate(t, cfg, at(2027, 4, 5)) // 5.4.-5.5.: 30 days

	// (250 - 10) / 30 = 8 calls a day: 4 airports with both options.
	for day, n := range perDay[:23] {
		if n != 8 {
			t.Errorf("day %d: %d calls, want 8", day, n)
		}
	}
	// The reserve is released over the last week, so the whole budget is
	// used.
	total := 0
	for _, n := range perDay {
		total += n
	}
	if total != 250 {
		t.Errorf("used %d calls, want 250 (per day %v)", total, perDay)
	}
	for origin, days := range checks {
		if g := maxGap(days, len(perDay)); g > 1 {
			t.Errorf("%s: %d days in a row without a check", origin, g)
		}
		if len(days) < 24 {
			t.Errorf("%s checked on %d days", origin, len(days))
		}
	}
}

func TestShortAndLongCycles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first time.Time
		days  int
	}{
		{"31 days", at(2027, 7, 1), 31},
		{"28 days", at(2027, 2, 1), 28},
	} {
		cfg := testConfig(t, "")
		perDay, checks := simulate(t, cfg, tc.first)
		if len(perDay) != tc.days {
			t.Fatalf("%s: %d days simulated", tc.name, len(perDay))
		}
		total := 0
		for _, n := range perDay {
			total += n
			if n%2 != 0 {
				t.Errorf("%s: %d calls on a day, not whole airport checks", tc.name, n)
			}
		}
		// At most one airport check's worth of the budget is left over.
		if total < cfg.Budget.MonthlyCalls-1 {
			t.Errorf("%s: only %d of %d calls used", tc.name, total, cfg.Budget.MonthlyCalls)
		}
		for origin, days := range checks {
			if g := maxGap(days, len(perDay)); g > 2 {
				t.Errorf("%s: %s went %d days without a check", tc.name, origin, g)
			}
		}
	}
}

func TestWeights(t *testing.T) {
	cfg := testConfig(t, "[weights]\nFRA = 3\n")
	_, checks := simulate(t, cfg, at(2027, 4, 1))
	if len(checks["FRA"]) < 29 {
		t.Errorf("FRA (weight 3) checked on %d of 30 days", len(checks["FRA"]))
	}
	for _, o := range []string{"MUC", "BER", "HAM", "DUS"} {
		if len(checks[o]) >= len(checks["FRA"]) {
			t.Errorf("%s checked as often as FRA: %d", o, len(checks[o]))
		}
	}
}

func TestSalesWindow(t *testing.T) {
	cfg := testConfig(t, "") // A returns 2027-05-24, B 2027-06-21; 331 days ahead
	opensA := time.Date(2026, 6, 27, 0, 0, 0, 0, time.UTC)
	if got := OpensOn(cfg.Options[0], 331); !got.Equal(opensA) {
		t.Fatalf("A opens on %v, want %v", got, opensA)
	}

	// Before either option opens: nothing to do.
	p := Make(cfg, at(2026, 6, 26), 0, nil, nil)
	if len(p.Searches) != 0 || len(p.Waiting) != 2 || !p.Waiting[0].OpensOn.Equal(opensA) {
		t.Errorf("before: %+v", p)
	}

	// A is in the window but has never had offers: one probe from the first
	// origin. B still waits.
	p = Make(cfg, at(2026, 6, 27), 0, nil, nil)
	want := []Search{{Origin: "FRA", Option: "A", Probe: true}}
	if !slices.Equal(p.Searches, want) || len(p.Waiting) != 1 || p.Waiting[0].Option != "B" {
		t.Errorf("A probing: %+v", p)
	}
	if len(p.Checks()) != 0 {
		t.Errorf("a probe counted as a check: %v", p.Checks())
	}

	// A on sale: the full share of the day goes to it alone.
	p = Make(cfg, at(2026, 6, 28), 0, nil, map[string]bool{"A": true})
	if len(p.Searches) != 5 || len(p.Checks()) != 5 {
		t.Errorf("A on sale: %d searches, %d calls, checks %v", len(p.Searches), p.Calls, p.Checks())
	}

	// B probing while A is on sale: the probe is kept out of A's checks.
	p = Make(cfg, at(2026, 7, 25), 0, nil, map[string]bool{"A": true})
	probes := slices.IndexFunc(p.Searches, func(s Search) bool { return s.Probe })
	if probes < 0 || p.Searches[probes] != (Search{Origin: "FRA", Option: "B", Probe: true}) || len(p.Searches) != 6 {
		t.Errorf("B probing next to A: %+v", p.Searches)
	}
}

func TestProbeOriginFollowsWeight(t *testing.T) {
	cfg := testConfig(t, "[weights]\nBER = 2\n")
	p := Make(cfg, at(2026, 6, 27), 0, nil, nil)
	if len(p.Searches) != 1 || p.Searches[0].Origin != "BER" {
		t.Errorf("probe = %+v", p.Searches)
	}
}

func TestBudgetUsedUp(t *testing.T) {
	cfg := testConfig(t, "[budget]\nreset_day = 5\n")
	onSale := map[string]bool{"A": true, "B": true}
	// Only the reserve is left with 10 days to go: nothing scheduled.
	p := Make(cfg, at(2027, 4, 25), 240, nil, onSale)
	if p.Calls != 0 || len(p.Searches) != 0 {
		t.Errorf("reserve only: %d calls, %d searches", p.Calls, len(p.Searches))
	}
	// Over the budget (e.g. many manual searches): still nothing.
	p = Make(cfg, at(2027, 4, 25), 260, nil, onSale)
	if p.Calls != 0 || len(p.Searches) != 0 {
		t.Errorf("over budget: %d calls", p.Calls)
	}
	// 1 call left on a day: not enough for a whole airport check.
	p = Make(cfg, at(2027, 5, 4), 249, nil, onSale)
	if p.Share != 1 || p.Calls != 0 || len(p.Searches) != 0 {
		t.Errorf("1 call: share %v, %d calls, %+v", p.Share, p.Calls, p.Searches)
	}
}

func TestNeverCheckedFirst(t *testing.T) {
	cfg := testConfig(t, "")
	now := at(2027, 4, 10)
	last := map[string]time.Time{"FRA": now.Add(-48 * time.Hour), "MUC": now.Add(-24 * time.Hour),
		"HAM": now.Add(-72 * time.Hour), "DUS": now.Add(-24 * time.Hour)}
	got := ranked(cfg, now, last)
	if want := []string{"BER", "HAM", "FRA", "MUC", "DUS"}; !slices.Equal(got, want) {
		t.Errorf("ranked = %v, want %v", got, want)
	}
}
