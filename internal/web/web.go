// Package web is the long-running part of flightprice: the daily scheduler
// (run.go) and the report page (page.go). One Server does both, so a manual
// search and the scheduled one can never overlap.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/uros678/flightprice/internal/config"
	"github.com/uros678/flightprice/internal/serpapi"
	"github.com/uros678/flightprice/internal/store"
)

// Searcher runs one search; *serpapi.Client in the program, a fake in tests.
type Searcher interface {
	Search(ctx context.Context, req serpapi.Request) (serpapi.Result, error)
}

// accountReader reads SerpApi's own count of this cycle's searches.
// *serpapi.Client is one; a Searcher without it uses the local count only.
type accountReader interface {
	Account(ctx context.Context) (serpapi.Account, error)
}

// Server holds everything the scheduler and the page need.
type Server struct {
	Version string // shown on the page

	cfg     *config.Config
	store   *store.Store
	search  Searcher
	dataDir string
	log     *slog.Logger

	ctx        context.Context // ends at shutdown; background searches use it
	now        func() time.Time
	pause      time.Duration // between two searches
	retryAfter time.Duration // before retrying failed searches

	mu sync.Mutex // held while searches run

	usageMu sync.Mutex
	usage   usageOffset
}

// usageOffset is how many searches of a quota cycle SerpApi counted that
// the database does not know (searches by hand, another program, a lost
// database). It is learned before every run and added to the local count.
type usageOffset struct {
	cycle  time.Time // the cycle it belongs to
	offset int
}

// New makes a Server. ctx ends the scheduler and background searches.
func New(ctx context.Context, cfg *config.Config, st *store.Store, search Searcher, dataDir string, log *slog.Logger) *Server {
	loc := cfg.Schedule.Location()
	return &Server{
		cfg: cfg, store: st, search: search, dataDir: dataDir, log: log, ctx: ctx,
		now:        func() time.Time { return time.Now().In(loc) },
		pause:      3 * time.Second,
		retryAfter: 5 * time.Minute,
	}
}

//go:embed templates/*.html
var templates embed.FS

var pageTemplate = template.Must(template.New("index.html").Funcs(template.FuncMap{
	"join": strings.Join,
}).ParseFS(templates, "templates/index.html"))

// Handler is the web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /run", s.handleRun)
	mux.HandleFunc("GET /data.json", s.handleData)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := s.page(r.Context(), r.URL.Query().Get("msg"))
	if err != nil {
		s.log.Error("page", "err", err)
		http.Error(w, "the page could not be built, see the log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		s.log.Error("page template", "err", err)
	}
}

// handleRun is the Search now button. The page has no login (LAN or VPN
// only), so a request another site makes the browser send is refused: each
// one would cost searches.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "cross-site request refused", http.StatusForbidden)
		return
	}
	origin := strings.ToUpper(r.FormValue("origin"))
	msg := "Search started for " + origin + ". This page refreshes by itself until it is done."
	if err := s.Manual(origin); err != nil {
		msg = "Not started: " + err.Error() + "."
	}
	http.Redirect(w, r, "/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleData is the price history as JSON.
func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	obs, err := s.store.Observations(r.Context(), "", "", time.Time{})
	if err != nil {
		s.log.Error("data.json", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	type point struct {
		At       time.Time `json:"at"`
		Option   string    `json:"option"`
		Origin   string    `json:"origin"`
		Price    float64   `json:"price"`
		Currency string    `json:"currency"`
		Level    string    `json:"level,omitempty"`
		Stops    int       `json:"stops"`
		Minutes  int       `json:"duration_min"`
		Airlines []string  `json:"airlines"`
	}
	out := make([]point, 0, len(obs))
	for _, o := range obs {
		out = append(out, point{o.At, o.Option, o.Origin, float64(o.PriceCents) / 100, o.Currency, o.Level, o.Stops, o.DurationMin, o.Airlines})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
