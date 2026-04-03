FROM --platform=$BUILDPLATFORM golang:1.24.0-bookworm AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

WORKDIR /src/backend

COPY backend/go.mod backend/go.sum ./
RUN go mod download

COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && useradd --create-home --shell /usr/sbin/nologin gateway \
  && mkdir -p /app /data \
  && chown -R gateway:gateway /app /data

WORKDIR /app

COPY --from=build /out/gateway /app/gateway

USER gateway

ENV GATEWAY_BIND=:8080 \
  GATEWAY_STATE_PATH=/data/gateway.db \
  GATEWAY_ENABLE_REALTIME=true

EXPOSE 8080

ENTRYPOINT ["/app/gateway"]
