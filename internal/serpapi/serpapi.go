// Package serpapi runs Google Flights round-trip searches through SerpApi
// and turns the answer into plain Offers. Only the fields flightprice uses
// are decoded; Result.Raw keeps the whole body so it can be stored in full.
//
// One Search is one SerpApi search from the monthly quota, also when Google
// returns nothing.
package serpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultEndpoint is SerpApi's search API.
const DefaultEndpoint = "https://serpapi.com/search.json"

// DefaultAccountEndpoint is SerpApi's account API. Reading it is free.
const DefaultAccountEndpoint = "https://serpapi.com/account.json"

// maxBody caps a response. Real ones are 20-40 KB.
const maxBody = 8 << 20

// Client runs searches. The zero value needs only Key.
type Client struct {
	Key             string
	Endpoint        string       // DefaultEndpoint when empty
	AccountEndpoint string       // DefaultAccountEndpoint when empty
	HTTP            *http.Client // a client with a 90 s timeout when nil
}

var defaultHTTP = &http.Client{Timeout: 90 * time.Second}

// Request is one round-trip search. It holds no secrets, so it can be stored
// with the run as the record of what was asked.
type Request struct {
	Origin       string   `json:"origin"`
	Destinations []string `json:"destinations"`
	Depart       string   `json:"depart"` // YYYY-MM-DD
	Return       string   `json:"return"` // departure date of the return flight
	Adults       int      `json:"adults"`
	Children     int      `json:"children,omitempty"`
	Cabin        string   `json:"cabin"` // economy, premium_economy, business, first
	Currency     string   `json:"currency"`
	Market       string   `json:"market,omitempty"` // Google's gl, e.g. "at"
	// MaxStops and MaxDurationMin let Google filter the offers. Its filter
	// applies to the outbound and the return flight, which the offer
	// details (outbound only) cannot show. nil and 0 mean no limit.
	MaxStops       *int `json:"max_stops,omitempty"`
	MaxDurationMin int  `json:"max_duration_min,omitempty"`
}

var travelClass = map[string]int{"economy": 1, "premium_economy": 2, "business": 3, "first": 4}

func (r Request) query(key string) (url.Values, error) {
	class, ok := travelClass[r.Cabin]
	if !ok {
		return nil, fmt.Errorf("unknown cabin %q", r.Cabin)
	}
	if r.Origin == "" || len(r.Destinations) == 0 || r.Depart == "" || r.Return == "" || r.Adults < 1 {
		return nil, errors.New("origin, destinations, both dates and at least 1 adult are needed")
	}
	q := url.Values{}
	q.Set("engine", "google_flights")
	q.Set("type", "1") // round trip
	q.Set("departure_id", r.Origin)
	q.Set("arrival_id", strings.Join(r.Destinations, ","))
	q.Set("outbound_date", r.Depart)
	q.Set("return_date", r.Return)
	q.Set("adults", strconv.Itoa(r.Adults))
	if r.Children > 0 {
		q.Set("children", strconv.Itoa(r.Children))
	}
	q.Set("travel_class", strconv.Itoa(class))
	// SerpApi's stops: 0 any, 1 nonstop only, 2 one stop or fewer, 3 two
	// stops or fewer.
	if r.MaxStops != nil && *r.MaxStops >= 0 && *r.MaxStops <= 2 {
		q.Set("stops", strconv.Itoa(*r.MaxStops+1))
	}
	if r.MaxDurationMin > 0 {
		q.Set("max_duration", strconv.Itoa(r.MaxDurationMin))
	}
	q.Set("currency", r.Currency)
	q.Set("hl", "en")
	if r.Market != "" {
		q.Set("gl", r.Market)
	}
	q.Set("api_key", key)
	return q, nil
}

// Status is the outcome of a search, as stored with the run.
type Status string

const (
	Success        Status = "SUCCESS"
	NoResults      Status = "NO_RESULTS" // valid search, Google has no flights (e.g. not on sale yet)
	RateLimited    Status = "RATE_LIMITED"
	AuthError      Status = "AUTH_ERROR"
	ProviderError  Status = "PROVIDER_ERROR"
	NetworkError   Status = "NETWORK_ERROR"
	InvalidRequest Status = "INVALID_REQUEST"
)

// Error is a failed search. Its message never contains the API key.
type Error struct {
	Status  Status
	Message string
}

func (e *Error) Error() string { return string(e.Status) + ": " + e.Message }

// Result is what a search returned. Status is always set; Raw is the body
// whenever SerpApi answered, also for errors.
type Result struct {
	Status   Status
	Offers   []Offer
	Insights *Insights // nil when Google gave none
	// GoogleFlightsURL opens the same search on Google Flights, where the
	// return flights can be seen and booked.
	GoogleFlightsURL string
	Raw              []byte
}

// Offer is one round-trip itinerary. The price is the round-trip total for
// all passengers; the flight details are the outbound leg (SerpApi gives the
// return leg only on a second search).
type Offer struct {
	PriceCents     int64
	Stops          int
	DurationMin    int      // outbound travel time
	Airlines       []string // in flight order, each once
	Route          []string // airports, e.g. FRA, KEF, JFK
	FlightNumbers  []string
	DepartTime     string // local time at the origin, "2006-01-02 15:04"
	ArriveTime     string // local time at the destination
	DepartureToken string // for a later search of the return leg
}

// Insights is Google's own view of the price for this search.
type Insights struct {
	LowestCents      int64
	Level            string // low, typical or high
	TypicalLowCents  int64
	TypicalHighCents int64
	History          []PricePoint // often empty for dates far ahead
}

// PricePoint is one entry of Google's price history.
type PricePoint struct {
	At         time.Time
	PriceCents int64
}

// Search runs one search. A search that worked returns a nil error, with
// Status Success or NoResults; anything else returns an *Error.
func (c *Client) Search(ctx context.Context, req Request) (Result, error) {
	q, err := req.query(c.Key)
	if err != nil {
		return c.fail(Result{}, InvalidRequest, err.Error())
	}
	body, st, msg := c.get(ctx, c.Endpoint, DefaultEndpoint, q)
	if st != "" {
		return c.fail(Result{Raw: body}, st, msg)
	}
	res, err := Parse(body)
	if err != nil {
		return c.fail(res, ProviderError, err.Error())
	}
	return res, nil
}

// Account is the part of SerpApi's account information flightprice uses.
type Account struct {
	PlanName         string `json:"plan_name"`
	SearchesPerMonth int    `json:"searches_per_month"`
	PlanSearchesLeft int    `json:"plan_searches_left"`
	ThisMonthUsage   int    `json:"this_month_usage"`
	RenewalDate      string `json:"plan_renewal_date"` // YYYY-MM-DD
}

// Account reads the plan and this cycle's usage. It costs no search.
func (c *Client) Account(ctx context.Context) (Account, error) {
	q := url.Values{}
	q.Set("api_key", c.Key)
	body, st, msg := c.get(ctx, c.AccountEndpoint, DefaultAccountEndpoint, q)
	if st != "" {
		_, err := c.fail(Result{}, st, msg)
		return Account{}, err
	}
	var a Account
	if err := json.Unmarshal(body, &a); err != nil {
		_, err := c.fail(Result{}, ProviderError, "unreadable account response: "+err.Error())
		return Account{}, err
	}
	return a, nil
}

// get sends a GET with query q to endpoint (fallback when empty). It returns
// the body, and on failure a Status and a message; the status is "" when
// SerpApi answered 200.
func (c *Client) get(ctx context.Context, endpoint, fallback string, q url.Values) ([]byte, Status, string) {
	if endpoint == "" {
		endpoint = fallback
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, InvalidRequest, err.Error()
	}
	client := c.HTTP
	if client == nil {
		client = defaultHTTP
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// A *url.Error repeats the whole URL, api_key included: keep only
		// the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, NetworkError, err.Error()
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, NetworkError, "reading the response: " + err.Error()
	}
	if len(body) > maxBody {
		return nil, ProviderError, "response larger than 8 MB"
	}
	if resp.StatusCode != http.StatusOK {
		return body, statusFor(resp.StatusCode), errorText(body, resp.Status)
	}
	return body, "", ""
}

// fail returns res with status st and an *Error, with the key cut out of
// the message in case anything ever repeats it.
func (c *Client) fail(res Result, st Status, msg string) (Result, error) {
	if c.Key != "" {
		msg = strings.ReplaceAll(msg, c.Key, "[api key]")
	}
	res.Status = st
	res.Offers, res.Insights = nil, nil
	return res, &Error{Status: st, Message: msg}
}

func statusFor(code int) Status {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return AuthError
	case code == http.StatusTooManyRequests:
		return RateLimited
	case code == http.StatusBadRequest || code == http.StatusNotFound:
		return InvalidRequest
	default:
		return ProviderError
	}
}

// errorText is SerpApi's "error" field, or fallback when the body has none.
func errorText(body []byte, fallback string) string {
	var r struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &r) == nil && r.Error != "" {
		return r.Error
	}
	return fallback
}

type response struct {
	Error          string `json:"error"`
	SearchMetadata struct {
		GoogleFlightsURL string `json:"google_flights_url"`
	} `json:"search_metadata"`
	SearchInformation struct {
		FlightsResultsState string `json:"flights_results_state"`
	} `json:"search_information"`
	BestFlights   []offerJSON   `json:"best_flights"`
	OtherFlights  []offerJSON   `json:"other_flights"`
	PriceInsights *insightsJSON `json:"price_insights"`
}

type offerJSON struct {
	Flights []struct {
		DepartureAirport airportJSON `json:"departure_airport"`
		ArrivalAirport   airportJSON `json:"arrival_airport"`
		Airline          string      `json:"airline"`
		FlightNumber     string      `json:"flight_number"`
	} `json:"flights"`
	TotalDuration  int      `json:"total_duration"`
	Price          *float64 `json:"price"`
	DepartureToken string   `json:"departure_token"`
}

type airportJSON struct {
	ID   string `json:"id"`
	Time string `json:"time"`
}

type insightsJSON struct {
	LowestPrice       *float64    `json:"lowest_price"`
	PriceLevel        string      `json:"price_level"`
	TypicalPriceRange []float64   `json:"typical_price_range"`
	PriceHistory      [][]float64 `json:"price_history"` // [unix seconds, price]
}

// Parse reads a SerpApi Google Flights response that came with HTTP 200.
// It is exported for tests and for reading stored responses again.
func Parse(body []byte) (Result, error) {
	res := Result{Raw: body}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		res.Status = ProviderError
		return res, fmt.Errorf("unreadable response: %w", err)
	}
	res.GoogleFlightsURL = GoogleFlightsURL(r.SearchMetadata.GoogleFlightsURL)
	if r.Error != "" {
		// An empty result also comes as HTTP 200 with an error text.
		if r.SearchInformation.FlightsResultsState == "Fully empty" || strings.Contains(r.Error, "hasn't returned any results") {
			res.Status = NoResults
			return res, nil
		}
		res.Status = ProviderError
		return res, errors.New(r.Error)
	}
	for _, o := range append(r.BestFlights, r.OtherFlights...) {
		// Offers without a price or flights cannot be tracked; Google
		// sometimes lists them without a fare.
		if o.Price == nil || len(o.Flights) == 0 {
			continue
		}
		first, last := o.Flights[0], o.Flights[len(o.Flights)-1]
		offer := Offer{
			PriceCents:     cents(*o.Price),
			Stops:          len(o.Flights) - 1,
			DurationMin:    o.TotalDuration,
			Route:          []string{first.DepartureAirport.ID},
			DepartTime:     first.DepartureAirport.Time,
			ArriveTime:     last.ArrivalAirport.Time,
			DepartureToken: o.DepartureToken,
		}
		for _, f := range o.Flights {
			offer.Route = append(offer.Route, f.ArrivalAirport.ID)
			offer.FlightNumbers = append(offer.FlightNumbers, f.FlightNumber)
			if f.Airline != "" && !slices.Contains(offer.Airlines, f.Airline) {
				offer.Airlines = append(offer.Airlines, f.Airline)
			}
		}
		res.Offers = append(res.Offers, offer)
	}
	if pi := r.PriceInsights; pi != nil {
		in := &Insights{Level: pi.PriceLevel}
		if pi.LowestPrice != nil {
			in.LowestCents = cents(*pi.LowestPrice)
		}
		if len(pi.TypicalPriceRange) == 2 {
			in.TypicalLowCents = cents(pi.TypicalPriceRange[0])
			in.TypicalHighCents = cents(pi.TypicalPriceRange[1])
		}
		for _, p := range pi.PriceHistory {
			if len(p) >= 2 {
				in.History = append(in.History, PricePoint{At: time.Unix(int64(p[0]), 0).UTC(), PriceCents: cents(p[1])})
			}
		}
		res.Insights = in
	}
	if len(res.Offers) == 0 {
		res.Status = NoResults
	} else {
		res.Status = Success
	}
	return res, nil
}

// GoogleFlightsURL returns u when it is a Google Flights link, else "".
func GoogleFlightsURL(u string) string {
	if strings.HasPrefix(u, "https://www.google.com/travel/flights") {
		return u
	}
	return ""
}

func cents(price float64) int64 { return int64(math.Round(price * 100)) }
