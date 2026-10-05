-- Schema version 1. Money is in cents, times are UTC (RFC 3339).
-- The observations view is not here: it depends on the config's limits and
-- is created by the program at every start (see store.go).

-- One SerpApi search: one date option from one origin.
CREATE TABLE runs (
  id           INTEGER PRIMARY KEY,
  option       TEXT    NOT NULL,   -- date option name, e.g. 'A'
  origin       TEXT    NOT NULL,   -- departure airport, e.g. 'FRA'
  started_at   TEXT    NOT NULL,
  source       TEXT    NOT NULL,   -- 'scheduled' | 'manual' | 'retry' | 'probe'
  status       TEXT    NOT NULL,   -- serpapi.Status
  error        TEXT,
  billed       INTEGER NOT NULL,   -- 1 when SerpApi answered (counts against the quota)
  request_json TEXT    NOT NULL,   -- what was asked, without the key
  raw_json     TEXT,               -- SerpApi's full answer
  price_level  TEXT,               -- Google: low | typical | high
  lowest       INTEGER,            -- Google's lowest price, cents
  typical_low  INTEGER,
  typical_high INTEGER
);
CREATE INDEX runs_started ON runs(started_at);
CREATE INDEX runs_origin ON runs(origin, started_at);

-- The itineraries of a run. Price is the round-trip total for all
-- passengers; the flight details are the outbound leg.
CREATE TABLE offers (
  id              INTEGER PRIMARY KEY,
  run_id          INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  price_cents     INTEGER NOT NULL,
  currency        TEXT    NOT NULL,
  stops           INTEGER NOT NULL,
  duration_min    INTEGER NOT NULL,
  airlines        TEXT    NOT NULL,  -- comma-separated, flight order
  route           TEXT    NOT NULL,  -- airports, comma-separated
  flight_numbers  TEXT    NOT NULL,  -- comma-separated
  depart_time     TEXT    NOT NULL,  -- local time at the origin, as SerpApi gives it
  arrive_time     TEXT    NOT NULL,
  departure_token TEXT
);
CREATE INDEX offers_run ON offers(run_id);

-- Sent price alerts. Suppression is per date option, across all origins.
CREATE TABLE alerts (
  id          INTEGER PRIMARY KEY,
  option      TEXT    NOT NULL,
  origin      TEXT    NOT NULL,
  run_id      INTEGER NOT NULL REFERENCES runs(id),
  price_cents INTEGER NOT NULL,
  reason      TEXT    NOT NULL,      -- 'threshold' | 'new_low' | 'price_level'
  sent_at     TEXT    NOT NULL
);
CREATE INDEX alerts_option ON alerts(option, sent_at);
