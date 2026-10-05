package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/uros678/flightprice/internal/serpapi"
)

var ctx = context.Background()

var limits = Limits{MaxStops: 1, MaxDurationMin: 14 * 60}

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(ctx, path, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func day(d int, hour int) time.Time { return time.Date(2027, 1, d, hour, 0, 0, 0, time.UTC) }

func request(origin string) serpapi.Request {
	return serpapi.Request{Origin: origin, Destinations: []string{"JFK"}, Depart: "2027-05-10",
		Return: "2027-05-24", Adults: 2, Cabin: "economy", Currency: "EUR"}
}

func offer(price int64, stops, minutes int, airline string) serpapi.Offer {
	return serpapi.Offer{PriceCents: price, Stops: stops, DurationMin: minutes, Airlines: []string{airline},
		Route: []string{"FRA", "JFK"}, FlightNumbers: []string{"XX 1"}, DepartTime: "2027-05-10 10:00", ArriveTime: "2027-05-10 13:00"}
}

// success is a run that found offers: a cheap 2-stop one (outside the
// limits), a cheap but too long one, and two acceptable ones.
func success(option, origin string, at time.Time, cheapest int64) Run {
	return Run{Option: option, Origin: origin, StartedAt: at, Source: "scheduled", Request: request(origin),
		Result: serpapi.Result{Status: serpapi.Success,
			Raw:      []byte(`{"search_metadata": {"google_flights_url": "https://www.google.com/travel/flights?tfs=` + origin + `"}}`),
			Insights: &serpapi.Insights{Level: "low", LowestCents: cheapest - 5000, TypicalLowCents: 100000, TypicalHighCents: 150000},
			Offers: []serpapi.Offer{
				offer(cheapest-5000, 2, 600, "Twostop Air"),
				offer(cheapest-3000, 1, 20*60, "Slow Air"),
				offer(cheapest+1000, 0, 520, "Direct Air"),
				offer(cheapest, 1, 700, "Good Air"),
			}}}
}

func TestSaveAndObservations(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "flightprice.db"))
	for _, r := range []Run{
		success("A", "FRA", day(1, 7), 120000),
		success("B", "FRA", day(1, 7), 130000),
		success("A", "MUC", day(2, 7), 115000),
		success("A", "FRA", day(3, 7), 118000),
	} {
		if _, err := s.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	obs, err := s.Observations(ctx, "A", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var prices []int64
	for _, o := range obs {
		prices = append(prices, o.PriceCents)
	}
	if !slices.Equal(prices, []int64{120000, 115000, 118000}) {
		t.Fatalf("option A prices = %v", prices)
	}
	first := obs[0]
	if first.Origin != "FRA" || !first.At.Equal(day(1, 7)) || first.Currency != "EUR" || first.Level != "low" {
		t.Errorf("first = %+v", first)
	}
	// The cheapest acceptable offer, not the 2-stop or the too long one.
	if !slices.Equal(first.Airlines, []string{"Good Air"}) || first.Stops != 1 || first.DurationMin != 700 {
		t.Errorf("tracked offer = %+v", first)
	}
	if !slices.Equal(first.Route, []string{"FRA", "JFK"}) {
		t.Errorf("route = %v", first.Route)
	}
	if first.GoogleFlightsURL != "https://www.google.com/travel/flights?tfs=FRA" {
		t.Errorf("google flights url = %q", first.GoogleFlightsURL)
	}

	fra, err := s.Observations(ctx, "A", "FRA", day(2, 0))
	if err != nil || len(fra) != 1 || fra[0].PriceCents != 118000 {
		t.Errorf("A from FRA since day 2 = %+v, %v", fra, err)
	}
}

func TestLimitsChangeTheView(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "flightprice.db"))
	if _, err := s.SaveRun(ctx, success("A", "FRA", day(1, 7), 120000)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		limits Limits
		want   int64
	}{
		{Limits{MaxStops: 0}, 121000},                      // direct only
		{Limits{MaxStops: 1}, 117000},                      // no duration limit: the slow one wins
		{Limits{MaxStops: 2, MaxDurationMin: 0}, 115000},   // 2 stops allowed
		{Limits{MaxStops: 1, MaxDurationMin: 840}, 120000}, // back to the default
	} {
		if err := s.SetLimits(ctx, tc.limits); err != nil {
			t.Fatal(err)
		}
		obs, err := s.Observations(ctx, "", "", time.Time{})
		if err != nil || len(obs) != 1 || obs[0].PriceCents != tc.want {
			t.Errorf("%+v: got %+v, %v; want %d", tc.limits, obs, err, tc.want)
		}
	}
}

func TestFailedRuns(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "flightprice.db"))
	runs := []Run{
		success("A", "FRA", day(1, 7), 120000),
		{Option: "A", Origin: "MUC", StartedAt: day(1, 7), Source: "scheduled", Request: request("MUC"),
			Result: serpapi.Result{Status: serpapi.NoResults, Raw: []byte(`{"error":"none"}`)}},
		{Option: "B", Origin: "MUC", StartedAt: day(2, 7), Source: "scheduled", Request: request("MUC"),
			Result: serpapi.Result{Status: serpapi.RateLimited, Raw: []byte(`{"error":"out"}`)},
			Err:    &serpapi.Error{Status: serpapi.RateLimited, Message: "out"}},
		// Never reached SerpApi: not billed.
		{Option: "B", Origin: "FRA", StartedAt: day(3, 7), Source: "retry", Request: request("FRA"),
			Result: serpapi.Result{Status: serpapi.NetworkError},
			Err:    &serpapi.Error{Status: serpapi.NetworkError, Message: "timeout"}},
	}
	for _, r := range runs {
		if _, err := s.SaveRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := s.CallsSince(ctx, day(1, 0)); err != nil || n != 3 {
		t.Errorf("calls since day 1 = %d, %v; want 3", n, err)
	}
	if n, err := s.CallsSince(ctx, day(2, 0)); err != nil || n != 1 {
		t.Errorf("calls since day 2 = %d, %v; want 1", n, err)
	}

	last, err := s.LastChecked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// MUC's rate-limited run on day 2 does not count as a check.
	if !last["FRA"].Equal(day(1, 7)) || !last["MUC"].Equal(day(1, 7)) || len(last) != 2 {
		t.Errorf("last checked = %v", last)
	}

	if ok, err := s.HasOffers(ctx, "A"); err != nil || !ok {
		t.Errorf("A has offers: %v, %v", ok, err)
	}
	if ok, err := s.HasOffers(ctx, "B"); err != nil || ok {
		t.Errorf("B has offers: %v, %v", ok, err)
	}

	var status, errText string
	var raw *string
	err = s.db.QueryRow("SELECT status, error, raw_json FROM runs WHERE source = 'retry'").Scan(&status, &errText, &raw)
	if err != nil || status != "NETWORK_ERROR" || errText != "NETWORK_ERROR: timeout" || raw != nil {
		t.Errorf("network error run: %s %q raw=%v, %v", status, errText, raw, err)
	}
}

func TestSaveRunNeedsFields(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "flightprice.db"))
	if _, err := s.SaveRun(ctx, Run{Option: "A", Origin: "FRA"}); err == nil {
		t.Error("run without status saved")
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flightprice.db")
	s, err := Open(ctx, path, limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveRun(ctx, success("A", "FRA", day(1, 7), 120000)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = open(t, path)
	obs, err := s.Observations(ctx, "", "", time.Time{})
	if err != nil || len(obs) != 1 {
		t.Fatalf("after reopen: %+v, %v", obs, err)
	}
	var version int
	s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != schemaVersion {
		t.Errorf("user_version = %d", version)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flightprice.db")
	s := open(t, path)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path, limits); err == nil {
		t.Error("a database from a newer version was opened")
	}
}

func TestBackup(t *testing.T) {
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "flightprice.db"))
	if _, err := s.SaveRun(ctx, success("A", "FRA", day(1, 7), 120000)); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dir, "backup")

	if last, err := LastBackup(backupDir); err != nil || !last.IsZero() {
		t.Errorf("no backups yet: %v, %v", last, err)
	}
	for d := 1; d <= 4; d++ {
		if _, err := s.Backup(ctx, backupDir, day(d*7, 3), 3); err != nil {
			t.Fatal(err)
		}
	}
	// Same day again: kept, not written twice.
	path, err := s.Backup(ctx, backupDir, day(28, 9), 3)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := backups(backupDir)
	var names []string
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	want := []string{"flightprice-2027-01-14.db", "flightprice-2027-01-21.db", "flightprice-2027-01-28.db"}
	if !slices.Equal(names, want) || filepath.Base(path) != want[2] {
		t.Errorf("backups = %v (last %s), want %v", names, path, want)
	}
	if last, err := LastBackup(backupDir); err != nil || !last.Equal(time.Date(2027, 1, 28, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last backup = %v, %v", last, err)
	}

	// The copy is a working database with the data.
	b, err := Open(ctx, path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if obs, err := b.Observations(ctx, "", "", time.Time{}); err != nil || len(obs) != 1 {
		t.Errorf("backup content: %+v, %v", obs, err)
	}

	// Other files in the folder are left alone.
	other := filepath.Join(backupDir, "notes.txt")
	os.WriteFile(other, nil, 0o644)
	s.Backup(ctx, backupDir, day(30, 3), 1)
	if _, err := os.Stat(other); errors.Is(err, os.ErrNotExist) {
		t.Error("an unrelated file was deleted")
	}
}

func TestRunLogAndOffers(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "flightprice.db"))
	okID, err := s.SaveRun(ctx, success("A", "FRA", day(1, 7), 120000))
	if err != nil {
		t.Fatal(err)
	}
	failed := Run{Option: "B", Origin: "MUC", StartedAt: day(1, 8), Source: "manual", Request: request("MUC"),
		Result: serpapi.Result{Status: serpapi.AuthError}, Err: &serpapi.Error{Status: serpapi.AuthError, Message: "bad key"}}
	if _, err := s.SaveRun(ctx, failed); err != nil {
		t.Fatal(err)
	}

	runs, err := s.RecentRuns(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if runs[0].Origin != "MUC" || runs[0].Status != "AUTH_ERROR" || runs[0].Error != "AUTH_ERROR: bad key" || runs[0].PriceCents != 0 {
		t.Errorf("newest = %+v", runs[0])
	}
	if runs[1].ID != okID || runs[1].PriceCents != 120000 || runs[1].Source != "scheduled" {
		t.Errorf("older = %+v", runs[1])
	}

	if n, err := s.CountRuns(ctx, day(1, 0), "scheduled", "probe"); err != nil || n != 1 {
		t.Errorf("scheduled runs = %d, %v", n, err)
	}
	if n, err := s.CountRuns(ctx, day(1, 0), "manual"); err != nil || n != 1 {
		t.Errorf("manual runs = %d, %v", n, err)
	}

	offers, err := s.Offers(ctx, []int64{okID})
	if err != nil || len(offers) != 4 {
		t.Fatalf("offers = %+v, %v", offers, err)
	}
	if offers[0].PriceCents != 115000 || offers[0].Stops != 2 || offers[0].Option != "A" || offers[0].Origin != "FRA" {
		t.Errorf("cheapest offer = %+v", offers[0])
	}
	if got, _ := s.Offers(ctx, nil); got != nil {
		t.Errorf("no runs: %+v", got)
	}
}

func TestCopyBeforeUpgrade(t *testing.T) {
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "flightprice.db"))
	if _, err := s.SaveRun(ctx, success("A", "FRA", day(1, 7), 120000)); err != nil {
		t.Fatal(err)
	}
	path, err := s.copyBeforeUpgrade(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "backup", "flightprice-schema-1.db") {
		t.Errorf("path = %s", path)
	}
	c, err := Open(ctx, path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if obs, err := c.Observations(ctx, "", "", time.Time{}); err != nil || len(obs) != 1 {
		t.Errorf("copy: %+v, %v", obs, err)
	}
	// Not a weekly backup: never pruned.
	if files, _ := backups(filepath.Join(dir, "backup")); len(files) != 0 {
		t.Errorf("listed as weekly backups: %v", files)
	}
}
