# plex-language-sync

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/size.json)](https://github.com/cplieger/plex-language-sync/pkgs/container/plex-language-sync)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/coverage.json)](https://github.com/cplieger/plex-language-sync/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/mutation.json)](https://github.com/cplieger/plex-language-sync/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13217/badge)](https://www.bestpractices.dev/projects/13217)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/plex-language-sync/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/plex-language-sync)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/plex-language-sync/releases)

Set your preferred audio and subtitle languages per show, and Plex applies them to every new episode automatically.

## What it does

Plex lets you choose which audio track and subtitle language to use when watching a show, but that choice only applies to the episode you're currently watching. If you start a series in Japanese audio with English subtitles, you have to set that manually on every single episode, and again when new episodes arrive.

plex-language-sync eliminates that friction. It watches your Plex playback in real time and automatically propagates your audio and subtitle language choices to every other episode in the same show. Set your preference once on any episode, and the rest of the series follows, the way Netflix does natively.

It also learns your habits. If you always watch anime in Japanese with English subtitles, brand new shows that arrive (via Sonarr or manual import) get those settings applied before you even press play.

**Key features:**

- Real-time WebSocket listener for play and library scan events
- Per-show language propagation with scored stream matching
  (language, codec, channel layout, title, forced, hearing
  impaired, visual impaired, descriptive track filtering)
- Language profiles: learns your audio→subtitle preferences
  from playback and applies them to brand new shows that have
  no watch history yet
- Subtitle codec preference: when multiple subtitle tracks
  match the same language, prefers ASS over image-based (PGS)
  over plain text (SRT)
- Configurable scope: entire show or current season only
- Configurable range: all episodes or future episodes only
- Ignore specific shows via Plex labels or entire libraries
- Scheduled daily deep scan as a safety net: re-applies
  the per-show selections recorded from your playback, so a
  missed real-time event is repaired without guessing your
  choice from the server's current state
- Persistent JSON cache survives container restarts
- Multi-user support: fetches shared user tokens from plex.tv
  automatically; each user gets independent language preferences
- Docker secrets support (`PLEX_TOKEN_FILE`)

### Why this design

- **Single binary, small footprint.** Written in Go; the only third-party runtime libraries are `coder/websocket` and `golang.org/x/sync` (the rest are the project's own support modules). No Python runtime, no YAML config files, no notification frameworks; just a distroless container that does one job well.
- **Rootless and minimal attack surface.** Runs as `nonroot` (UID 65532) on `gcr.io/distroless/static-debian13` with no shell, no package manager, and no inbound network listener. The only outbound connections are to your Plex server and plex.tv.

## Quick start

Images are published to both `ghcr.io/cplieger/plex-language-sync` and `docker.io/cplieger/plex-language-sync`; use whichever registry you prefer.

```yaml
services:
  plex-language-sync:
    image: ghcr.io/cplieger/plex-language-sync:latest
    container_name: plex-language-sync
    restart: unless-stopped
    stop_grace_period: 20s  # headroom for the final cache save (see Graceful shutdown)
    # Override with PUID/PGID in .env; defaults to 1000:1000.
    user: "${PUID:-1000}:${PGID:-1000}"  # must own the /config host dir

    environment:
      PLEX_URL: "http://plex:32400"  # full URL including scheme and port
      PLEX_TOKEN: "your-plex-token"  # admin token from Plex Web settings
      UPDATE_LEVEL: "show"  # show = entire show, season = current season only
      UPDATE_STRATEGY: "all"  # all = every episode, next = future episodes only

    volumes:
      - "/path/to/plex-language-sync/config:/config"
```

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `PLEX_URL` | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`) | - | Yes |
| `PLEX_TOKEN` | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken. Also supports Docker secrets via `PLEX_TOKEN_FILE` | - | Yes |
| `UPDATE_LEVEL` | Scope of language propagation. `show` applies to all episodes in the show. `season` applies only to the current season | `show` | No |
| `UPDATE_STRATEGY` | Which episodes to update. `all` updates every episode in scope. `next` updates only episodes after the one being played | `all` | No |
| `TRIGGER_ON_PLAY` | React to playback events: when you play an episode, propagate its language settings | `true` | No |
| `TRIGGER_ON_SCAN` | React to library scan events: when episodes are added or updated, apply each user's recorded selection for the show (falling back to the show's established selection, then to the user's learned profile) | `true` | No |
| `LEARN_LANGUAGE_PROFILES` | Learn audio→subtitle language pairs from playback and apply them to brand new shows that have no watch history | `true` | No |
| `SUBTITLE_MATCH_TIER` | How far a subtitle substitution may reach when no exact language match exists: `identical`, `same-language`, `other-script`, `intelligible`, or `shared-literacy` (see [Language matching](#language-matching)) | `same-language` | No |
| `DEEP_SCAN_INTERVAL` | Cadence of the daily deep-scan safety net, a Go duration (e.g. `24h`, `12h`). `off`/`disabled`/`0` disables it (the app then runs WebSocket-only). | `24h` | No |
| `PLEX_CA_CERT_PATH` | Path to a PEM file with the CA certificate that signed your Plex server's cert; TLS verification stays **on**, pinned to that CA. Needed only for self-signed or private-CA `https://` URLs (see [TLS / certificate setup](#tls--certificate-setup)) | unset | No |
| `IGNORE_LABELS` | Comma-separated Plex labels that exclude a show from language sync (a show carrying any listed label is skipped); label matching is exact and case-sensitive, and setting this replaces the built-in defaults | `PAL_IGNORE,PLS_IGNORE` | No |
| `IGNORE_LIBRARIES` | Comma-separated Plex library names to exclude from language sync entirely (exact, case-sensitive name match) | unset | No |
| `LOG_LEVEL` | Minimum log level: `debug`, `info`, `warn`/`warning`, or `error` (case-insensitive; slog offsets such as `info+2` work). `debug` surfaces the per-event skip reasons and stream-matching detail used for troubleshooting. An unrecognized value warns and falls back to `info` | `info` | No |

### Language matching

Media files disagree about how to name a language. One episode carries a subtitle Plex labels
"Norwegian Bokmål" and reports as `nob`; the next carries one labelled "Norwegian" and reports as
`nor`. Both name the same thing. Comparing the codes as text finds no match, so the second episode
would be left alone.

Language identifiers are canonicalized with [`langtag`](https://github.com/cplieger/langtag), which
collapses the several published spellings of one language onto one form, then grades how far a
candidate track sits from the one you chose. `SUBTITLE_MATCH_TIER` sets how far a substitution may
reach:

| Value | Accepts | Example |
| --- | --- | --- |
| `identical` | Only the same language, same spelling | `ger` and `deu` |
| `same-language` (default) | One language a reader takes in without effort | `nob` and `nor`, `es-ES` and `es-419`, Serbian in either script |
| `other-script` | One language in another script | Simplified and Traditional Chinese |
| `intelligible` | A different but close language | Bokmål and Nynorsk, Norwegian and Danish |
| `shared-literacy` | A different language its readers are generally schooled in | Catalan and Spanish |

The default never gives you a different language. It solves the case above and leaves everything
debatable switched off.

It does allow one script difference, for Serbian, which is written in both Latin and Cyrillic and
whose readers are taught both. That is the only pair the Unicode locale data explicitly rates as
close, at 5 against the 50 it gives a script substitution it does not vouch for, so it is a fact
about the data rather than a judgment.

Each tier past the default costs something specific. `other-script` asks you to read Traditional
Chinese when you chose Simplified, which is more work than its position suggests: the Unicode locale
data scores a generic script substitution at distance 50, where every close-language pair it carries
scores between 4 and 20. `intelligible` will put Danish subtitles on a Norwegian selection when no
Norwegian track exists. `shared-literacy` will put Spanish on a Catalan one.

**Audio is not configurable** and never substitutes another language. It accepts a regional variant,
so audio tagged `zh-CN` still matches `zh-TW`, but Danish never plays for a Norwegian selection.
Subtitles you can read past or switch off; wrong audio is the thing you notice.

An unrecognized value logs a warning and falls back to the default.

Every change is logged with the distance it matched at, so a surprising substitution is
diagnosable:

```text
level=INFO msg="track language substituted" kind=subtitle from=nob to=nno match_tier=intelligible
```

## TLS / certificate setup

Pick the configuration that matches your Plex server:

| Your `PLEX_URL` looks like | What to do |
| --- | --- |
| `http://plex:32400` (Docker network, LAN, etc.) | nothing; TLS isn't in use |
| `https://<hash>.plex.direct:32400` (Plex's official cert) | nothing; Let's Encrypt is trusted by default |
| `https://192.0.2.100:32400` or `https://plex.local` (self-signed / private CA) | set `PLEX_CA_CERT_PATH` to the PEM file of the CA that signed your Plex cert. Mount it into the container and point the env var at the in-container path. |

### Volumes

| Mount | Description |
| --- | --- |
| `/config` | Persistent cache (`profiles.json` learned language profiles, `tokens.json` encrypted shared-user tokens, `state.json` sync state). Mount a named volume or host path to preserve data across restarts. A corrupt file resets only its own section; a `cache.json` from an earlier version migrates automatically on first start. |

## Graceful shutdown

On `SIGTERM`/`SIGINT` the container drains its background loops for up
to 10 seconds, then writes the cache files under `/config` and exits.
Set a stop grace period comfortably longer than that drain window, for
example `stop_grace_period: 20s` on the compose service; Docker's
default of 10s leaves no headroom for the final save. A save truncated
by `SIGKILL` is recoverable: profiles and per-show selections are
re-learned from live playback as you watch.

## Alerting

plex-language-sync has no metrics endpoint; its operational state is in
its logs. Ship the container's logs to Loki (Grafana Alloy's Docker log
discovery does this with no configuration) and evaluate these rules with
[Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing
alerts deliver through your Alertmanager exactly like Prometheus metric
alerts.

```yaml
groups:
  - name: plex-language-sync
    rules:
      # The deadman. Both rules below fire on a record the app LOGGED, so a
      # wedged deep-scan loop logs nothing at all while the WebSocket plane
      # stays connected and healthy, and neither notices. This one fires on
      # silence instead, keyed on the reconcile plane's completion line.
      #
      # 50h, not 30h, and the size comes from the RESTART behaviour rather than
      # the interval. The startup pass runs only when the last completion is
      # already older than one interval; otherwise it starts a fresh interval
      # from boot. So a restart shortly before the deadline defers the next pass
      # by almost a full interval, and two consecutive completions can sit ~47h
      # apart with nothing wrong. 50h absorbs one such restart plus runtime. It
      # does NOT absorb repeated restarts inside the interval, which no
      # completion-only window can; a container that restart-loops needs its own
      # alert, not a wider window here.
      #
      # DROP this rule if you set DEEP_SCAN_INTERVAL to off/disabled/0, which
      # runs the app WebSocket-only with no periodic pass, so there is no
      # heartbeat to miss and the rule would fire forever.
      - alert: PlexLanguageSyncDeepScanStalled
        expr: |
          absent_over_time({container="plex-language-sync"} |= `deep analysis completed` [50h])
        for: 1h
        labels:
          severity: warning
        annotations:
          summary: "no plex-language-sync deep-analysis heartbeat in 50h"
          description: >
            The reconcile plane logs `deep analysis completed` at the end of
            every pass, and none has arrived in 50h (DEEP_SCAN_INTERVAL defaults
            to 24h). The usual cause is the periodic safety net wedged, so
            replayed history items stop being reconciled while the container
            stays healthy and connected. Check the container logs for the last
            `scheduled deep analysis starting` line and whether it finished. An
            absence rule cannot tell that apart from a container that never
            started or was renamed, or a log pipeline that stopped shipping this
            stream, so rule those out first.
      - alert: PlexLanguageSyncErrorLog
        expr: |
          sum by (container) (count_over_time(
            {container="plex-language-sync"} |= `level=ERROR` [15m]
          )) >= 3
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "plex-language-sync emitting repeated ERROR logs"
          description: >
            plex-language-sync logged 3 or more ERROR lines in 15m
            (sustained 5m). ERROR covers only hard failures: a fatal
            startup misconfiguration (a bad PLEX_TOKEN, a wrong-server
            URL, or a TLS/certificate error, which logs and exits, so
            the container crash-loops), a WebSocket connection that
            keeps failing to reconnect, and a failed cache save on
            shutdown. An unreachable Plex server at startup is
            transient: the container starts healthy in a degraded state
            and retries at WARN, so it never fires this alert. The
            healthcheck deliberately ignores WebSocket state (the
            listener auto-reconnects), so nothing else reports a
            container that is up and failing.

      - alert: PlexLanguageSyncResolutionStalled
        expr: |
          sum by (container) (count_over_time(
            {container="plex-language-sync"} |= `user resolution stalled` [30m]
          )) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "plex-language-sync is attributing no playback to any user"
          description: >
            plex-language-sync could not attribute 20 consecutive play
            events to a user. It identifies the viewer by joining each
            playback notification against the server's active sessions,
            and it skips an event it cannot attribute rather than
            writing a language choice under the wrong identity, so a
            stall means playback is being watched and nothing is
            propagating. Check that PLEX_TOKEN still has admin rights
            and that the active-session endpoint answers. Individual
            skips are expected and log at DEBUG: the server announces
            playback a few seconds before a session becomes queryable,
            and an idle web client re-announces a finished item for as
            long as its tab stays open. The app logs "user resolution
            recovered" once playback is attributed again.
```

The thresholds, windows, and `severity` labels are starting points;
adjust the `container` selector to your deployment and route by whatever
labels your Alertmanager uses.

## Healthcheck

The container includes a built-in CLI health probe (`/plex-language-sync health`) that checks for a marker file written at `/tmp/.healthy`. It requires no shell, HTTP client, or open port.

Startup distinguishes fatal from transient failures. A **fatal** misconfiguration (a bad `PLEX_TOKEN`, a wrong-server URL, or a TLS/certificate error) exits non-zero, so the container never goes healthy and the problem stays loud. A **transient** failure (Plex unreachable or still starting at boot) starts the container healthy in a degraded state and keeps retrying the initial connection with capped exponential backoff (1s→30s) instead of crash-looping; normal operation begins once Plex answers. WebSocket disconnects after the initial connection never cause unhealthy status either; the listener reconnects with the same backoff.

## Security

No inbound network listener; the only outbound connections are to your
Plex server and plex.tv. Runs as `nonroot` on a distroless base image
with no shell or package manager. The Plex token is never logged or
written to the cache; Docker secrets are supported via
`PLEX_TOKEN_FILE`. Shared user tokens are cached encrypted in
`tokens.json` for offline restart; still protect the `/config` volume.

Response bodies are capped at 10 MB and WebSocket messages at 1 MB.
Rating keys are validated as numeric before URL construction. Cache
writes are atomic (temp file + rename). TLS connections require TLS 1.2
or newer. Live scan results are on the repository's Security tab; the
one accepted static-analysis finding is the `/tmp/.healthy` healthcheck
marker, which is intentional (see [Healthcheck](#healthcheck)).

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| gcr.io/distroless/static-debian13:nonroot | [Distroless](https://github.com/GoogleContainerTools/distroless) |

## Credits

This is an original tool that builds upon [Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages).

- [Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages)
  by [@RemiRigal](https://github.com/RemiRigal): the original
  Python project that pioneered per-show language automation for
  Plex. The stream matching algorithm and event-driven architecture
  in this rewrite are directly inspired by the original design.
- [Plex-Auto-Languages](https://github.com/JourneyDocker/Plex-Auto-Languages)
  by [@JourneyDocker](https://github.com/JourneyDocker): the
  actively maintained fork that added improved stream scoring,
  visual impaired track handling, and memory management fixes
- [Plex Media Server API](https://developer.plex.tv/pms/): the
  official API documentation
- [coder/websocket](https://github.com/coder/websocket): Go
  WebSocket implementation

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
