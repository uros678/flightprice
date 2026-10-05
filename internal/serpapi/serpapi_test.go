package serpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testKey = "test-key-0123456789abcdef"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testRequest() Request {
	return Request{
		Origin: "FRA", Destinations: []string{"JFK", "EWR"},
		Depart: "2027-05-10", Return: "2027-05-24",
		Adults: 2, Cabin: "economy", Currency: "EUR", Market: "de",
	}
}

func TestParseOffers(t *testing.T) {
	res, err := Parse(fixture(t, "search_ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Success {
		t.Fatalf("status = %s, want %s", res.Status, Success)
	}
	// 5 offers in the file, one without a price.
	if len(res.Offers) != 4 {
		t.Fatalf("got %d offers, want 4", len(res.Offers))
	}

	lh := res.Offers[0]
	if lh.PriceCents != 141200 || lh.Stops != 0 || lh.DurationMin != 525 {
		t.Errorf("first offer = %+v", lh)
	}
	if !slices.Equal(lh.Route, []string{"FRA", "JFK"}) || !slices.Equal(lh.Airlines, []string{"Lufthansa"}) {
		t.Errorf("first offer route %v, airlines %v", lh.Route, lh.Airlines)
	}
	if lh.DepartTime != "2027-05-10 10:05" || lh.ArriveTime != "2027-05-10 12:50" || lh.DepartureToken == "" {
		t.Errorf("first offer times %q-%q, token %q", lh.DepartTime, lh.ArriveTime, lh.DepartureToken)
	}

	twoStops := res.Offers[2]
	if twoStops.Stops != 2 || !slices.Equal(twoStops.Route, []string{"FRA", "LHR", "DUB", "EWR"}) {
		t.Errorf("2-stop offer = %+v", twoStops)
	}
	// Two Aer Lingus legs: the airline is listed once.
	if !slices.Equal(twoStops.Airlines, []string{"British Airways", "Aer Lingus"}) {
		t.Errorf("airlines = %v", twoStops.Airlines)
	}
	if !slices.Equal(twoStops.FlightNumbers, []string{"BA 903", "EI 155", "EI 109"}) {
		t.Errorf("flight numbers = %v", twoStops.FlightNumbers)
	}

	if got := res.Offers[3].PriceCents; got != 149050 {
		t.Errorf("decimal price = %d cents, want 149050", got)
	}

	in := res.Insights
	if in == nil {
		t.Fatal("no insights")
	}
	if in.LowestCents != 110500 || in.Level != "low" || in.TypicalLowCents != 125000 || in.TypicalHighCents != 170000 {
		t.Errorf("insights = %+v", in)
	}
	if len(in.History) != 2 || in.History[1].PriceCents != 129000 || in.History[1].At.Unix() != 1780086400 {
		t.Errorf("history = %+v", in.History)
	}
}

func TestParseEmpty(t *testing.T) {
	res, err := Parse(fixture(t, "search_empty.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != NoResults || len(res.Offers) != 0 || len(res.Raw) == 0 {
		t.Errorf("got %s with %d offers, raw %d bytes", res.Status, len(res.Offers), len(res.Raw))
	}
}

func TestParseOtherErrors(t *testing.T) {
	for name, body := range map[string]string{
		"not json":     "<html>",
		"other error":  `{"error": "Something broke"}`,
		"wrong shapes": `{"best_flights": "nope"}`,
	} {
		res, err := Parse([]byte(body))
		if err == nil || res.Status != ProviderError {
			t.Errorf("%s: got %s, %v", name, res.Status, err)
		}
	}
	// A valid answer without any priced offer counts as no results.
	res, err := Parse([]byte(`{"best_flights": [{"flights": []}]}`))
	if err != nil || res.Status != NoResults {
		t.Errorf("no priced offers: got %s, %v", res.Status, err)
	}
}

func TestSearchSendsQuery(t *testing.T) {
	body := fixture(t, "search_ok.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		want := map[string]string{
			"engine": "google_flights", "type": "1", "departure_id": "FRA", "arrival_id": "JFK,EWR",
			"outbound_date": "2027-05-10", "return_date": "2027-05-24", "adults": "2",
			"travel_class": "1", "currency": "EUR", "hl": "en", "gl": "de", "api_key": testKey,
		}
		for k, v := range want {
			if got := q.Get(k); got != v {
				t.Errorf("%s = %q, want %q", k, got, v)
			}
		}
		if q.Has("children") {
			t.Errorf("children sent although 0")
		}
		w.Write(body)
	}))
	defer srv.Close()

	c := &Client{Key: testKey, Endpoint: srv.URL}
	res, err := c.Search(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != Success || len(res.Offers) != 4 || string(res.Raw) != string(body) {
		t.Errorf("got %s, %d offers, raw %d bytes", res.Status, len(res.Offers), len(res.Raw))
	}
}

func TestSearchChildrenAndCabin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("children") != "2" || q.Get("travel_class") != "3" {
			t.Errorf("children %q, travel_class %q", q.Get("children"), q.Get("travel_class"))
		}
		w.Write(fixture(t, "search_empty.json"))
	}))
	defer srv.Close()

	req := testRequest()
	req.Children, req.Cabin = 2, "business"
	res, err := (&Client{Key: testKey, Endpoint: srv.URL}).Search(context.Background(), req)
	if err != nil || res.Status != NoResults {
		t.Errorf("got %s, %v", res.Status, err)
	}
}

func TestSearchHTTPErrors(t *testing.T) {
	auth := string(fixture(t, "error_auth.json"))
	for _, tc := range []struct {
		code int
		body string
		want Status
		msg  string
	}{
		{401, auth, AuthError, "Invalid API key"},
		{429, `{"error": "Your account has run out of searches."}`, RateLimited, "run out of searches"},
		{400, `{"error": "Missing outbound_date"}`, InvalidRequest, "Missing outbound_date"},
		{503, `upstream down`, ProviderError, "503"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.code)
			w.Write([]byte(tc.body))
		}))
		res, err := (&Client{Key: testKey, Endpoint: srv.URL}).Search(context.Background(), testRequest())
		srv.Close()

		var se *Error
		if !errors.As(err, &se) || se.Status != tc.want || res.Status != tc.want {
			t.Errorf("HTTP %d: got %v (result %s), want %s", tc.code, err, res.Status, tc.want)
			continue
		}
		if !strings.Contains(se.Message, tc.msg) {
			t.Errorf("HTTP %d: message %q does not contain %q", tc.code, se.Message, tc.msg)
		}
		if string(res.Raw) != tc.body {
			t.Errorf("HTTP %d: raw body not kept", tc.code)
		}
	}
}

func TestSearchNetworkErrorHidesKey(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // nothing listens any more

	_, err := (&Client{Key: testKey, Endpoint: srv.URL}).Search(context.Background(), testRequest())
	var se *Error
	if !errors.As(err, &se) || se.Status != NetworkError {
		t.Fatalf("got %v, want %s", err, NetworkError)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Errorf("error contains the API key: %v", err)
	}
}

func TestSearchRefusesBadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a bad request must not reach SerpApi")
	}))
	defer srv.Close()

	for name, change := range map[string]func(*Request){
		"cabin":     func(r *Request) { r.Cabin = "sofa" },
		"no adults": func(r *Request) { r.Adults = 0 },
		"no origin": func(r *Request) { r.Origin = "" },
	} {
		req := testRequest()
		change(&req)
		res, err := (&Client{Key: testKey, Endpoint: srv.URL}).Search(context.Background(), req)
		if res.Status != InvalidRequest || err == nil {
			t.Errorf("%s: got %s, %v", name, res.Status, err)
		}
	}
}

// TestRealResponses reads real responses saved from SerpApi, when
// FLIGHTPRICE_RESPONSES names a folder with them. They are not in the repo
// because they contain real searches.
func TestRealResponses(t *testing.T) {
	dir := os.Getenv("FLIGHTPRICE_RESPONSES")
	if dir == "" {
		t.Skip("FLIGHTPRICE_RESPONSES not set")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no responses in %s (%v)", dir, err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Parse(b)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		for i, o := range res.Offers {
			if o.PriceCents <= 0 || len(o.Route) < 2 || o.DurationMin <= 0 || len(o.Airlines) == 0 {
				t.Errorf("%s: offer %d incomplete: %+v", filepath.Base(f), i, o)
			}
		}
		t.Logf("%s: %s, %d offers", filepath.Base(f), res.Status, len(res.Offers))
	}
}
