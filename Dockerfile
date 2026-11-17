# syntax=docker/dockerfile:1
#
# BoxPilot — read-only SPX box spread scanner.
#
# The image contains the web server (cmd/boxpilotd) with its front-end
# assets embedded; there is no runtime file dependency and no database.
#
#   docker build -t boxpilot .
#   docker run --rm -p 127.0.0.1:8080:8080 \
#     --env-file .env -v boxpilot-data:/data \
#     -v "$PWD/config.yaml:/app/config.yaml:ro" boxpilot
#
# Publish to 127.0.0.1 on the host: the service holds Schwab credentials
# and has no authentication of its own.

# ---------- build ----------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first so the layer survives source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off keeps the binary fully static; the runtime image has no libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/boxpilotd ./cmd/boxpilotd

# ---------- runtime ----------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

COPY --from=build /out/boxpilotd /usr/local/bin/boxpilotd
COPY --from=build /src/config.example.yaml /app/config.yaml

# Inside the container the process must bind 0.0.0.0 for a published
# port to reach it; the perimeter is then the host-side publish binding
# plus the reverse proxy. Keep the publish on 127.0.0.1.
ENV BOXPILOT_ADDR=0.0.0.0:8080 \
    SCHWAB_TOKEN_STORE=/data/schwab_refresh_token

# The rotated OAuth refresh token lives here: mount a volume so a
# container restart does not lose access.
VOLUME ["/data"]

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/boxpilotd"]
CMD ["-config", "/app/config.yaml"]
