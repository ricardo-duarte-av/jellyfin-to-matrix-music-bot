# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TAG=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# CGO_ENABLED=0 keeps the binary static. The bot deliberately has no cgo
# dependencies: audio and video are encoded by the ffmpeg binary rather than by
# linking libopus and libx264 into the process.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags="-s -w \
            -X main.version=${TAG} \
            -X main.commit=${COMMIT} \
            -X main.buildTime=${BUILD_TIME}" \
        -o /out/musicbot ./cmd/musicbot

FROM alpine:3.21

# ffmpeg is a hard runtime dependency: it decodes everything Jellyfin serves,
# encodes the Opus the call carries, and renders the album art keyframes. It
# must have libopus and libx264, which Alpine's build does.
#
# ttf-dejavu is for the generated placeholder tile shown when a track has no
# cover art; without a font that tile falls back to a plain square.
RUN apk add --no-cache \
        ffmpeg \
        ttf-dejavu \
        ca-certificates \
        tzdata

COPY --from=build /out/musicbot /usr/local/bin/musicbot

# Fail the build rather than the first playback if anything is missing. This
# runs the bot's own preflight, which renders a real album art tile, so it
# checks the encoders, the font lookup and the H.264 output together.
RUN musicbot -check

# Nothing here needs root.
RUN adduser -D -u 1000 musicbot
USER musicbot

# Repeat the check as the unprivileged user, so a permissions problem shows up
# now rather than on the first track.
RUN musicbot -check

# The config carries an access token and an API key, so it is mounted rather
# than baked in: -v ./config.yaml:/config/config.yaml:ro
VOLUME ["/config"]

ENTRYPOINT ["/usr/local/bin/musicbot"]
CMD ["-config", "/config/config.yaml"]
