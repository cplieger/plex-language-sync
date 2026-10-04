# Monitoring and alerts

This page describes what plex-language-sync logs, how its healthcheck works, and the Loki alert rules it ships. Read it when you want to be told that the app has stopped doing its job.

## Log lines

plex-language-sync has no metrics endpoint and opens no port. Its state is in its log, written to standard error as `key=value` text in UTC.

- At start it logs `configuration loaded` with every setting, showing the token only as `configured`. Then come `connected to plex server` and `authenticated as admin user`.
- Each handled play logs `play event detected`, and each new or updated episode logs `library scan event detected`.
- A subtitle set in another script or another language logs `track language substituted` with the codes and the `match_tier`. Closer matches log `track language matched` at DEBUG.
- The daily deep scan logs `scheduled deep analysis starting` and `deep analysis completed`.
- When no playback can be attributed to any user for a sustained run, it logs `user resolution stalled` once at WARN with a `cause` field, then `user resolution recovered` when attribution works again.
- `LOG_LEVEL=debug` adds the reason each event was skipped and the detail of each track match.

ERROR lines are kept for four failures that need you. Two stop the start: a bad setting and a `/config` folder the app cannot write to. The other two are a WebSocket connection that keeps failing to reconnect and a cache save that failed on shutdown.

## Health

The image's `HEALTHCHECK` runs `/plex-language-sync health`, which checks a marker file at `/tmp/.healthy`. It needs no shell and no port. The app writes the marker once it has connected to Plex, or once it starts retrying an unreachable Plex, and removes it when it stops.

A setting Plex rejects makes the app exit before it writes the marker, so the container never turns healthy. That covers a wrong token, a URL that reaches another server and a certificate error. When Plex is only unreachable at start, the container turns healthy and keeps retrying, waiting from 1 second up to 30 seconds between tries. It therefore does not restart in a loop while Plex boots. A lost WebSocket connection never makes it unhealthy, because the listener reconnects on its own. That is why the alert rules below exist.

## Alerting

Ship the container's logs to Loki and load the rules in [`alerts/logql.yaml`](../alerts/logql.yaml) into [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships the logs with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert. The rules cover:

| Alert | Fires when | Severity |
| --- | --- | --- |
| `PlexLanguageSyncDeepScanStalled` | no `deep analysis completed` line in 26h, so the daily deep scan has stopped | warning |
| `PlexLanguageSyncErrorLog` | 3 or more ERROR lines in 15m, such as a token Plex rejects or a WebSocket that will not reconnect | warning |
| `PlexLanguageSyncResolutionStalled` | the app logs `user resolution stalled`, so no playback is being attributed to a user | warning |

Drop `PlexLanguageSyncDeepScanStalled` if you set `DEEP_SCAN_INTERVAL` to `off`, and set its window to your interval plus 2 hours if you changed the interval. Each rule's description in the file says what to check when it fires.

Thresholds, windows and the `severity` labels are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.
