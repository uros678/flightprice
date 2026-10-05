# Minimal image for flightprice. It does not compile anything: it copies a
# static Linux binary built beforehand for the image's architecture
# (flightprice-linux-amd64 or flightprice-linux-arm64), so no Go is needed
# inside Docker. Released images are built by .github/workflows/release.yml;
# by hand:
#
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o flightprice-linux-amd64 ./cmd/flightprice
#   docker build -t flightprice .
#
# distroless/static: CA certificates (for HTTPS to SerpApi), no shell, a
# non-root user (uid 65532, overridden in compose). Time zone data is in the
# binary.
#
# The image holds only the program. The database lives in the /data volume,
# so replacing the image (another version) keeps all data; a newer version
# upgrades the database itself, after copying it to /data/backup.
FROM gcr.io/distroless/static-debian12:nonroot

# Set by BuildKit (the default builder) from the target platform.
ARG TARGETARCH
COPY flightprice-linux-${TARGETARCH} /flightprice

EXPOSE 8080
VOLUME /data

ENTRYPOINT ["/flightprice"]
CMD ["serve", "-config", "/config.toml", "-data", "/data"]

# /healthz only says that the program runs, not that the searches work.
HEALTHCHECK --interval=1m --timeout=5s --start-period=10s \
	CMD ["/flightprice", "healthcheck", "-config", "/config.toml"]
