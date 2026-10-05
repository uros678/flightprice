// Package config reads and checks config.toml, the single source of truth for
// what flightprice tracks. Secrets never go in the file: the SerpApi key comes
// from the environment (see SerpAPIKey).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // the distroless image has no zone files of its own

	"github.com/BurntSushi/toml"
)

// Config is the whole file. Load fills in the defaults for missing settings
// and refuses unknown ones, so a typo is an error instead of a silent default.
type Config struct {
	Passengers  Passengers     `toml:"passengers"`
	Route       Route          `toml:"route"`
	Constraints Constraints    `toml:"constraints"`
	Options     []Option       `toml:"option"`
	Budget      Budget         `toml:"budget"`
	Weights     map[string]int `toml:"weights"`
	Alert       Alert          `toml:"alert"`
	Schedule    Schedule       `toml:"schedule"`
	Web         Web            `toml:"web"`
}

// Passengers is who every search is for. All of them are always searched
// together, as one booking.
type Passengers struct {
	Adults   int `toml:"adults"`
	Children int `toml:"children"`
}

// Total is the number of seats every search asks for.
func (p Passengers) Total() int { return p.Adults + p.Children }

// Route is where from and where to. Each origin is searched on its own; any
// of the destinations counts.
type Route struct {
	Origins     []string `toml:"origins"`
	Destination []string `toml:"destination"`
	Cabin       string   `toml:"cabin"`
	Currency    string   `toml:"currency"`
	// Market is Google's country code (gl), e.g. "at". Prices can differ by
	// market; empty leaves it to SerpApi.
	Market string `toml:"market"`
}

// Cabins are the accepted values of route.cabin.
var Cabins = []string{"economy", "premium_economy", "business", "first"}

// Constraints decide which offers count. The tracked price is the cheapest
// offer within both limits.
type Constraints struct {
	MaxStops int `toml:"max_stops"`
	// MaxDurationH limits the outbound travel time; 0 means no limit.
	MaxDurationH int `toml:"max_duration_h"`
}

// MaxDurationMin is the travel time limit in minutes (SerpApi's unit), 0 for
// no limit.
func (c Constraints) MaxDurationMin() int { return c.MaxDurationH * 60 }

// Option is one fixed date combination.
type Option struct {
	Name   string `toml:"name"`
	Depart Date   `toml:"depart"`
	Return Date   `toml:"return"` // departure date of the return flight
	HomeBy Date   `toml:"home_by"`
}

// Budget is the SerpApi quota and how the schedule spends it.
type Budget struct {
	MonthlyCalls int `toml:"monthly_calls"`
	Reserve      int `toml:"reserve"`   // kept back for Search now and retries
	ResetDay     int `toml:"reset_day"` // day of the month the quota renews
	// MaxDaysAhead: an option whose return date is further away is not
	// searched yet. Google returns nothing that far out, and an empty search
	// still costs a call.
	MaxDaysAhead int `toml:"max_days_ahead"`
}

// Alert is when and where price alerts go. Amounts are whole units of
// route.currency, for all passengers together.
type Alert struct {
	TotalBelow     int    `toml:"total_below"` // 0 = no price alerts yet
	MinImprovement int    `toml:"min_improvement"`
	NtfyURL        string `toml:"ntfy_url"`
	NtfyTopic      string `toml:"ntfy_topic"`
}

// TotalBelowCents is TotalBelow in cents, the unit prices are stored in.
func (a Alert) TotalBelowCents() int64 { return int64(a.TotalBelow) * 100 }

// MinImprovementCents is MinImprovement in cents.
func (a Alert) MinImprovementCents() int64 { return int64(a.MinImprovement) * 100 }

// Schedule is when the daily run starts.
type Schedule struct {
	Time     string `toml:"time"` // "15:04"
	Timezone string `toml:"timezone"`

	loc          *time.Location
	hour, minute int
}

// Location is the parsed time zone.
func (s Schedule) Location() *time.Location { return s.loc }

// Clock is the parsed start time.
func (s Schedule) Clock() (hour, minute int) { return s.hour, s.minute }

// Web is the report page.
type Web struct {
	Listen string `toml:"listen"`
}

func defaults() *Config {
	return &Config{
		Route:       Route{Cabin: "economy", Currency: "EUR"},
		Constraints: Constraints{MaxStops: 1},
		Budget:      Budget{MonthlyCalls: 250, Reserve: 10, ResetDay: 1, MaxDaysAhead: 331},
		Alert:       Alert{NtfyURL: "https://ntfy.sh"},
		Schedule:    Schedule{Time: "07:00", Timezone: "UTC"},
		Web:         Web{Listen: ":8080"},
	}
}

// Load reads and checks the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse reads and checks a config from its TOML text.
func Parse(src string) (*Config, error) {
	cfg := defaults()
	meta, err := toml.Decode(src, cfg)
	if err != nil {
		return nil, err
	}
	if und := meta.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown setting: %s", strings.Join(keys, ", "))
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return cfg, nil
}

var (
	iataCode  = regexp.MustCompile(`^[A-Z]{3}$`)
	currency  = regexp.MustCompile(`^[A-Z]{3}$`)
	market    = regexp.MustCompile(`^[a-z]{2}$`)
	topicName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// check validates the decoded config, normalizes airport codes to upper case
// and parses the schedule. It reports every problem at once.
func (c *Config) check() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	p := c.Passengers
	switch {
	case p.Adults < 1:
		bad("passengers.adults must be at least 1")
	case p.Children < 0:
		bad("passengers.children must not be negative")
	case p.Total() > 9:
		bad("passengers: Google Flights allows at most 9 passengers, got %d", p.Total())
	}

	c.Route.Origins = upper(c.Route.Origins)
	c.Route.Destination = upper(c.Route.Destination)
	checkAirports := func(key string, codes []string) {
		if len(codes) == 0 {
			bad("route.%s: at least one airport is needed", key)
		}
		for i, code := range codes {
			if !iataCode.MatchString(code) {
				bad("route.%s: %q is not a 3-letter airport code", key, code)
			} else if slices.Index(codes, code) != i {
				bad("route.%s: %s is listed twice", key, code)
			}
		}
	}
	checkAirports("origins", c.Route.Origins)
	checkAirports("destination", c.Route.Destination)
	if !slices.Contains(Cabins, c.Route.Cabin) {
		bad("route.cabin: %q is not one of %s", c.Route.Cabin, strings.Join(Cabins, ", "))
	}
	if !currency.MatchString(c.Route.Currency) {
		bad("route.currency: %q is not a 3-letter currency code like EUR", c.Route.Currency)
	}
	if c.Route.Market != "" && !market.MatchString(c.Route.Market) {
		bad("route.market: %q is not a 2-letter lower-case country code like at", c.Route.Market)
	}

	if c.Constraints.MaxStops < 0 || c.Constraints.MaxStops > 3 {
		bad("constraints.max_stops must be between 0 and 3, got %d", c.Constraints.MaxStops)
	}
	if c.Constraints.MaxDurationH < 0 {
		bad("constraints.max_duration_h must not be negative (0 = no limit)")
	}

	if len(c.Options) == 0 {
		bad("at least one [[option]] with dates is needed")
	}
	for i, o := range c.Options {
		name := fmt.Sprintf("option %d", i+1)
		switch {
		case o.Name == "":
			bad("%s: name is missing", name)
		case slices.IndexFunc(c.Options, func(x Option) bool { return x.Name == o.Name }) != i:
			bad("%s: name %q is used twice", name, o.Name)
		default:
			name = fmt.Sprintf("option %q", o.Name)
		}
		switch {
		case o.Depart.IsZero() || o.Return.IsZero():
			bad("%s: depart and return dates are both needed", name)
		case !o.Return.After(o.Depart.Time):
			bad("%s: return %s is not after depart %s", name, o.Return, o.Depart)
		case !o.HomeBy.IsZero() && o.HomeBy.Before(o.Return.Time):
			bad("%s: home_by %s is before return %s", name, o.HomeBy, o.Return)
		}
	}

	b := c.Budget
	if b.MonthlyCalls < 1 {
		bad("budget.monthly_calls must be at least 1")
	}
	if b.Reserve < 0 || b.Reserve >= b.MonthlyCalls {
		bad("budget.reserve must be between 0 and monthly_calls - 1, got %d", b.Reserve)
	}
	if b.ResetDay < 1 || b.ResetDay > 31 {
		bad("budget.reset_day must be a day of the month (1-31), got %d", b.ResetDay)
	}
	if b.MaxDaysAhead < 1 || b.MaxDaysAhead > 366 {
		bad("budget.max_days_ahead must be between 1 and 366, got %d", b.MaxDaysAhead)
	}

	if c.Weights != nil {
		weights := make(map[string]int, len(c.Weights))
		for code, w := range c.Weights {
			code = strings.ToUpper(code)
			if !slices.Contains(c.Route.Origins, code) {
				bad("weights: %s is not in route.origins", code)
			}
			if w < 1 {
				bad("weights.%s must be at least 1, got %d", code, w)
			}
			weights[code] = w
		}
		c.Weights = weights
	}

	a := c.Alert
	if a.TotalBelow < 0 || a.MinImprovement < 0 {
		bad("alert.total_below and alert.min_improvement must not be negative")
	}
	if u, err := url.Parse(a.NtfyURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("alert.ntfy_url: %q is not an http(s) URL", a.NtfyURL)
	}
	if !topicName.MatchString(a.NtfyTopic) {
		bad("alert.ntfy_topic: %q must be 1-64 letters, digits, - or _", a.NtfyTopic)
	}

	if t, err := time.Parse("15:04", c.Schedule.Time); err != nil {
		bad("schedule.time: %q is not a time like 07:00", c.Schedule.Time)
	} else {
		c.Schedule.hour, c.Schedule.minute = t.Hour(), t.Minute()
	}
	if loc, err := time.LoadLocation(c.Schedule.Timezone); err != nil || c.Schedule.Timezone == "" {
		bad("schedule.timezone: %q is not a time zone like Europe/Berlin", c.Schedule.Timezone)
	} else {
		c.Schedule.loc = loc
	}

	if c.Web.Listen == "" {
		bad("web.listen must not be empty")
	}

	return errors.Join(errs...)
}

// Weight is how often an origin is checked relative to the others (default 1).
func (c *Config) Weight(origin string) int {
	if w, ok := c.Weights[origin]; ok {
		return w
	}
	return 1
}

func upper(codes []string) []string {
	out := make([]string, len(codes))
	for i, s := range codes {
		out[i] = strings.ToUpper(strings.TrimSpace(s))
	}
	return out
}

// Date is a calendar date without a time, written in the file as a TOML
// date (2027-05-10) or a string ("2027-05-10"). It is kept at midnight UTC.
type Date struct{ time.Time }

// DateLayout is how dates are written in the file and sent to SerpApi.
const DateLayout = "2006-01-02"

// UnmarshalTOML implements toml.Unmarshaler.
func (d *Date) UnmarshalTOML(v any) error {
	var t time.Time
	switch v := v.(type) {
	case time.Time:
		if v.Hour() != 0 || v.Minute() != 0 || v.Second() != 0 || v.Nanosecond() != 0 {
			return fmt.Errorf("%v: want a date without a time, like 2027-05-10", v)
		}
		t = v
	case string:
		var err error
		if t, err = time.Parse(DateLayout, v); err != nil {
			return fmt.Errorf("%q: want a date like 2027-05-10", v)
		}
	default:
		return fmt.Errorf("%v: want a date like 2027-05-10", v)
	}
	d.Time = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return nil
}

// String is the date as YYYY-MM-DD, or "" when it is not set.
func (d Date) String() string {
	if d.IsZero() {
		return ""
	}
	return d.Format(DateLayout)
}

// SerpAPIKey returns the SerpApi key from the file named by SERPAPI_KEY_FILE
// (a Docker secret) or from SERPAPI_KEY. The file wins when both are set.
func SerpAPIKey() (string, error) {
	if path := os.Getenv("SERPAPI_KEY_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("SERPAPI_KEY_FILE: %w", err)
		}
		key := strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("SERPAPI_KEY_FILE: %s is empty", path)
		}
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("SERPAPI_KEY")); key != "" {
		return key, nil
	}
	return "", errors.New("no SerpApi key: set SERPAPI_KEY_FILE or SERPAPI_KEY")
}
