# docker-plex-language-sync

![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/docker-plex-language-sync)](https://github.com/cplieger/docker-plex-language-sync/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/plex-language-sync/size)](https://github.com/cplieger/docker-plex-language-sync/pkgs/container/plex-language-sync)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)

Automatic per-show audio and subtitle language sync for Plex TV shows

## Overview

A ground-up Go rewrite of
[Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages)
(and its actively maintained fork
[JourneyDocker/Plex-Auto-Languages](https://github.com/JourneyDocker/Plex-Auto-Languages)),
rebuilt for reliability, minimal dependencies, and distroless
deployment.

Watches your Plex TV show playback via WebSocket and automatically
propagates your audio and subtitle language choices to other
episodes in the same show. Like Netflix — set it once, enjoy the
rest of the series.

**Example use case:** You start watching *Squid Game* and select
Korean audio with English subtitles on the first episode. This
tool detects your choice and applies the same audio/subtitle
selection to every other episode in the show. When a new episode
of *Squid Game* is added by Sonarr, it gets Korean audio and
English subs before you even open Plex.

**Key features:**
- Real-time WebSocket listener for play and library scan events
- Per-show language propagation with scored stream matching
  (language, codec, channel layout, title, forced, hearing
  impaired, visual impaired, descriptive track filtering)
- Language profiles — learns your audio→subtitle preferences
  from playback and applies them to brand new shows that have
  no watch history yet
- Subtitle codec preference — when multiple subtitle tracks
  match the same language, prefers ASS over image-based (PGS)
  over plain text (SRT)
- Configurable scope: entire show or current season only
- Configurable range: all episodes or future episodes only
- Ignore specific shows via Plex labels or entire libraries
- Scheduled daily deep analysis as a safety net
- Persistent JSON cache survives container restarts
- Multi-user support — automatically fetches shared user tokens
  from plex.tv, each user gets independent language preferences
- Docker secrets support (`PLEX_TOKEN_FILE`)

This is a distroless, rootless container running on
`gcr.io/distroless/static` with no shell or package manager.
One direct Go dependency: `coder/websocket` for the Plex
notification stream.

### Comparison With Upstream

This is a complete rewrite — no code is shared with the upstream
projects. The architecture and dependency choices are fundamentally
different:

| | Original (RemiRigal) | Fork (JourneyDocker) | This Project |
|---|---|---|---|
| **Language** | Python 3.8+ | Python 3.8+ | Go 1.26 |
| **Dependencies** | PlexAPI, APScheduler, websocket-client, Apprise, PyYAML | PlexAPI, APScheduler, websocket-client, Apprise, PyYAML | 1 (coder/websocket) |
| **Base image** | python:3-slim (Debian) | python:3-slim (Debian) | distroless/static (no OS) |
| **Image size** | ~250 MB | ~250 MB | ~8 MB |
| **Image user** | root | root | nonroot (UID 65534) |
| **Config format** | YAML file + env vars | YAML file + env vars | Env vars only |
| **Notifications** | Apprise (Discord, Telegram, etc.) | Apprise (Discord, Telegram, etc.) | Structured slog (Loki/Grafana) |
| **Health check** | None | None | CLI probe (`/plex-language-sync health`) |
| **WebSocket reconnect** | PlexAPI AlertListener | PlexAPI AlertListener | Automatic with exponential backoff (1s→30s) |
| **Language profiles** | No | No | Yes — learns audio→subtitle pairs |
| **Subtitle codec preference** | No | No | Yes — ASS → image-based → SRT |
| **Activity trigger** | Yes (experimental) | Yes (experimental) | Removed — redundant with scan trigger + scheduler |
| **Maintenance** | Abandoned (2023) | Active | Active |

**Language profiles** is a feature unique to this project. The
upstream tools treat each show independently — if you start a new
anime, you have to manually set Japanese audio + English subs on
the first episode before the tool can propagate it. Language
profiles close this gap:

1. **Learning.** Every time you play an episode, the tool records
   your audio→subtitle language pair (e.g. Japanese audio →
   English subtitles). This is stored per user — each household
   member builds their own profile.
2. **Applying.** When a brand new show arrives (via Sonarr or
   manual import) and you have no watch history for it, the tool
   looks up the audio language of the first episode and checks
   your profile. If you've previously watched Japanese audio with
   English subs, the new show gets English subs automatically.
3. **Scope.** Profiles only apply to shows with zero watch
   history for that user. Once you've watched one episode of a
   show, per-show propagation takes over — your actual selection
   on that episode becomes the reference for all others.
4. **Last-write-wins.** The profile stores the most recent pair,
   not the most frequent. If you switch from English to French
   subs for Japanese audio, the next new anime gets French subs.

**Subtitle codec preference** also applies when language profiles
select a subtitle track. When multiple tracks match the target
language, the tool picks the best available format:

| Priority | Codecs | Rationale |
|----------|--------|-----------|
| 1 (best) | ASS, SSA | Styled text — preserves typesetting, signs, karaoke |
| 2 | PGS, VOBSUB, DVB | Image-based — source-provided, reliable sync |
| 3 | SRT, SUBRIP, WebVTT | Plain text — often Bazarr-sourced, may have sync issues |

This preference applies when the tool selects subtitles for new
shows via language profiles. For existing shows, per-episode
propagation matches the codec of your reference episode — if you
manually switched to SRT on episode 1, the rest of the show gets
SRT regardless of the global preference. This means you can
always override the codec choice: just change the subtitle track
during playback and the tool propagates your selection.

### Limitations

- **TV shows only.** Movies are not processed — they don't have
  the "propagate to next episode" concept.
- **No Apprise notifications.** The upstream versions support
  Discord/Telegram notifications via Apprise. This version uses
  structured logging (Go `slog`) instead, which is a better fit
  for observability stacks. Every language change, play event,
  profile update, and error is emitted as a structured log line
  with fields like `trigger`, `user`, `show`, `audio`, and
  `subtitle`. Pipe these to Loki via Alloy and build Grafana
  dashboards or alert rules on any field. For push notifications,
  set up a Grafana alert rule (e.g. alert on `"language update
  complete"` log lines filtered by user or show).
- **plex.tv dependency for multi-user.** Shared user tokens are
  fetched from `plex.tv/api/servers/.../shared_servers`. If
  plex.tv is unreachable, cached tokens are used. Single-user
  setups (no shared users) work entirely offline.


## Container Registries

This image is published to both GHCR and Docker Hub:

| Registry | Image |
|----------|-------|
| GHCR | `ghcr.io/cplieger/plex-language-sync` |
| Docker Hub | `docker.io/cplieger/plex-language-sync` |

```bash
# Pull from GHCR
docker pull ghcr.io/cplieger/plex-language-sync:latest

# Pull from Docker Hub
docker pull cplieger/plex-language-sync:latest
```

Both registries receive identical images and tags. Use whichever you prefer.

## Quick Start

```yaml
services:
  plex-language-sync:
    image: ghcr.io/cplieger/plex-language-sync:latest
    container_name: plex-language-sync
    restart: unless-stopped
    user: "1000:1000"  # match your host user
    mem_limit: 128m

    environment:
      TZ: "Europe/Paris"
      PLEX_URL: "http://plex:32400"  # full URL including scheme and port
      PLEX_TOKEN: "your-plex-token"  # admin token from Plex Web settings
      UPDATE_LEVEL: "show"  # show = entire show, season = current season only
      UPDATE_STRATEGY: "all"  # all = every episode, next = future episodes only
      TRIGGER_ON_PLAY: "true"
      TRIGGER_ON_SCAN: "true"
      LANGUAGE_PROFILES: "true"  # learn and apply audio→subtitle pairs for new shows
      SCHEDULER_ENABLE: "true"
      SCHEDULER_SCHEDULE_TIME: "02:00"

    volumes:
      - /opt/appdata/plex-language-sync:/config

    healthcheck:
      test:
        - CMD
        - /plex-language-sync
        - health
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 15s
```

## Deployment

1. Set `PLEX_URL` to the full URL of your Plex server
   (e.g. `http://192.0.2.100:32400` or `https://plex.local:32400`).
2. Set `PLEX_TOKEN` to a Plex authentication token belonging to the
   server administrator. See
   [Finding an authentication token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/).
3. The tool connects immediately, verifies the admin user, and
   starts listening for WebSocket events. Language changes begin
   within seconds of playback.
4. If your Plex server uses a self-signed TLS certificate, set
   `SKIP_TLS_VERIFICATION=true`.
5. To ignore specific shows, add the label `PLS_IGNORE` (or
   `PAL_IGNORE` for backward compatibility) to the show in Plex.
6. The `/config` volume stores a persistent cache (`cache.json`)
   containing processed episode tracking and learned language
   profiles. Back it up if you want to preserve your profiles
   across reinstalls.


## Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `TZ` | Container timezone | `Europe/Paris` | No |
| `PLEX_URL` | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`) | `http://plex:32400` | Yes |
| `PLEX_TOKEN` | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken. Also supports Docker secrets via `PLEX_TOKEN_FILE` | - | Yes |
| `UPDATE_LEVEL` | Scope of language propagation. `show` applies to all episodes in the show. `season` applies only to the current season (default `show`) | `show` | No |
| `UPDATE_STRATEGY` | Which episodes to update. `all` updates every episode in scope. `next` updates only episodes after the one being played (default `all`) | `all` | No |
| `TRIGGER_ON_PLAY` | React to playback events — when you play an episode, propagate its language settings (default `true`) | `true` | No |
| `TRIGGER_ON_SCAN` | React to library scan events — when new episodes are added, apply language settings from the show's history (default `true`) | `true` | No |
| `LANGUAGE_PROFILES` | Learn audio→subtitle language pairs from playback and apply them to brand new shows that have no watch history. For example, if you always watch Japanese audio with English subs, new anime shows will automatically get English subs (default `true`) | `true` | No |
| `SCHEDULER_ENABLE` | Run a daily deep analysis that processes recent play history and newly added episodes as a safety net for missed real-time events (default `true`) | `true` | No |
| `SCHEDULER_SCHEDULE_TIME` | Time of day (HH:MM, 24-hour) to run the daily deep analysis (default `02:00`) | `02:00` | No |


## Volumes

| Mount | Description |
|-------|-------------|
| `/config` | Persistent cache storage. Contains `cache.json` with processed episode tracking, learned language profiles, and scheduler state. Mount a named volume or host path to preserve data across container restarts. |


## Docker Healthcheck

The container includes a CLI health probe for distroless Docker
healthchecks.

The main process writes a marker file at `/tmp/.healthy` once the
initial Plex connection succeeds and the admin user is verified.
The `health` subcommand checks for this file — it requires no
shell, HTTP client, or open port.

**When it becomes unhealthy:**
- The initial connection to Plex fails (bad URL, invalid token)
- The admin user cannot be resolved from the Plex token

**WebSocket disconnects do not cause unhealthy status.** The tool
automatically reconnects with exponential backoff (1s→30s).

| Type | Command | Meaning |
|------|---------|---------|
| Docker | `/plex-language-sync health` | Exit 0 = connected to Plex and listening |


## Code Quality

| Metric | Value |
|--------|-------|
| [Test Coverage](https://go.dev/blog/cover) | 41.6% |
| Tests | 286 |
| [Cyclomatic Complexity](https://en.wikipedia.org/wiki/Cyclomatic_complexity) (avg) | 3.8 |
| [Cognitive Complexity](https://www.sonarsource.com/docs/CognitiveComplexity.pdf) (avg) | 3.6 |
| [Mutation Efficacy](https://en.wikipedia.org/wiki/Mutation_testing) | 89.8% (29 runs) |
| Test Framework | Property-based ([rapid](https://github.com/flyingmutant/rapid)) + table-driven |

Tests cover stream matching and scoring (audio/subtitle selection
with comprehensive input combinations), subtitle codec preference
ranking, language profile learning and application, episode
filtering, cache lifecycle with boundary tests, config loading and
validation (including Docker secrets via `_FILE` suffix), multi-user
token management, handler dispatch for play and scan events, and
XML parsing for Plex shared server responses. Property-based tests
verify scoring invariants and panic-freedom on arbitrary input.

Not tested: WebSocket connection management, HTTP API calls to
Plex, the main event loop, scheduler tick loop, and cache file
I/O — these are I/O-bound runtime paths that can't be
meaningfully unit tested, validated instead by Docker healthchecks
and structured logging in production.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Version | Source |
|------------|---------|--------|
| golang | `1.26-alpine` | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static-debian13 | `nonroot` | [Distroless](https://github.com/GoogleContainerTools/distroless) |

## Design Principles

- **Always up to date**: Base images, packages, and libraries are updated automatically via Renovate. Unlike many community Docker images that ship outdated or abandoned dependencies, these images receive continuous updates.
- **Minimal attack surface**: When possible, pure Go apps use `gcr.io/distroless/static:nonroot` (no shell, no package manager, runs as non-root). Apps requiring system packages use Alpine with the minimum necessary privileges.
- **Digest-pinned**: Every `FROM` instruction pins a SHA256 digest. All GitHub Actions are digest-pinned.
- **Multi-platform**: Built for `linux/amd64` and `linux/arm64`.
- **Healthchecks**: Every container includes a Docker healthcheck.
- **Provenance**: Build provenance is attested via GitHub Actions, verifiable with `gh attestation verify`.

## Credits

This is an original tool that builds upon [Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages).
- [Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages)
  by [@RemiRigal](https://github.com/RemiRigal) — the original
  Python project that pioneered per-show language automation for
  Plex. The stream matching algorithm and event-driven architecture
  in this rewrite are directly inspired by the original design.
- [Plex-Auto-Languages](https://github.com/JourneyDocker/Plex-Auto-Languages)
  by [@JourneyDocker](https://github.com/JourneyDocker) — the
  actively maintained fork that added improved stream scoring,
  visual impaired track handling, and memory management fixes
- [Plex Media Server API](https://developer.plex.tv/pms/) — the
  official API documentation
- [coder/websocket](https://github.com/coder/websocket) — Go
  WebSocket implementation

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
