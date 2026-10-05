# Contributing to plex-language-sync

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Change a log message or field that `alerts/logql.yaml` matches or names, such as `deep analysis completed`, with its rule and `docs/monitoring.md` in one commit. Only the Go side has tests, so a one-sided change passes and the alert breaks.
- Log each failure the app retries at WARN. Log ERROR only once the failure needs the operator, as the WebSocket reconnect loop does after repeated failures. A retried failure at ERROR can fire `PlexLanguageSyncErrorLog` for something that heals itself.
- Add a new ERROR failure to the list in `docs/monitoring.md` and to the `PlexLanguageSyncErrorLog` description in `alerts/logql.yaml`. An unlisted ERROR fires the alert without telling the operator what failed.
