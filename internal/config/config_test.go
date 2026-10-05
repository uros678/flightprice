package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// minimal is the smallest valid config; everything else has a default.
const minimal = `
[passengers]
adults = 1

[route]
origins = ["fra"]
destination = ["JFK"]

[[option]]
name = "A"
depart = 2027-05-10
return = 2027-05-24

[alert]
ntfy_topic = "flights"
`

func TestExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Options) != 2 || cfg.Options[1].Name != "B" || cfg.Options[1].Return.String() != "2027-06-21" {
		t.Errorf("options = %+v", cfg.Options)
	}
	if cfg.Passengers.Total() != 2 || cfg.Budget.MonthlyCalls != 250 {
		t.Errorf("passengers %+v, budget %+v", cfg.Passengers, cfg.Budget)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse(minimal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Route.Origins, []string{"FRA"}) {
		t.Errorf("origins not upper-cased: %v", cfg.Route.Origins)
	}
	if cfg.Route.Cabin != "economy" || cfg.Route.Currency != "EUR" || cfg.Route.Market != "" {
		t.Errorf("route = %+v", cfg.Route)
	}
	if cfg.Constraints.MaxStops != 1 || cfg.Constraints.MaxDurationMin() != 0 {
		t.Errorf("constraints = %+v", cfg.Constraints)
	}
	if b := cfg.Budget; b.MonthlyCalls != 250 || b.Reserve != 10 || b.ResetDay != 1 || b.MaxDaysAhead != 331 {
		t.Errorf("budget = %+v", b)
	}
	if cfg.Alert.NtfyURL != "https://ntfy.sh" || cfg.Alert.TotalBelowCents() != 0 {
		t.Errorf("alert = %+v", cfg.Alert)
	}
	if h, m := cfg.Schedule.Clock(); h != 7 || m != 0 || cfg.Schedule.Location().String() != "UTC" {
		t.Errorf("schedule = %d:%d %v", h, m, cfg.Schedule.Location())
	}
	if cfg.Web.Listen != ":8080" || cfg.Weight("FRA") != 1 {
		t.Errorf("listen %q, weight %d", cfg.Web.Listen, cfg.Weight("FRA"))
	}
	if o := cfg.Options[0]; o.Depart.String() != "2027-05-10" || !o.HomeBy.IsZero() {
		t.Errorf("option = %+v", o)
	}
}

func TestSettings(t *testing.T) {
	cfg, err := Parse(minimal + `
[weights]
fra = 3

[schedule]
time = "06:30"
timezone = "Europe/Berlin"
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Weight("FRA") != 3 {
		t.Errorf("weight = %d", cfg.Weight("FRA"))
	}
	if h, m := cfg.Schedule.Clock(); h != 6 || m != 30 || cfg.Schedule.Location().String() != "Europe/Berlin" {
		t.Errorf("schedule = %d:%d %v", h, m, cfg.Schedule.Location())
	}
}

func TestDateAsString(t *testing.T) {
	cfg, err := Parse(strings.Replace(minimal, "return = 2027-05-24", `return = "2027-05-24"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Options[0].Return.String(); got != "2027-05-24" {
		t.Errorf("return = %q", got)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name, change, want string
	}{
		{"unknown key", "\n[web]\nlisten_port = 1\n", "unknown setting: web.listen_port"},
		{"no adults", "\n[passengers]\nadults = 0\n", "passengers.adults"},
		{"too many", "\n[passengers]\nadults = 5\nchildren = 5\n", "at most 9 passengers"},
		{"bad airport", "\n[route]\norigins = [\"FRANK\"]\ndestination = [\"JFK\"]\n", `"FRANK" is not a 3-letter airport code`},
		{"twice", "\n[route]\norigins = [\"FRA\", \"fra\"]\ndestination = [\"JFK\"]\n", "FRA is listed twice"},
		{"cabin", "\n[route]\norigins = [\"FRA\"]\ndestination = [\"JFK\"]\ncabin = \"sofa\"\n", "route.cabin"},
		{"market", "\n[route]\norigins = [\"FRA\"]\ndestination = [\"JFK\"]\nmarket = \"AT\"\n", "route.market"},
		{"stops", "\n[constraints]\nmax_stops = 5\n", "constraints.max_stops"},
		{"return first", "\n[[option]]\nname = \"B\"\ndepart = 2027-05-10\nreturn = 2027-05-01\n", `option "B": return 2027-05-01 is not after depart 2027-05-10`},
		{"home_by", "\n[[option]]\nname = \"B\"\ndepart = 2027-05-10\nreturn = 2027-05-20\nhome_by = 2027-05-19\n", "home_by 2027-05-19 is before return"},
		{"same name", "\n[[option]]\nname = \"A\"\ndepart = 2027-06-10\nreturn = 2027-06-20\n", `name "A" is used twice`},
		{"time in date", "\n[[option]]\nname = \"B\"\ndepart = 2027-06-10T10:00:00\nreturn = 2027-06-20\n", "without a time"},
		{"reserve", "\n[budget]\nmonthly_calls = 10\nreserve = 10\n", "budget.reserve"},
		{"reset day", "\n[budget]\nreset_day = 32\n", "budget.reset_day"},
		{"weight origin", "\n[weights]\nMUC = 2\n", "weights: MUC is not in route.origins"},
		{"weight value", "\n[weights]\nFRA = 0\n", "weights.FRA must be at least 1"},
		{"ntfy url", "\n[alert]\nntfy_url = \"ntfy.sh\"\nntfy_topic = \"x\"\n", "alert.ntfy_url"},
		{"time", "\n[schedule]\ntime = \"7am\"\n", "schedule.time"},
		{"zone", "\n[schedule]\ntimezone = \"Mars/Base\"\n", "schedule.timezone"},
	} {
		// Later tables replace the minimal ones; a second [[option]] adds one.
		src := minimal + tc.change
		if strings.Contains(tc.change, "[passengers]") || strings.Contains(tc.change, "[route]") || strings.Contains(tc.change, "[alert]") {
			src = replaceTable(minimal, tc.change)
		}
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error with %q", tc.name, err, tc.want)
		}
	}
}

// replaceTable drops the tables from base that change defines again (TOML
// does not allow a table twice) and appends change.
func replaceTable(base, change string) string {
	var keep []string
	skip := false
	for _, line := range strings.Split(base, "\n") {
		if strings.HasPrefix(line, "[") {
			skip = strings.Contains(change, line+"\n")
		}
		if !skip {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n") + change
}

func TestAllErrorsAtOnce(t *testing.T) {
	_, err := Parse(`
[passengers]
adults = 0
[route]
origins = []
destination = ["JFK"]
[alert]
ntfy_topic = ""
`)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"passengers.adults", "route.origins", "[[option]]", "alert.ntfy_topic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestSerpAPIKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key")
	os.WriteFile(file, []byte("from-file\n"), 0o600)

	t.Setenv("SERPAPI_KEY", " from-env ")
	t.Setenv("SERPAPI_KEY_FILE", "")
	if key, err := SerpAPIKey(); err != nil || key != "from-env" {
		t.Errorf("env: %q, %v", key, err)
	}

	t.Setenv("SERPAPI_KEY_FILE", file)
	if key, err := SerpAPIKey(); err != nil || key != "from-file" {
		t.Errorf("file wins: %q, %v", key, err)
	}

	t.Setenv("SERPAPI_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := SerpAPIKey(); err == nil {
		t.Error("missing file: no error")
	}

	t.Setenv("SERPAPI_KEY_FILE", "")
	t.Setenv("SERPAPI_KEY", "")
	if _, err := SerpAPIKey(); err == nil {
		t.Error("no key: no error")
	}
}
