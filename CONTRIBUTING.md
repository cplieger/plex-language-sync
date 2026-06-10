# Contributing to plex-language-sync

Thanks for helping improve plex-language-sync. This guide covers the
bits that are specific to this repo; org-wide defaults still apply.

## What this is

A single Go binary that listens to Plex over a WebSocket and
propagates your per-show audio/subtitle language choices to the rest
of a series. It ships as a ~8 MB distroless image with one non-cplieger
runtime dependency (`coder/websocket`). There is no HTTP server, no
config file, and no inbound listener — everything is configured through
environment variables (see the README's configuration reference).

## Architecture

The code is split between a composition root at the module root and
business logic under `internal/`.

- `main.go` — composition root. `run()` constructs the Plex client,
  cache, user manager, syncer, and scheduler, wires them together, and
  starts the WebSocket listener. `notifyAdapter` is the glue that gates
  events on `TRIGGER_ON_PLAY` / `TRIGGER_ON_SCAN` and forwards them to
  the syncer.
- `config.go` — env-var parsing, defaults, `_FILE`-suffix Docker-secret
  handling, and `HH:MM` validation.
- `internal/api/` — the cross-package interface spine (`PlexReadWriter`,
  `IgnoreChecker`, cache/user contracts). Concrete types in
  `internal/{plex,cache,users,...}` implement these; consumers
  (`internal/{sync,scheduler,notify}`) depend only on the interfaces.
  This keeps `main.go` the single wiring layer and lets tests substitute
  fakes without reaching into production packages. `internal/api` must
  not import its implementers — that would reintroduce the import cycle
  the spine exists to break.
- `internal/{plex,streams,cache,notify,users,sync,scheduler,ignore,timeutil}`
  — the focused business-logic packages (Plex HTTP client, stream
  matching/scoring, persistent cache, WebSocket listener, multi-user
  token management, propagation logic, daily scheduler, ignore policy,
  time parsing).

A daily scheduler runs a deep analysis as a safety net for missed
real-time events; the WebSocket listener reconnects with exponential
backoff. The persistent cache (`/config/cache.json`) is written via
atomic temp-file + rename.

## Frozen contracts

A few things are deliberately stable; change them only with intent, not
incidentally:

- The env-var contract (names, defaults, boolean parsing, `_FILE` secret
  handling, `HH:MM` parsing) in `config.go`.
- The on-disk cache path `/config/cache.json` (`cachePath` in `main.go`).

The in-memory representation behind these may evolve freely; the
external surface should not drift.

## Local development

Requires the Go toolchain matching `go.mod` (currently Go 1.26).

```sh
go build ./...
go test ./...
```

Tests live beside the code they test (standard Go layout) and mix
table-driven cases with property-based tests via
[`pgregory.net/rapid`](https://github.com/flyingmutant/rapid). Pure
logic (stream scoring, codec ranking, profile learning, episode
filtering, cache lifecycle, config parsing, backoff math) is unit- and
property-tested; the I/O-bound runtime paths (WebSocket connection
management, Plex HTTP calls, the main loop, scheduler tick loop, cache
file I/O) are intentionally not unit-tested and are validated in
production via the healthcheck and structured logging. When you add
logic, add it to a package that can be tested without live Plex.

## Linting and formatting

Linting is enforced by golangci-lint v2 using the repo's
`.golangci.yaml`. The CI workflow is synced from `cplieger/ci`, so match
it locally before pushing:

```sh
golangci-lint run
golangci-lint fmt
```

A few config choices worth knowing:

- Formatting is `gofumpt` with `extra-rules` (groups adjacent same-type
  params, forbids naked returns) plus `gci` import grouping
  (standard → third-party/local). `golangci-lint run` reports
  unformatted files as issues, so run `fmt` first.
- `sloglint` is `kv-only`: structured logs must use key/value pairs, not
  attribute helpers.
- The Plex token must never be logged or written to the cache — log it
  as `"configured"`, the way `logConfig` does.

## Plex API gotchas

These have bitten this project in production; keep them in mind when
touching `internal/plex`:

- Filter operators are single literal comparator chars: use
  `viewedAt>=12345`, never `>>=`. A double `>>` is silently ignored and
  Plex returns the full unfiltered history. Do not URL-encode operators
  either — Plex ignores encoded ones too.
- Some endpoints return an empty body instead of `{"MediaContainer":{}}`.
  Guard with `len(body) == 0` before unmarshalling.
- `/library/metadata/<key>` is not user-scoped: admin and user tokens
  return identical `Stream.selected` values, so re-fetching with a user
  token to "see their view" adds latency with no information gain.

## Commits and pull requests

Commits follow [Conventional Commits](https://www.conventionalcommits.org/);
git-cliff parses them to generate release notes, and the commit type
drives the version bump (`feat:`, `fix:`, `sec:`; `chore`/`docs`/etc.
produce no release). Write the subject as the changelog line a user
would read. Open an issue first for larger changes so the approach can
be discussed before implementation.

## Conduct & security

By participating you agree to the org-wide
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security vulnerabilities through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
