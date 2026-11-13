# Contributing to plex-language-sync

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Change a log message or field that `alerts/logql.yaml` matches or names, such as `deep analysis completed`, with its rule and `docs/monitoring.md` in one commit. Go tests apply the line filters of `PlexLanguageSyncDeepScanStalled` and `PlexLanguageSyncResolutionStalled` to the lines the code writes, so changing one side alone fails them.
- Log each failure the app retries at WARN. Log ERROR only once the failure needs the operator, as the WebSocket reconnect loop does after repeated failures. A retried failure at ERROR can fire `PlexLanguageSyncErrorLog` for something that heals itself.
- Add a new ERROR failure to the list in `docs/monitoring.md` and to the `PlexLanguageSyncErrorLog` description in `alerts/logql.yaml`. An unlisted ERROR fires the alert without telling the operator what failed.
- Rename a metric or label with `alerts/promql.yaml`, `alerts/promql_test.yaml` and `docs/monitoring.md` in one commit. `TestPromQLRulesReadExportedSeries` catches a rule that reads a metric the app no longer exports, but no test checks label names.
