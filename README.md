# plex-language-sync

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/plex-language-sync/size)](https://github.com/cplieger/plex-language-sync/pkgs/container/plex-language-sync)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/plex-language-sync)](https://goreportcard.com/report/github.com/cplieger/plex-language-sync)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/coverage.json)](https://github.com/cplieger/plex-language-sync/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/mutation.json)](https://github.com/cplieger/plex-language-sync/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13217/badge)](https://www.bestpractices.dev/projects/13217)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/plex-language-sync/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/plex-language-sync)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/plex-language-sync/releases)

Set your preferred audio and subtitle languages per show — Plex applies them to every new episode automatically.

## What it does

Plex lets you choose which audio track and subtitle language to use when watching a show — but that choice only applies to the episode you're currently watching. If you start a series in Japanese audio with English subtitles, you have to manually set that on every single episode, and again when new episodes arrive.

plex-language-sync eliminates that friction. It watches your Plex playback in real time and automatically propagates your audio and subtitle language choices to every other episode in the same show. Set your preference once on any episode, and the rest of the series follows — like Netflix does natively.

It also learns your habits. If you always watch anime in Japanese with English subtitles, brand new shows that arrive (via Sonarr or manual import) get those settings applied before you even press play.

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

### Why this design

- **Single binary, one dependency.** Written in Go with only one external library (`coder/websocket`). No Python runtime, no YAML config files, no notification frameworks — just a distroless container that does one job well.
- **Rootless and minimal attack surface.** Runs as `nonroot` (UID 65534) on `gcr.io/distroless/static` with no shell, no package manager, and no inbound network listener. The only outbound connections are to your Plex server and plex.tv.
- **Learns, not just copies.** Language profiles close the gap that upstream tools leave open: new shows get correct subtitles from day one, without requiring you to watch an episode first.
- **Resilient by default.** WebSocket reconnects with exponential backoff, a daily scheduler catches missed events, and a persistent cache survives restarts — so your preferences are never lost.

## Quick start

Images are published to both `ghcr.io/cplieger/plex-language-sync` and `docker.io/cplieger/plex-language-sync` — use whichever registry you prefer.

```yaml
services:
  plex-language-sync:
    image: ghcr.io/cplieger/plex-language-sync:latest
    container_name: plex-language-sync
    restart: unless-stopped
    user: "1000:1000"  # match your host user

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
```

## Configuration reference

### Environment variables

| Variable                  | Description                                                                                                                                                                                                                                                                                                                                                                                                                     | Default                 | Required |
|---------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------|----------|
| `TZ`                      | Container timezone                                                                                                                                                                                                                                                                                                                                                                                                              | `Europe/Paris`          | No       |
| `PLEX_URL`                | Full URL of your Plex Media Server including scheme and port (e.g. `http://192.0.2.100:32400`)                                                                                                                                                                                                                                                                                                                                  | `http://plex:32400`     | Yes      |
| `PLEX_TOKEN`              | Plex authentication token for the server administrator. Get it from Plex Web → Settings → XML view → myPlexAccessToken. Also supports Docker secrets via `PLEX_TOKEN_FILE`                                                                                                                                                                                                                                                      | -                       | Yes      |
| `UPDATE_LEVEL`            | Scope of language propagation. `show` applies to all episodes in the show. `season` applies only to the current season                                                                                                                                                                                                                                                                                                          | `show`                  | No       |
| `UPDATE_STRATEGY`         | Which episodes to update. `all` updates every episode in scope. `next` updates only episodes after the one being played                                                                                                                                                                                                                                                                                                         | `all`                   | No       |
| `TRIGGER_ON_PLAY`         | React to playback events — when you play an episode, propagate its language settings                                                                                                                                                                                                                                                                                                                                            | `true`                  | No       |
| `TRIGGER_ON_SCAN`         | React to library scan events — when new episodes are added, apply language settings from the show's history                                                                                                                                                                                                                                                                                                                     | `true`                  | No       |
| `LANGUAGE_PROFILES`       | Learn audio→subtitle language pairs from playback and apply them to brand new shows that have no watch history                                                                                                                                                                                                                                                                                                                  | `true`                  | No       |
| `SCHEDULER_ENABLE`        | Run a daily deep analysis that processes recent play history and newly added episodes as a safety net for missed real-time events                                                                                                                                                                                                                                                                                               | `true`                  | No       |
| `SCHEDULER_SCHEDULE_TIME` | Time of day (HH:MM, 24-hour) to run the daily deep analysis                                                                                                                                                                                                                                                                                                                                                                     | `02:00`                 | No       |
| `PLEX_CA_CERT_PATH`       | Path to a PEM file containing your Plex server's CA certificate. When set, that CA is added to the TLS RootCAs pool — TLS verification stays **on**, pinned to your CA. Required only when (a) your `PLEX_URL` uses `https://` and (b) the cert isn't trusted by the OS bundle (i.e. you signed it yourself or with a private CA). Plain `http://` URLs and Plex's official `*.plex.direct` HTTPS URLs need **no** TLS env var. | unset                   | No       |
| `IGNORE_LABELS`           | Comma-separated Plex labels that exclude a show from language sync (a show carrying any listed label is skipped)                                                                                                                                                                                                                                                                                                                | `PAL_IGNORE,PLS_IGNORE` | No       |
| `IGNORE_LIBRARIES`        | Comma-separated Plex library names to exclude from language sync entirely                                                                                                                                                                                                                                                                                                                                                       | unset                   | No       |

### TLS / certificate setup

Pick the configuration that matches your Plex server:

| Your `PLEX_URL` looks like                                                     | What to do                                                                                                                                                |
| ------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `http://plex:32400` (Docker network, LAN, etc.)                                | nothing — TLS isn't in use                                                                                                                                |
| `https://<hash>.plex.direct:32400` (Plex's official cert)                      | nothing — Let's Encrypt is trusted by default                                                                                                             |
| `https://192.0.2.100:32400` or `https://plex.local` (self-signed / private CA) | set `PLEX_CA_CERT_PATH` to the PEM file of the CA that signed your Plex cert. Mount it into the container and point the env var at the in-container path. |

### Volumes

| Mount     | Description                                                                                                                                                                                                    |
| --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/config` | Persistent cache storage. Contains `cache.json` with processed episode tracking, learned language profiles, and scheduler state. Mount a named volume or host path to preserve data across container restarts. |

## Graceful shutdown

On `SIGTERM`/`SIGINT` the container first drains its background loops
(user-token refresh + daily scheduler) for up to 10 seconds, then writes
`/config/cache.json` and exits. Give the container a stop grace period
comfortably longer than that drain window so the final save is never
truncated by `SIGKILL` — for example `stop_grace_period: 20s` on the
compose service (Docker's default is only 10s, which leaves no headroom
for the save once the drain budget is spent). A truncated save is
recoverable — the cache is rebuilt from Plex over time and the daily
deep-analysis scheduler re-learns profiles — but a clean save preserves
your latest preferences immediately.

## Healthcheck

The container includes a built-in CLI health probe (`/plex-language-sync health`) that checks for a marker file written at `/tmp/.healthy` once the initial Plex connection succeeds and the admin user is verified. It requires no shell, HTTP client, or open port. The probe reports unhealthy only if the initial connection to Plex fails or the admin user cannot be resolved — WebSocket disconnects do not cause unhealthy status because the tool automatically reconnects with exponential backoff (1s→30s).

## Security

**No vulnerabilities found.** All scans clean across all CI security tools.

| Tool                                                                | Result                              |
| ------------------------------------------------------------------- | ----------------------------------- |
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | No vulnerabilities in call graph    |
| [golangci-lint](https://golangci-lint.run/) (gosec, gocritic)       | 0 issues                            |
| [trivy](https://trivy.dev/)                                         | 0 vulnerabilities (distroless base) |
| [grype](https://github.com/anchore/grype)                           | 0 vulnerabilities                   |
| [gitleaks](https://github.com/gitleaks/gitleaks)                    | No secrets detected                 |
| [semgrep](https://semgrep.dev/)                                     | 2 info (false positives)            |
| [hadolint](https://github.com/hadolint/hadolint)                    | Clean                               |

No inbound network listener; connects outbound to Plex and
plex.tv only. Supports Docker secrets via `PLEX_TOKEN_FILE`.
The Plex token is never logged or written to the cache file.
Runs as `nonroot` on a distroless base image with no shell.

**Details for advanced users:** Response bodies capped at 10 MB
via `io.LimitReader`. WebSocket read limit 1 MB. Cache writes
use atomic temp-file + rename. Rating keys validated as numeric
before URL construction. Explicit `MinVersion: tls.VersionTLS12`
set on TLS config. Shared user tokens are cached in
`cache.json` for offline restart; protect the `/config` volume
accordingly. Semgrep flags the `/tmp/.healthy` marker and the
opt-in TLS skip (both intentional).

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency                       | Source                                                           |
| -------------------------------- | ---------------------------------------------------------------- |
| golang                           | [Go](https://hub.docker.com/_/golang)                            |
| gcr.io/distroless/static:nonroot | [Distroless](https://github.com/GoogleContainerTools/distroless) |

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

## Contributing

Issues and pull requests are welcome. Please open an issue first for
larger changes so the approach can be discussed before implementation.

## Disclaimer

These images are built with care and follow security best practices, but they are intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
