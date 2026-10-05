// Command flightprice tracks round-trip flight prices; see README.md.
//
//	flightprice check-config [-config path]
//	flightprice search -origin FRA -option A [-config path]
//
// serve (the scheduler and web page), run and healthcheck come later.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/serpapi"
)

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "check-config":
		err = checkConfig(args)
	case "search":
		err = search(args)
	case "serve", "run", "healthcheck":
		err = fmt.Errorf("%s is not implemented yet", cmd)
	default:
		err = fmt.Errorf("unknown command %q (commands: check-config, search)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "flightprice:", err)
		os.Exit(1)
	}
}

func checkConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ExitOnError)
	path := fs.String("config", "config.toml", "config file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("%s is OK: %d passengers, %d origins, %d date options\n",
		*path, cfg.Passengers.Total(), len(cfg.Route.Origins), len(cfg.Options))
	return nil
}

// search runs one live search and prints the offers. It uses one search from
// the SerpApi quota.
func search(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	path := fs.String("config", "config.toml", "config file")
	origin := fs.String("origin", "", "departure airport from route.origins")
	option := fs.String("option", "", "name of the date option")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	*origin = strings.ToUpper(*origin)
	if !slices.Contains(cfg.Route.Origins, *origin) {
		return fmt.Errorf("-origin must be one of %s", strings.Join(cfg.Route.Origins, ", "))
	}
	i := slices.IndexFunc(cfg.Options, func(o config.Option) bool { return o.Name == *option })
	if i < 0 {
		return fmt.Errorf("-option must name an [[option]] in %s", *path)
	}
	key, err := config.SerpAPIKey()
	if err != nil {
		return err
	}

	req := newRequest(cfg, *origin, cfg.Options[i])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := (&serpapi.Client{Key: key}).Search(ctx, req)
	if err != nil {
		return err
	}

	fmt.Printf("%s -> %s, %s to %s, %d passengers: %s\n",
		req.Origin, strings.Join(req.Destinations, ","), req.Depart, req.Return, cfg.Passengers.Total(), res.Status)
	if in := res.Insights; in != nil {
		fmt.Printf("Google: %s, typical %s-%s\n", in.Level, money(in.TypicalLowCents), money(in.TypicalHighCents))
	}
	maxStops, maxMin := cfg.Constraints.MaxStops, cfg.Constraints.MaxDurationMin()
	slices.SortStableFunc(res.Offers, func(a, b serpapi.Offer) int { return cmp.Compare(a.PriceCents, b.PriceCents) })
	for _, o := range res.Offers {
		mark := " "
		if o.Stops <= maxStops && (maxMin == 0 || o.DurationMin <= maxMin) {
			mark = "*"
		}
		fmt.Printf("%s %10s  %d stop  %2dh%02d  %-20s %s\n", mark, money(o.PriceCents), o.Stops,
			o.DurationMin/60, o.DurationMin%60, strings.Join(o.Route, ">"), strings.Join(o.Airlines, ", "))
	}
	if len(res.Offers) > 0 {
		fmt.Println("* = within constraints (max_stops, max_duration_h)")
	}
	return nil
}

// newRequest is the search for one origin and date option, always for all
// passengers together.
func newRequest(cfg *config.Config, origin string, o config.Option) serpapi.Request {
	return serpapi.Request{
		Origin:       origin,
		Destinations: cfg.Route.Destination,
		Depart:       o.Depart.String(),
		Return:       o.Return.String(),
		Adults:       cfg.Passengers.Adults,
		Children:     cfg.Passengers.Children,
		Cabin:        cfg.Route.Cabin,
		Currency:     cfg.Route.Currency,
		Market:       cfg.Route.Market,
	}
}

func money(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }
