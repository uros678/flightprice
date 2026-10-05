// Command flightprice tracks round-trip flight prices; see README.md.
//
//	flightprice [serve] [-config path] [-data dir]   scheduler and web page (default)
//	flightprice run [-config path] [-data dir]       today's planned searches once, now
//	flightprice plan [-config path] [-data dir]      what the next run would search (no searches)
//	flightprice search -origin FRA -option A         one live search, printed, not stored
//	flightprice check-config [-config path]
//	flightprice healthcheck [-config path]           for the container's HEALTHCHECK
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/serpapi"
	"github.com/uros678/flightprice/internal/store"
	"github.com/uros678/flightprice/internal/web"
)

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	commands := map[string]func([]string) error{
		"serve": serve, "run": runOnce, "plan": showPlan, "search": search,
		"check-config": checkConfig, "healthcheck": healthcheck,
	}
	f, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(os.Stderr, "flightprice: unknown command %q (serve, run, plan, search, check-config, healthcheck)\n", cmd)
		os.Exit(2)
	}
	if err := f(args); err != nil {
		fmt.Fprintln(os.Stderr, "flightprice:", err)
		os.Exit(1)
	}
}

type flags struct {
	fs     *flag.FlagSet
	config *string
	data   *string
}

func newFlags(name string) flags {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return flags{fs: fs,
		config: fs.String("config", "config.toml", "config file"),
		data:   fs.String("data", "data", "folder for the database and its backups"),
	}
}

// open loads the config, the API key and the database, and makes the web
// server around them.
func open(ctx context.Context, f flags, needKey bool) (*config.Config, *store.Store, *web.Server, error) {
	cfg, err := config.Load(*f.config)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := config.SerpAPIKey()
	if err != nil && needKey {
		return nil, nil, nil, err
	}
	if err := os.MkdirAll(*f.data, 0o755); err != nil {
		return nil, nil, nil, err
	}
	st, err := store.Open(ctx, filepath.Join(*f.data, "flightprice.db"), store.Limits{
		MaxStops: cfg.Constraints.MaxStops, MaxDurationMin: cfg.Constraints.MaxDurationMin(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := web.New(ctx, cfg, st, &serpapi.Client{Key: key}, *f.data, log)
	return cfg, st, srv, nil
}

func serve(args []string) error {
	f := newFlags("serve")
	f.fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, st, srv, err := open(ctx, f, true)
	if err != nil {
		return err
	}
	defer st.Close()

	httpSrv := &http.Server{Addr: cfg.Web.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	go srv.Schedule(ctx)
	slog.Info("flightprice started", "listen", cfg.Web.Listen, "data", *f.data)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdown)
}

// runOnce runs today's planned searches now, as the scheduler would. They
// count as today's run, so a later start of serve does not repeat them.
func runOnce(args []string) error {
	f := newFlags("run")
	f.fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	_, st, srv, err := open(ctx, f, true)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = srv.RunPlanned(ctx)
	return err
}

func showPlan(args []string) error {
	f := newFlags("plan")
	f.fs.Parse(args)
	ctx := context.Background()
	_, st, srv, err := open(ctx, f, false)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := srv.Plan(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Quota cycle %s to %s: %d used, %d days left, today's share %.1f calls\n",
		p.CycleStart.Format(config.DateLayout), p.CycleEnd.Format(config.DateLayout), p.Used, p.DaysLeft, p.Share)
	for _, w := range p.Waiting {
		fmt.Printf("Option %s: not searched before %s\n", w.Option, w.OpensOn.Format(config.DateLayout))
	}
	if len(p.Searches) == 0 {
		fmt.Println("No searches planned.")
	}
	for _, s := range p.Searches {
		kind := ""
		if s.Probe {
			kind = " (is it on sale yet?)"
		}
		fmt.Printf("  %s option %s%s\n", s.Origin, s.Option, kind)
	}
	return nil
}

func checkConfig(args []string) error {
	f := newFlags("check-config")
	f.fs.Parse(args)
	cfg, err := config.Load(*f.config)
	if err != nil {
		return err
	}
	fmt.Printf("%s is OK: %d passengers, %d origins, %d date options\n",
		*f.config, cfg.Passengers.Total(), len(cfg.Route.Origins), len(cfg.Options))
	return nil
}

// healthcheck asks the running server's /healthz; the image has no curl.
func healthcheck(args []string) error {
	f := newFlags("healthcheck")
	f.fs.Parse(args)
	cfg, err := config.Load(*f.config)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.Web.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

// search runs one live search and prints the offers. It uses one search from
// the SerpApi quota and stores nothing.
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

	req := web.NewRequest(cfg, *origin, cfg.Options[i])
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

func money(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }
