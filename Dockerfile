# Two stages: build a static binary, then drop it into a small runtime.
#
# Unlike HopReach, nothing here needs cgo — the SQLite driver is
# modernc.org/sqlite (pure Go), chosen precisely so this stays
# CGO_ENABLED=0 and the runtime image can be minimal.
FROM golang:1.23-bookworm AS build
ARG VERSION=dev
WORKDIR /src

# Dependencies first, so a source-only change doesn't re-download them.
COPY go.mod go.sum* ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X hopreact/internal/buildinfo.Version=${VERSION}" \
      -o /out/hopreact ./cmd/hopreact

FROM debian:bookworm-slim
# ca-certificates: HTTPS to CoreScope and the Discord API.
# tzdata: without it every timestamp silently renders as UTC, because
#   time.LoadLocation has no database to read.
# curl: the healthcheck below.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata curl \
 && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/hopreact /app/hopreact
COPY config.example.yaml /config/config.yaml

ENV HOPREACT_CONFIG=/config/config.yaml
# SQLite plus its -wal/-shm sidecars.
VOLUME ["/data"]
EXPOSE 8080

# Deliberately does NOT check CoreScope. If this went red whenever the
# upstream API was down, Docker would restart the container during exactly
# the outage the alert engine is designed to ride out. Poll freshness is
# reported in the UI instead.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz -o /dev/null || exit 1

ENTRYPOINT ["/app/hopreact"]
