# flightprice

Small self-hosted flight price tracker. It checks round-trip prices for a few
fixed date combinations from one or more departure airports through
[SerpApi](https://serpapi.com/)'s Google Flights engine, keeps the full price
history in SQLite, sends an [ntfy](https://ntfy.sh/) notification when a fare
drops below your threshold, and shows everything on one web page.

It only reads prices. It never books or reserves anything.

> **Status:** early development. The design is done, the code is not written
> yet, and the configuration format may still change.

## How it works

- One Go binary, one process, one container. SQLite is compiled in; the
  database is a file in `/data`.
- Every morning the scheduler spreads the remaining monthly SerpApi budget
  over the days left in the quota cycle and checks the airports that have
  gone longest without a check. One airport check is one search per date
  option.
- The price of a check is the cheapest offer within your limits for stops
  and travel time, as the total for all passengers.
- An alert is sent when that price is below `total_below` and at least
  `min_improvement` below the last alerted price, so the same price level is
  never reported twice.
- The web page shows the best price per date option, a table per airport,
  the price history chart, the run log and a "Search now" button.

## Configuration

Copy `config.example.toml` to `config.toml` and edit it. The file is read at
startup; restart the container after a change.

The SerpApi key never goes into the config file. Pass it as `SERPAPI_KEY`
or, with Docker secrets, as a file named by `SERPAPI_KEY_FILE`.

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

## Build

```sh
CGO_ENABLED=0 go build -o flightprice ./cmd/flightprice
docker build -t flightprice .
```

## License

MIT, see [LICENSE](LICENSE).
