package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/serpapi"
	"github.com/uros678/flightprice/internal/store"
)

// fake answers searches without SerpApi: offers priced by origin, or the
// error queued for an origin.
type fake struct {
	mu     sync.Mutex
	prices map[string]int64
	fail   map[string][]serpapi.Status // errors to return first, per origin
	calls  []serpapi.Request
}

func (f *fake) Search(ctx context.Context, req serpapi.Request) (serpapi.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if q := f.fail[req.Origin]; len(q) > 0 {
		f.fail[req.Origin] = q[1:]
		res := serpapi.Result{Status: q[0]}
		if q[0] != serpapi.NetworkError {
			res.Raw = []byte(`{"error":"x"}`)
		}
		return res, &serpapi.Error{Status: q[0], Message: "test failure"}
	}
	price := f.prices[req.Origin]
	return serpapi.Result{Status: serpapi.Success,
		Raw:      []byte(`{"search_metadata": {"google_flights_url": "https://www.google.com/travel/flights?tfs=` + req.Origin + req.Depart + `"}}`),
		Insights: &serpapi.Insights{Level: "typical"},
		Offers: []serpapi.Offer{
			{PriceCents: price, Stops: 1, DurationMin: 600, Airlines: []string{"Good Air"}, Route: []string{req.Origin, "XX", "JFK"},
				FlightNumbers: []string{"GA 1", "GA 2"}, DepartTime: "2027-05-10 10:00", ArriveTime: "2027-05-10 20:00"},
			{PriceCents: price - 20000, Stops: 3, DurationMin: 2000, Airlines: []string{"Long Air"}, Route: []string{req.Origin, "A", "B", "C", "JFK"},
				FlightNumbers: []string{"LA 1"}, DepartTime: "2027-05-10 06:00", ArriveTime: "2027-05-11 15:00"},
		}}, nil
}

// fakeAccount is a fake that also answers the account call, as SerpApi
// does: it counts every search it got, plus extra ones made elsewhere.
type fakeAccount struct {
	*fake
	extra   int
	renewal string
	err     error
}

func (f *fakeAccount) Account(ctx context.Context) (serpapi.Account, error) {
	if f.err != nil {
		return serpapi.Account{}, f.err
	}
	return serpapi.Account{SearchesPerMonth: 60, ThisMonthUsage: f.count() + f.extra, RenewalDate: f.renewal}, nil
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Option A (return 2027-05-24) is in the sales window from 2026-06-27,
// option B (return 2028-06-21) much later.
const testConfig = `
[passengers]
adults = 2
children = 1
[route]
origins = ["FRA", "MUC", "BER"]
destination = ["JFK"]
[constraints]
max_stops = 1
max_duration_h = 14
[[option]]
name = "A"
depart = 2027-05-10
return = 2027-05-24
[[option]]
name = "B"
depart = 2028-06-07
return = 2028-06-21
[budget]
monthly_calls = 60
reserve = 0
reset_day = 1
[alert]
total_below = 1500
ntfy_topic = "t"
[schedule]
time = "07:00"
timezone = "Europe/Berlin"
`

type testServer struct {
	*Server
	fake  *fake
	clock *time.Time
	dir   string
}

func newTestServer(t *testing.T, extra string) testServer {
	t.Helper()
	cfg, err := config.Parse(testConfig + extra)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "flightprice.db"),
		store.Limits{MaxStops: cfg.Constraints.MaxStops, MaxDurationMin: cfg.Constraints.MaxDurationMin()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fake{prices: map[string]int64{"FRA": 130000, "MUC": 125000, "BER": 140000}, fail: map[string][]serpapi.Status{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(ctx, cfg, st, f, dir, discard)
	clock := time.Date(2026, 7, 1, 7, 0, 0, 0, cfg.Schedule.Location())
	s.now = func() time.Time { return clock }
	s.pause, s.retryAfter = 0, 0
	return testServer{Server: s, fake: f, clock: &clock, dir: dir}
}

func (ts testServer) advance(d time.Duration) { *ts.clock = ts.clock.Add(d) }

func (ts testServer) runs(t *testing.T) []store.RunInfo {
	t.Helper()
	runs, err := ts.store.RecentRuns(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestDailyRuns(t *testing.T) {
	ts := newTestServer(t, "")
	ctx := context.Background()

	// Day 1: A is in the window but has never had offers: one probe.
	p, err := ts.RunPlanned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Searches) != 1 || !p.Searches[0].Probe || len(p.Waiting) != 1 {
		t.Fatalf("day 1 plan = %+v", p)
	}
	runs := ts.runs(t)
	if len(runs) != 1 || runs[0].Source != SourceProbe || runs[0].Origin != "FRA" || runs[0].Option != "A" {
		t.Fatalf("day 1 runs = %+v", runs)
	}
	req := ts.fake.calls[0]
	if req.Adults != 2 || req.Children != 1 || req.Depart != "2027-05-10" || req.Return != "2027-05-24" {
		t.Errorf("request = %+v", req)
	}
	// The probe found fares: the tracked price ignores the 3-stop offer.
	if runs[0].PriceCents != 130000 {
		t.Errorf("tracked price = %d", runs[0].PriceCents)
	}
	if ts.missedToday(ctx) {
		t.Error("today's run counted as missed after it ran")
	}

	// Day 2: A is on sale, so the airports are checked. The day's share is
	// 59 calls / 30 days, rounded up to whole checks: 2 airports.
	ts.advance(24 * time.Hour)
	if !ts.missedToday(ctx) {
		t.Error("day 2: run not due")
	}
	p, err = ts.RunPlanned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Checks(), ","); got != "MUC,BER" {
		t.Errorf("day 2 checks = %s, want the never-checked MUC and BER", got)
	}
	if n := ts.fake.count(); n != 3 {
		t.Errorf("searches so far = %d", n)
	}

	// The weekly backup was written on day 1 and not again on day 2.
	files, _ := filepath.Glob(filepath.Join(ts.dir, "backup", "*.db"))
	if len(files) != 1 {
		t.Errorf("backups = %v", files)
	}
}

func TestRetry(t *testing.T) {
	ts := newTestServer(t, "")
	ts.fake.fail["FRA"] = []serpapi.Status{serpapi.NetworkError}
	if _, err := ts.RunPlanned(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs := ts.runs(t) // newest first
	if len(runs) != 2 || runs[1].Status != "NETWORK_ERROR" || runs[0].Source != SourceRetry || runs[0].Status != "SUCCESS" {
		t.Errorf("runs = %+v", runs)
	}
	// The failed search never reached SerpApi, so only the retry counts.
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if n, _ := ts.store.CallsSince(context.Background(), start); n != 1 {
		t.Errorf("billed calls = %d", n)
	}
}

func TestBudgetStopsSearches(t *testing.T) {
	ts := newTestServer(t, "")
	ts.cfg.Budget.MonthlyCalls = 1
	ctx := context.Background()
	if _, err := ts.searchOne(ctx, "FRA", "A", SourceManual); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.searchOne(ctx, "MUC", "A", SourceManual); !errors.Is(err, errBudget) {
		t.Errorf("second search: %v, want the budget error", err)
	}
	if n := ts.fake.count(); n != 1 {
		t.Errorf("SerpApi was asked %d times", n)
	}
}

func TestManual(t *testing.T) {
	ts := newTestServer(t, "")
	if err := ts.Manual("XXX"); err == nil {
		t.Error("unknown origin accepted")
	}
	if err := ts.Manual("MUC"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, ts.Server)
	runs := ts.runs(t)
	// Only option A is in the window, so one search.
	if len(runs) != 1 || runs[0].Source != SourceManual || runs[0].Origin != "MUC" || runs[0].Option != "A" {
		t.Errorf("runs = %+v", runs)
	}

	ts.mu.Lock()
	if err := ts.Manual("MUC"); !errors.Is(err, ErrBusy) {
		t.Errorf("while busy: %v", err)
	}
	if _, err := ts.RunPlanned(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("planned run while busy: %v", err)
	}
	ts.mu.Unlock()
}

func TestNothingInWindow(t *testing.T) {
	ts := newTestServer(t, "")
	*ts.clock = time.Date(2026, 6, 1, 7, 0, 0, 0, ts.cfg.Schedule.Location())
	if err := ts.Manual("FRA"); err == nil {
		t.Error("manual search before any option is on sale")
	}
	p, err := ts.RunPlanned(context.Background())
	if err != nil || len(p.Searches) != 0 || ts.fake.count() != 0 {
		t.Errorf("plan %+v, %d searches, %v", p, ts.fake.count(), err)
	}
}

func waitIdle(t *testing.T, s *Server) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if !s.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background search did not finish")
}

func TestNextRun(t *testing.T) {
	ts := newTestServer(t, "")
	loc := ts.cfg.Schedule.Location()
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 7, 1, 6, 59, 0, 0, loc), time.Date(2026, 7, 1, 7, 0, 0, 0, loc)},
		{time.Date(2026, 7, 1, 7, 0, 0, 0, loc), time.Date(2026, 7, 2, 7, 0, 0, 0, loc)},
		{time.Date(2026, 10, 24, 22, 0, 0, 0, loc), time.Date(2026, 10, 25, 7, 0, 0, 0, loc)}, // clocks go back that night
	} {
		if got := ts.nextRun(tc.now); !got.Equal(tc.want) {
			t.Errorf("after %v: %v, want %v", tc.now, got, tc.want)
		}
	}
}

func TestPage(t *testing.T) {
	ts := newTestServer(t, "")
	ctx := context.Background()
	ts.RunPlanned(ctx)
	ts.advance(24 * time.Hour)
	ts.RunPlanned(ctx)

	h := ts.Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: msgCookie, Value: "hello"})
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Option A", "Option B", "€1,250", "best now, from <strong>MUC</strong>", "2 adults &#43; 1 child",
		"not searched before", "<svg", "hello", "3 / 60", "Search now", "probe",
		`href="https://www.google.com/travel/flights?tfs=MUC2027-05-10"`, "Return flights and booking on Google Flights",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, "Long Air") {
		t.Error("the 3-stop offer outside the limits is listed")
	}
	if !strings.Contains(body, "more outside them are not shown") {
		t.Error("the hidden offers are not counted")
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("no CSP header")
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("the page refreshes itself with no search running")
	}

	// While a search runs, the page refreshes itself.
	ts.mu.Lock()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	ts.mu.Unlock()
	if !strings.Contains(rec.Body.String(), `<meta http-equiv="refresh" content="10; url=/">`) {
		t.Error("the page does not refresh itself while a search runs")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/data.json", nil))
	var points []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &points); err != nil || len(points) != 3 {
		t.Errorf("data.json: %v, %d points", err, len(points))
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))
	if rec.Code != 404 {
		t.Errorf("unknown path = %d", rec.Code)
	}
}

func TestSearchNowButton(t *testing.T) {
	ts := newTestServer(t, "")
	h := ts.Handler()
	post := func(site string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/run", strings.NewReader(url.Values{"origin": {"ber"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("cross-site"); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d", rec.Code)
	}
	if ts.fake.count() != 0 {
		t.Error("a cross-site POST ran a search")
	}

	rec := post("same-origin")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("POST = %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	waitIdle(t, ts.Server)
	if ts.fake.count() != 1 {
		t.Errorf("searches = %d", ts.fake.count())
	}

	// The message shows on the next view only, never from the URL.
	get := func(target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", target, nil)
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	page := get("/", rec.Result().Cookies())
	if !strings.Contains(page.Body.String(), "Search started for BER") {
		t.Error("the message is not shown after the redirect")
	}
	if c := page.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Errorf("the message cookie is not cleared: %v", c)
	}
	if strings.Contains(get("/?msg=hello", nil).Body.String(), "hello") {
		t.Error("a message is taken from the URL")
	}
}

func TestEmptyPage(t *testing.T) {
	ts := newTestServer(t, "")
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No searches yet") {
		t.Errorf("empty page = %d", rec.Code)
	}
}

func TestMoney(t *testing.T) {
	for cents, want := range map[int64]string{0: "€0", 99: "€1", 316300: "€3,163", 123456789: "€1,234,568"} {
		if got := money(cents, "EUR"); got != want {
			t.Errorf("money(%d) = %q, want %q", cents, got, want)
		}
	}
	if got := money(150000, "CHF"); got != "CHF 1,500" {
		t.Errorf("CHF: %q", got)
	}
}

func TestUsageFromSerpApi(t *testing.T) {
	ts := newTestServer(t, "")
	acct := &fakeAccount{fake: ts.fake, extra: 40, renewal: "2026-08-01"}
	ts.search = acct
	ctx := context.Background()

	// SerpApi already counted 40 searches made elsewhere: 20 are left over
	// 31 days, enough for the daily probe.
	p, err := ts.RunPlanned(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Used != 40 || len(p.Searches) != 1 {
		t.Errorf("used = %d, %d searches; want SerpApi's 40 and the probe", p.Used, len(p.Searches))
	}
	if n, _ := ts.used(ctx, ts.now()); n != 41 {
		t.Errorf("after the probe: used = %d, want 41", n)
	}

	// With only 10 left, the share is too small even for a probe.
	acct.extra = 50
	ts.SyncUsage(ctx)
	if p, _ := ts.Plan(ctx); p.Used != 51 || len(p.Searches) != 0 {
		t.Errorf("10 left: used %d, searches %+v", p.Used, p.Searches)
	}

	// The hard stop uses the same count.
	acct.extra = 59
	ts.SyncUsage(ctx)
	if _, err := ts.searchOne(ctx, "FRA", "A", SourceManual); !errors.Is(err, errBudget) {
		t.Errorf("search past SerpApi's count: %v", err)
	}

	// The account call failing keeps the last known offset.
	acct.err = errors.New("down")
	ts.SyncUsage(ctx)
	if n, _ := ts.used(ctx, ts.now()); n != 60 {
		t.Errorf("after a failed sync: used = %d", n)
	}

	// A new cycle forgets the old offset.
	*ts.clock = time.Date(2026, 8, 2, 7, 0, 0, 0, ts.cfg.Schedule.Location())
	if n, _ := ts.used(ctx, ts.now()); n != 0 {
		t.Errorf("next cycle: used = %d", n)
	}
}

func TestRequestCarriesLimits(t *testing.T) {
	ts := newTestServer(t, "")
	if _, err := ts.RunPlanned(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := ts.fake.calls[0]
	if req.MaxStops == nil || *req.MaxStops != 1 || req.MaxDurationMin != 14*60 {
		t.Errorf("limits not sent: stops %v, duration %d", req.MaxStops, req.MaxDurationMin)
	}
}

func TestDepartedOptionNotSearched(t *testing.T) {
	ts := newTestServer(t, "")
	*ts.clock = time.Date(2027, 5, 10, 7, 0, 0, 0, ts.cfg.Schedule.Location()) // A departs today
	p, err := ts.RunPlanned(context.Background())
	if err != nil || len(p.Searches) != 0 || ts.fake.count() != 0 {
		t.Errorf("plan %+v, %d searches, %v", p, ts.fake.count(), err)
	}
	if err := ts.Manual("FRA"); err == nil {
		t.Error("manual search for a departed option")
	}
	rec := httptest.NewRecorder()
	ts.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), "departed, no longer searched") {
		t.Error("page does not say option A departed")
	}
}
