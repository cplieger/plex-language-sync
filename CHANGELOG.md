# Changelog

## 2026.05.19-f357801 (2026-05-21)

### Added

- Add modular internal architecture with caching and sync logic
- Improve security, health signaling, and subtitle matching
- Add file-based healthcheck for distroless container
- Add file-based healthcheck for distroless containers

### Fixed

- Fsync before atomic rename in cache writer
- Resolve revive var-naming on Users.AllResult
- Resolve golangci-lint findings from cycle 1
- Remove stale //nolint:goconst directives in score.go
- Refactor health probe to enable unit testing

### Security

- Block in-container privilege escalation (security hardening)

### Changed

- Refactor(plex-language-sync): replace 5min read timeout with TCP keepalive + 1h backstop
- Refactor(plex-language-sync): cycle 1 structural improvements
- Update Dockerfile to include internal package directory
- Test(plex-language-sync): update test IP to RFC 5737 documentation range
- Move healthcheck to Dockerfile and standardize resource limits
- Refactor(plex-language-sync): replace magic strings with named constants
- Test(plex-language-sync): update test IP to RFC 5737 documentation range
- Hoist reference search out of per-user loop, harden scheduler and WebSocket

### Dependencies

- Update gcr.io/distroless/static-debian13:nonroot docker digest to 963fa6c (#300)
- Update golang:1.26-alpine docker digest to 91eda97
- Update third-party dependencies
- fix(deps): update module pgregory.net/rapid to v1.3.0 (#231)

## 2026.04.16-19b3cfe (2026-04-17)

### Dependencies

- Update golang:1.26-alpine docker digest to 27f8293 (#204)
- Update golang:1.26-alpine docker digest to f853308

## 2026.04.15-d28a27b (2026-04-16)

### Dependencies

- Update golang:1.26-alpine docker digest to 1fb7391 (#193)

## 2026.04.13-98ff0b3 (2026-04-13)

### Changed

- Refactor(plex-language-sync): improve error handling, logging, and code organization
- Update Go toolchain configuration

### Dependencies

- Update go to v1.26.2
- Update golang:1.26-alpine docker digest to c2a1f7b (#174)

## 2026.04.07-6860326 (2026-04-08)

### Changed

- Update Go toolchain configuration

### Dependencies

- Update go to v1.26.2
- Update golang:1.26-alpine docker digest to c2a1f7b (#174)

## 2026.04.01-a6d3653 (2026-04-01)

### Added

- Enhance security with TOCTOU protection for secret files
- Test(plex-language-sync): add test for next update strategy
- Test(plex-language-sync): add boundary test for cache prune threshold
- Remove activity trigger and improve subtitle codec selection
- Add nil check for empty response body
- Add nil check for response body before closing
- Add new app for syncing TV show language preferences

### Fixed

- Enforce minimum TLS version for secure connections

### Changed

- Refactor(plex-language-sync): improve code quality and resource handling

### Dependencies

- Update gcr.io/distroless/static-debian13:nonroot docker digest to e3f9456

## 2026.03.21-06dd556 (2026-03-22)

### Added

- Enhance security with TOCTOU protection for secret files

## 2026.03.17-89def15 (2026-03-17)

### Fixed

- Enforce minimum TLS version for secure connections

## 2026.03.15-29f56ba (2026-03-16)

### Dependencies

- Update gcr.io/distroless/static-debian13:nonroot docker digest to e3f9456

## 2026.03.14-ac5a23b (2026-03-14)

- Initial release
