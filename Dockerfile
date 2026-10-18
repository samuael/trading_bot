# syntax=docker/dockerfile:1

# Build a static binary.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trading_bot . \
    && mkdir -p /out/data

# Minimal runtime: CA certificates and tzdata, no shell, non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trading_bot /app/trading_bot
COPY config_example.json strategy.json /app/
COPY --from=build --chown=nonroot:nonroot /out/data /data

# State lives in /data: positions.json, trades.csv, profit_log.csv, and gocryptotrader's ~/.gocryptotrader.
ENV HOME=/data
WORKDIR /data
VOLUME /data

# Prometheus metrics.
EXPOSE 2113

# GATEIO_API_KEY and GATEIO_API_SECRET must be supplied at runtime, never baked into the image.
ENTRYPOINT ["/app/trading_bot", "-exchange-config", "/app/config_example.json", "-strategy-config", "/app/strategy.json"]
