# flightprice

Small self-hosted flight price tracker. It checks round-trip prices for a few
fixed date combinations from one or more departure airports through
[SerpApi](https://serpapi.com/)'s Google Flights engine, keeps the full price
history in SQLite, sends an [ntfy](https://ntfy.sh/) notification when a fare
drops below your threshold, and shows everything on one web page.

It only reads prices. It never books or reserves anything.

> **Status:** early development. Searching, storing and the web page work;
> notifications are not written yet, and the configuration format may still
> change.

## How it works

- One Go binary, one process, one container. SQLite is compiled in; the
  database is a file in `/data`.
- Every morning the scheduler spreads the remaining monthly SerpApi budget
  over the days left in the quota cycle and checks the airports that have
  gone longest without a check. One airport check is one search per date
  option.
- The price of a check is the cheapest offer within your limits for stops
  and travel time, as the total for all passengers.
- An option is not searched before its return date is within
  `max_days_ahead` days (Google has nothing further out, and empty searches
  still count). Once in that window it is checked once a day until fares
  appear.
- Planned: an alert when that price is below `total_below` and at least
  `min_improvement` below the last alerted price.
- The web page shows the best price per date option, a table per airport,
  the price history chart, the latest offers, the run log and a "Search now"
  button. It is plain HTML with an SVG chart, no JavaScript.
- A copy of the database is written to `data/backup/` once a week (last 8
  kept).

## Configuration

Copy `config.example.toml` to `config.toml` and edit it. The file is read at
startup; restart the container after a change.

The SerpApi key never goes into the config file. Pass it as `SERPAPI_KEY`
or, with Docker secrets, as a file named by `SERPAPI_KEY_FILE`.

## Commands

```sh
flightprice serve -config config.toml -data data   # scheduler + web page (the default)
flightprice plan  -config config.toml -data data   # what the next run would search, no searches
flightprice run   -config config.toml -data data   # today's planned searches once, now
flightprice search -origin FRA -option A           # one live search, printed, not stored (1 search)
flightprice check-config -config config.toml       # check the file, no searches
```

## Run with Docker Compose

```sh
cp config.example.toml config.toml        # then edit it
mkdir -p data secrets
printf '%s' 'YOUR_SERPAPI_KEY' > secrets/serpapi_key
chmod 600 secrets/serpapi_key
docker compose up -d
```

The page is at http://localhost:8080. The container runs as uid/gid 1000 by
default (`UID` / `GID` in `.env`), so that user must be able to write
`./data`. The image is distroless: no shell, read-only root filesystem, no
capabilities.

The page has no login. Keep it on your LAN or VPN, or put an authenticating
proxy in front of it.

## Updates and your data

The image holds only the program; the database is in `./data`. To update:

```sh
docker compose pull && docker compose up -d
```

All price history stays. When a new version changes the database layout,
it first copies the database to `data/backup/flightprice-schema-N.db` and
then upgrades it. An older version refuses to open a newer database rather
than damage it: to go back, stop the container, restore that copy as
`data/flightprice.db` and start the older image. A weekly copy of the
database is kept in `data/backup/` as well (the last 8).

Images are tagged with the full version (`0.1.0`), the minor version
(`0.1`) and `latest`; pin one in `compose.yml` to update only when you
choose.

## Build

```sh
CGO_ENABLED=0 go build -o flightprice ./cmd/flightprice
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o flightprice-linux-amd64 ./cmd/flightprice
docker build -t flightprice .
```

Releases are built by GitHub Actions from a `v*` tag.

## License

MIT, see [LICENSE](LICENSE).
