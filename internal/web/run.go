package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/plan"
	"github.com/uros678/flightprice/internal/serpapi"
	"github.com/uros678/flightprice/internal/store"
)

// Sources of runs, as stored.
const (
	SourceScheduled = "scheduled"
	SourceManual    = "manual"
	SourceRetry     = "retry"
	SourceProbe     = "probe"
)

// ErrBusy is returned when a run is already going on.
var ErrBusy = errors.New("a search is already running")

// errBudget stops a run when the quota of the cycle is used up.
var errBudget = errors.New("the monthly SerpApi budget is used up")

// backupEvery and backupKeep: a copy of the database once a week, the last
// 8 kept.
const (
	backupEvery = 7 * 24 * time.Hour
	backupKeep  = 8
)

// Plan is the plan for now, made from what the store knows.
func (s *Server) Plan(ctx context.Context) (plan.Plan, error) {
	return s.planAt(ctx, s.now())
}

func (s *Server) planAt(ctx context.Context, now time.Time) (plan.Plan, error) {
	start, _ := plan.Cycle(now, s.cfg.Budget.ResetDay)
	used, err := s.store.CallsSince(ctx, start)
	if err != nil {
		return plan.Plan{}, err
	}
	last, err := s.store.LastChecked(ctx)
	if err != nil {
		return plan.Plan{}, err
	}
	onSale := map[string]bool{}
	for _, o := range s.cfg.Options {
		if onSale[o.Name], err = s.store.HasOffers(ctx, o.Name); err != nil {
			return plan.Plan{}, err
		}
	}
	return plan.Make(s.cfg, now, used, last, onSale), nil
}

// RunPlanned runs today's plan: the planned searches one after the other,
// then one retry of those that failed for a passing reason, then the weekly
// backup. Only one run (planned or manual) goes on at a time.
func (s *Server) RunPlanned(ctx context.Context) (plan.Plan, error) {
	if !s.mu.TryLock() {
		return plan.Plan{}, ErrBusy
	}
	defer s.mu.Unlock()

	p, err := s.Plan(ctx)
	if err != nil {
		return p, err
	}
	s.log.Info("daily run", "calls", p.Calls, "share", fmt.Sprintf("%.1f", p.Share), "used", p.Used,
		"days_left", p.DaysLeft, "checks", p.Checks())
	for _, w := range p.Waiting {
		s.log.Info("not searched yet", "option", w.Option, "opens_on", w.OpensOn.Format(config.DateLayout))
	}

	var retry []plan.Search
	for i, sr := range p.Searches {
		if i > 0 && !s.sleep(ctx, s.pause) {
			return p, ctx.Err()
		}
		source := SourceScheduled
		if sr.Probe {
			source = SourceProbe
		}
		st, err := s.searchOne(ctx, sr.Origin, sr.Option, source)
		if errors.Is(err, errBudget) || ctx.Err() != nil {
			return p, err
		}
		if st == serpapi.NetworkError || st == serpapi.ProviderError {
			retry = append(retry, sr)
		}
	}
	if len(retry) > 0 && s.sleep(ctx, s.retryAfter) {
		for _, sr := range retry {
			if _, err := s.searchOne(ctx, sr.Origin, sr.Option, SourceRetry); errors.Is(err, errBudget) || ctx.Err() != nil {
				break
			}
		}
	}
	s.backup(ctx)
	return p, nil
}

// Manual checks one origin now for every option in the sales window, in
// the background. It returns at once; ErrBusy when a run is going on.
func (s *Server) Manual(origin string) error {
	if !slices.Contains(s.cfg.Route.Origins, origin) {
		return fmt.Errorf("%s is not one of the origins", origin)
	}
	options := s.searchable()
	if len(options) == 0 {
		return errors.New("no date option is on sale yet")
	}
	start, _ := plan.Cycle(s.now(), s.cfg.Budget.ResetDay)
	used, err := s.store.CallsSince(s.ctx, start)
	if err != nil {
		return err
	}
	if used+len(options) > s.cfg.Budget.MonthlyCalls {
		return errBudget
	}
	if !s.mu.TryLock() {
		return ErrBusy
	}
	go func() {
		defer s.mu.Unlock()
		for i, opt := range options {
			if i > 0 && !s.sleep(s.ctx, s.pause) {
				return
			}
			if _, err := s.searchOne(s.ctx, origin, opt, SourceManual); errors.Is(err, errBudget) {
				return
			}
		}
	}()
	return nil
}

// Busy reports whether a run is going on.
func (s *Server) Busy() bool {
	if s.mu.TryLock() {
		s.mu.Unlock()
		return false
	}
	return true
}

// searchable is the options in the sales window today.
func (s *Server) searchable() []string {
	var names []string
	for _, o := range s.cfg.Options {
		if plan.InWindow(o, s.cfg.Budget.MaxDaysAhead, s.now()) {
			names = append(names, o.Name)
		}
	}
	return names
}

// searchOne runs and stores one search. It refuses to start when the cycle's
// budget is used up, whatever the plan said.
func (s *Server) searchOne(ctx context.Context, origin, option, source string) (serpapi.Status, error) {
	now := s.now()
	start, _ := plan.Cycle(now, s.cfg.Budget.ResetDay)
	used, err := s.store.CallsSince(ctx, start)
	if err != nil {
		return "", err
	}
	if used >= s.cfg.Budget.MonthlyCalls {
		s.log.Warn("search skipped", "origin", origin, "option", option, "reason", errBudget)
		return "", errBudget
	}
	i := slices.IndexFunc(s.cfg.Options, func(o config.Option) bool { return o.Name == option })
	if i < 0 {
		return "", fmt.Errorf("unknown option %q", option)
	}
	req := NewRequest(s.cfg, origin, s.cfg.Options[i])
	res, searchErr := s.search.Search(ctx, req)

	// Save even when the context ended: the search may have been billed.
	_, err = s.store.SaveRun(context.WithoutCancel(ctx), store.Run{
		Option: option, Origin: origin, StartedAt: now, Source: source,
		Request: req, Result: res, Err: searchErr,
	})
	if err != nil {
		s.log.Error("saving the run failed", "origin", origin, "option", option, "err", err)
		return res.Status, err
	}
	attrs := []any{"origin", origin, "option", option, "source", source, "status", res.Status, "offers", len(res.Offers)}
	if searchErr != nil {
		s.log.Warn("search failed", append(attrs, "err", searchErr)...)
	} else {
		s.log.Info("search", attrs...)
	}
	return res.Status, nil
}

// backup writes the weekly copy of the database when the last one is older
// than a week.
func (s *Server) backup(ctx context.Context) {
	dir := filepath.Join(s.dataDir, "backup")
	last, err := store.LastBackup(dir)
	if err != nil {
		s.log.Error("backup", "err", err)
		return
	}
	now := s.now()
	if !last.IsZero() && now.Sub(last) < backupEvery-12*time.Hour {
		return
	}
	path, err := s.store.Backup(ctx, dir, now, backupKeep)
	if err != nil {
		s.log.Error("backup", "err", err)
		return
	}
	s.log.Info("backup written", "path", path)
}

// Schedule runs the plan every day at the configured time until ctx ends.
// When the program starts after today's time and today's run has not
// happened, it runs at once.
func (s *Server) Schedule(ctx context.Context) {
	if s.missedToday(ctx) {
		s.runLogged(ctx)
	}
	for {
		next := s.nextRun(s.now())
		s.log.Info("next run", "at", next.Format("2006-01-02 15:04 MST"))
		if !s.sleep(ctx, next.Sub(s.now())) {
			return
		}
		s.runLogged(ctx)
	}
}

func (s *Server) runLogged(ctx context.Context) {
	if _, err := s.RunPlanned(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("daily run", "err", err)
	}
}

// todayAt is today's run time.
func (s *Server) todayAt(now time.Time) time.Time {
	h, m := s.cfg.Schedule.Clock()
	y, mo, d := now.Date()
	return time.Date(y, mo, d, h, m, 0, 0, now.Location())
}

// nextRun is the first run time after now.
func (s *Server) nextRun(now time.Time) time.Time {
	t := s.todayAt(now)
	if !t.After(now) {
		h, m := s.cfg.Schedule.Clock()
		y, mo, d := now.AddDate(0, 0, 1).Date()
		t = time.Date(y, mo, d, h, m, 0, 0, now.Location())
	}
	return t
}

// missedToday reports whether today's run time has passed without a
// planned run.
func (s *Server) missedToday(ctx context.Context) bool {
	now := s.now()
	at := s.todayAt(now)
	if now.Before(at) {
		return false
	}
	n, err := s.store.CountRuns(ctx, at, SourceScheduled, SourceProbe)
	if err != nil {
		s.log.Error("checking today's run", "err", err)
		return false
	}
	return n == 0
}

// sleep waits for d or until ctx ends; it reports whether it waited fully.
func (s *Server) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// NewRequest is the search for one origin and date option, always for all
// passengers together.
func NewRequest(cfg *config.Config, origin string, o config.Option) serpapi.Request {
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

// discard is a logger for tests.
var discard = slog.New(slog.DiscardHandler)
