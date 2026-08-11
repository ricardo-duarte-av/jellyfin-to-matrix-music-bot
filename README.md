# jellyfin-to-matrix-music-bot

A bot that sits in a Matrix room's Element Call and streams music from a
Jellyfin server into it, driven by chat commands.

```
!search pink floyd
  1. [artist] Pink Floyd
  2. [album]  Pink Floyd – The Dark Side of the Moon
  3. [track]  Pink Floyd – Money (The Dark Side of the Moon) [6:23]
!play 3
  Queued Pink Floyd – Money (The Dark Side of the Moon) [6:23].
```

Anyone in the room can join the Element Call and hear it.

## How it works

Element Call is not peer-to-peer: it is MatrixRTC (MSC4143) on top of a LiveKit
SFU (MSC4195). The bot therefore does three separate things to become audible:

1. **Signalling** — publishes an `org.matrix.msc3401.call.member` state event so
   Element Call clients show it as a participant, plus a *delayed leave* event
   the homeserver fires if the bot dies without cleaning up.
2. **Authorization** — asks the homeserver for a Matrix OpenID token and trades
   it at the MatrixRTC authorization service (`lk-jwt-service`, discovered from
   `/.well-known/matrix/client`) for a LiveKit URL and JWT.
3. **Media** — connects to the LiveKit SFU and publishes an Opus track. ffmpeg
   reads the track from Jellyfin and encodes it to Opus; the bot demuxes the Ogg
   stream and paces one 20 ms packet at a time into the SFU.

## Requirements

- Go 1.25+ (the toolchain is pinned in `go.mod` and downloaded automatically)
- `ffmpeg` on `PATH`, built with `libopus` (check: `ffmpeg -encoders | grep libopus`)
- A Matrix account for the bot, with an access token
- A homeserver with a MatrixRTC backend (LiveKit + `lk-jwt-service`) — the same
  thing Element Call itself needs
- A Jellyfin server with an API key

There are no cgo dependencies: audio is encoded by ffmpeg rather than by linking
libopus into the binary.

## The room must be unencrypted

**This is a hard constraint, not a preference.** In an encrypted room Element
Call encrypts call media end-to-end (SFrame, with per-participant keys exchanged
over Olm-encrypted room events). This bot publishes plain Opus, so in an
encrypted room other participants would receive audio they cannot decrypt.

Use a dedicated, unencrypted room for streaming. Element Call runs the call
without media E2EE there, and everything works.

## Setup

```sh
cp config.example.yaml config.yaml
$EDITOR config.yaml
go build ./cmd/musicbot
./musicbot -config config.yaml
```

The bot joins the configured room and the call at startup and stays in the call,
silent, until it is given something to play.

`matrix.device_id` must be the device the access token belongs to — it forms half
of the LiveKit identity the JWT service derives. Leave it empty to have the bot
look it up via `/whoami` at startup.

## Commands

| Command | Effect |
| --- | --- |
| `!search [artist\|album\|track\|playlist] <query>` | search the library; results are numbered |
| `!list` | show the last search results again |
| `!play <n> [n ...]` | queue results by number |
| `!play <query>` | search and queue the best match |
| `!queue` | show the queue |
| `!nowplaying` | show the current track and elapsed time |
| `!pause` / `!resume` | pause or resume |
| `!next` / `!prev` | move through the queue |
| `!skip <n>` | jump to queue position n |
| `!clear` | drop everything after the current track |
| `!stop` | stop and empty the queue |

Queuing an artist, album or playlist expands it into its tracks. Search results
expire after `player.result_ttl`, so a stale number cannot play the wrong thing
much later.

`!stop`, `!clear` and `!skip` are limited to `matrix.admins`. Leave that list
empty to let everyone use them.

## Testing

```sh
go test ./...          # includes a real ffmpeg -> Opus -> publisher run
go test -race ./...
```

The player tests generate tones with ffmpeg and push them through the entire
audio pipeline, so they catch pipeline breakage without needing a Matrix
homeserver or a LiveKit server.

## Limitations

- No media E2EE, hence the unencrypted-room requirement above.
- Uses the session-style (`org.matrix.msc3401.call.member`) membership event and
  the legacy `/sfu/get` token endpoint. Both are what current Element Call
  deployments use; the newer sticky-event membership (MSC4354) and `/get_token`
  are not implemented.
- One room per bot process.
