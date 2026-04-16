# Changelog

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
