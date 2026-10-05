# Image for flightprice: a static Go binary on distroless. The first stage
# compiles it, the final image holds only the binary (no shell, no packages).
#
#   docker build -t flightprice .
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/flightprice ./cmd/flightprice

# distroless/static: CA certificates for HTTPS, a non-root user (uid 65532,
# overridden in compose). Time zone data is embedded in the binary.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/flightprice /flightprice
EXPOSE 8080
ENTRYPOINT ["/flightprice"]
CMD ["serve", "--config", "/config.toml", "--data", "/data"]
