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

Each rule's description in the file says what to check first when it fires. Notes on each rule:

- `PlexLanguageSyncDeepScanStalled` fires on silence. The other two rules need a line the app logged, so a stalled deep scan that logs nothing would go unnoticed while the WebSocket listener stays connected. A stalled scan means episodes Plex added during a missed real-time event get no tracks, and recorded choices are no longer applied again to recent history. The container stays healthy the whole time.
- The 26h window is one interval (24h by default) plus the time a pass takes. The schedule keeps its timing across restarts through the last-run record on `/config`, so two completions sit at most about 24h plus a pass's runtime apart. A container that restarts more often than the time left in its period never gets to run the pass, and this window catches that too.
- This rule cannot tell a stalled scan from a container that never started, a renamed container or a log pipeline that stopped shipping this stream.
- If you set `DEEP_SCAN_INTERVAL` to `off`, `disabled` or `0`, drop `PlexLanguageSyncDeepScanStalled`. The app then has no periodic pass, so the rule would fire forever. If you changed the interval, set the window to your interval plus 2 hours.
- `PlexLanguageSyncErrorLog` counts ERROR lines, which the app keeps for hard failures. A fatal setting is a bad `PLEX_TOKEN`, a URL that points at another server, a TLS certificate error or a `/config` folder the container cannot write. The app logs it and exits, so the container restarts in a loop. An unreachable Plex at start is not an ERROR. The container starts healthy and retries at WARN. The healthcheck ignores the WebSocket state, so this rule is what reports a container that is up and failing.
- `PlexLanguageSyncResolutionStalled` follows the app's own `user resolution stalled` line. The app finds the viewer by matching each playback notification with the server's active sessions. It skips an event it cannot attribute rather than save a language choice under the wrong user. With `cause=sessions_unreadable`, the session list could not be read for 20 events in a row. With `cause=all_clients_absent`, the list was read every time but 5 different client and item pairs were missing from it, and the `absent_pairs` field carries that count.
- Repetition alone never fires `PlexLanguageSyncResolutionStalled`, and a paused client never counts toward it. Three normal cases look like a missing session. Plex announces playback a few seconds before the session can be read. An idle web client keeps announcing a finished item while its tab stays open. A client whose session Plex removed keeps announcing it as paused. Individual skips log at DEBUG, and `user resolution recovered` follows once playback is attributed again.

Thresholds, windows and the `severity` labels are starting points. Change the `container` selector to the label your log collector sets, such as `job` or `service`. Route by whatever labels your Alertmanager uses.
