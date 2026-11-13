# Monitoring and alerts

This page describes the metrics, log lines and alert rules of plex-language-sync, and what its healthcheck proves. Read it when you want to be told that the app has stopped doing its job. plex-language-sync serves a metrics page when you turn it on, and ships metric alert rules for it. If you run Loki and no metric store, a log rule file gives you the deep scan and user attribution alerts, and one on the app's ERROR lines.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager | The five metric alerts, `up` and graphs | `METRICS_LISTEN_ADDR`, a scrape job named `plex-language-sync`, and [`alerts/promql.yaml`](../alerts/promql.yaml) on your ruler |
| Loki and Alertmanager, no metric store | The three log alerts | [`alerts/logql.yaml`](../alerts/logql.yaml) on Loki's ruler, with `LOG_LEVEL` at `info` |
| Grafana-managed alerting only | The same rules, imported into Grafana | A Prometheus or Loki data source, and the rules |
| Docker or Unraid only | Docker health, which notifies nobody on its own | An uptime prober on `/healthz`, or something that watches Docker health |

## Metrics

Set `METRICS_LISTEN_ADDR` to an address such as `:9471`, and the app serves `GET /metrics` and `GET /healthz` there. Unset, it opens no port. An address the app cannot listen on stops the container at start with `cannot bind the metrics listener`. Neither route has a login, so keep the port on your network. No label carries a user name, a show or a token, because every label takes its value from a fixed list.

| Series | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `plex_language_sync_websocket_connected` | gauge | none | 1 while the Plex notification stream is connected, else 0 |
| `plex_language_sync_websocket_reconnects_total` | counter | none | Reconnect attempts after the stream dropped or failed to connect |
| `plex_language_sync_deep_scan_last_success_timestamp_seconds` | gauge | none | Unix time the last complete deep scan finished, kept across restarts. 0 when none was ever recorded or the scan is off |
| `plex_language_sync_deep_scan_interval_seconds` | gauge | none | `DEEP_SCAN_INTERVAL` in seconds, 0 when the scan is off |
| `plex_language_sync_deep_scan_deadline_timestamp_seconds` | gauge | none | Unix time by which the next complete deep scan is due: the last complete one, kept across restarts, or else the start, plus two intervals and 2 hours. Only a complete scan moves it. 0 when the scan is off |
| `plex_language_sync_deep_scan_runs_total` | counter | `result`: `complete`, `incomplete` | Deep scans finished. An incomplete one stopped before it covered its window |
| `plex_language_sync_user_resolution_stalled` | gauge | `cause`: `sessions_unreadable`, `all_clients_absent` | 1 while no play can be attributed to a user, else 0 |
| `plex_language_sync_play_events_total` | counter | `outcome`: `attributed`, `skipped` | Play events handled, attributed to a user or skipped because no user could be found |
| `plex_language_sync_track_updates_total` | counter | `kind`: `audio`, `subtitle`. `result`: `set`, `failed` | Track selections written to Plex, one per request |
| `plex_language_sync_cache_save_failures_total` | counter | none | Failed saves of what the app learned to `/config` |
| `plex_language_sync_shared_users` | gauge | none | Shared Plex users the app follows, besides the admin |
| `plex_language_sync_build_info` | gauge | `version` | Always 1. `version` is the release the container runs |

The page also carries the Go runtime and process families (`go_*`, `process_*`) under the names Go's official Prometheus client uses, so stock Go dashboards work. Name the scrape job `plex-language-sync`, because `PlexLanguageSyncDown` selects on it. plex-language-sync bundles no dashboard. These queries answer the common questions in Grafana:

- `sum by (kind) (increase(plex_language_sync_track_updates_total{result="set"}[1d]))` shows how many tracks the app set in the last day.
- `(time() - plex_language_sync_deep_scan_last_success_timestamp_seconds) / 3600` shows the hours since the last complete deep scan.
- `increase(plex_language_sync_play_events_total{outcome="skipped"}[1d])` shows the plays of the last day that the app could not attribute to a user.

## Logs

The app writes `key=value` text lines to standard error, with times in UTC.

- At start it logs `configuration loaded` with every setting, showing the token only as `configured`. Then come `connected to plex server` and `authenticated as admin user`.
- Each play that sets or changes a choice logs `play event detected`, and each new or updated episode logs `library scan event detected`.
- A subtitle matched past the `other-region` tier logs `track language substituted` with the codes and the `match_tier`. Closer matches log `track language matched` at DEBUG.
- The daily deep scan logs `scheduled deep analysis starting` and `deep analysis completed`. The second line carries `complete=true` when the pass covered its whole window, and `unknown_users` with the number of Plex users in the history that the app does not follow.
- When no playback can be attributed to any user for a sustained run, it logs `user resolution stalled` once at WARN with a `cause` field. It logs `user resolution recovered` when attribution works again.
- When the WebSocket listener, the reconnect loop or the deep scan stops making progress, it logs `a loop stopped making progress` once at WARN. The `loop` field is `websocket`, `reconnect` or `deep_scan`, and `lease` is how long that loop may stay silent.
- With `METRICS_LISTEN_ADDR` set, the listener logs one `http` line per request: at DEBUG for `/metrics` and `/healthz`, so scrapes and probes stay out of the INFO log, and at INFO for any other path.
- `LOG_LEVEL=debug` adds the reason each event was skipped and the detail of each track match.

ERROR lines are kept for four failures that need you. A bad setting and a `/config` folder the app cannot write to stop the start. The other two are a WebSocket connection that keeps failing to reconnect and a cache save that failed on shutdown.

The rules in `alerts/logql.yaml` read `deep analysis completed` with `complete=true`, `user resolution stalled` and the `level=ERROR` lines. The deep scan line is INFO, so keep `LOG_LEVEL` at `info` or `debug`.

## Alerting

Load [`alerts/promql.yaml`](../alerts/promql.yaml) into your Prometheus-compatible ruler, or [`alerts/logql.yaml`](../alerts/logql.yaml) into Loki's ruler. Load one, not both, or each alert arrives twice under the same name. [Loading metric alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-metric-alert-rules) and [Loading an app's alert rules](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#loading-an-apps-alert-rules) show how.

`PlexLanguageSyncErrorLog` exists only in `alerts/logql.yaml`. In `alerts/promql.yaml`, `PlexLanguageSyncDown` and `PlexLanguageSyncWebSocketDown` cover its causes. `PlexLanguageSyncDown` and `PlexLanguageSyncCacheSaveFailed` exist only in `alerts/promql.yaml`, because a stopped process and a failed save during a deep scan leave no line a log rule reads.

| Alert | File | Fires when | Severity |
| --- | --- | --- | --- |
| `PlexLanguageSyncDown` | `promql.yaml` | the scrape of the `plex-language-sync` job failed, or the job has no target, for 5 minutes | warning |
| `PlexLanguageSyncDeepScanStalled` | both | no complete deep scan in two scan intervals plus 2 hours, 50 hours at the default | warning |
| `PlexLanguageSyncWebSocketDown` | `promql.yaml` | the Plex notification stream has been down for 10 minutes | warning |
| `PlexLanguageSyncResolutionStalled` | both | no playback is being attributed to any user | warning |
| `PlexLanguageSyncCacheSaveFailed` | `promql.yaml` | a save to `/config` failed in the last hour | warning |
| `PlexLanguageSyncErrorLog` | `logql.yaml` | 3 or more ERROR lines in 15 minutes, such as a token Plex rejects or a WebSocket that will not reconnect | warning |

Each rule's description in its file says what to check first when it fires. Notes on each rule:

- `PlexLanguageSyncDeepScanStalled` fires on silence. A stalled scan means episodes Plex added during a missed real-time event get no tracks, and recorded choices are no longer applied again to recent history.
- Only a complete pass counts. A pass that stops early, because Plex failed to answer or the circuit breaker tripped, logs `complete=false` and leaves its window to the next pass.
- The window is two intervals plus 2 hours, so one missed pass never fires it. A container that restarts during every pass never completes one, and the window catches that too.
- In `alerts/promql.yaml` the rule reads `DEEP_SCAN_INTERVAL` from the metrics, so it needs no editing and stays silent with the scan off. It keeps its clock across restarts, because the app reads the time of its last complete pass from `/config` at start.
- Until a first complete pass is recorded, the metric rule counts from the process start. So on a new install whose first pass never completes, a container that restarts more often than two intervals plus 2 hours keeps the metric rule silent. The log rule still fires.
- In `alerts/logql.yaml` the window is a fixed 50 hours, which fits the default `24h` interval. If you changed `DEEP_SCAN_INTERVAL`, set the window to two intervals plus 2 hours. With the scan `off`, `disabled` or `0`, drop the rule, because it would fire forever.
- The log rule cannot tell a stalled scan from a container that never started, a renamed container or a log pipeline that stopped shipping this stream.
- `PlexLanguageSyncErrorLog` counts ERROR lines. A fatal setting is a bad `PLEX_TOKEN`, a URL that points at another server, a TLS failure on an `https://` `PLEX_URL`, an unusable `METRICS_LISTEN_ADDR` or a `/config` folder the container cannot write. A Plex server error that repeats, such as 501, is fatal too. The app logs it and exits, so the container restarts in a loop. An unreachable Plex at start is not an ERROR, because the app retries at WARN. In `alerts/logql.yaml` this rule is what reports a container that is up and failing, and in `alerts/promql.yaml` `PlexLanguageSyncWebSocketDown` does.
- A cache save that failed on shutdown logs one ERROR, which alone does not fire `PlexLanguageSyncErrorLog`. `PlexLanguageSyncCacheSaveFailed` counts the saves that fail while the app runs.
- `PlexLanguageSyncResolutionStalled` follows the app's own stall. The app finds the viewer by matching each playback notification with the server's active sessions. It skips an event it cannot attribute rather than save a language choice under the wrong user. With `cause=sessions_unreadable`, the session list could not be read for 20 events in a row. With `cause=all_clients_absent`, the list was read every time but 5 different client and item pairs were missing from it, and the log's `absent_pairs` field carries that count.
- Repetition alone never fires `PlexLanguageSyncResolutionStalled`, and a paused client never counts toward it. Three normal cases look like a missing session. Plex announces playback a few seconds before the session can be read. An idle web client keeps announcing a finished item while its tab stays open. A client whose session Plex removed keeps announcing it as paused. Individual skips log at DEBUG.
- The metric rule holds until playback is attributed again. The log rule holds for 6 hours after the stall line and does not follow recovery, so check for a later `user resolution recovered` line.

Thresholds, windows and the `severity` labels are starting points. In `alerts/logql.yaml`, change the `container` selector to the label your log collector sets, such as `job` or `service`. In `alerts/promql.yaml`, keep the scrape job name that `PlexLanguageSyncDown` names.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `METRICS_LISTEN_ADDR`, unset by default | none | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |
| `/metrics` | `GET` | the same port | none. Keep the port on your network | Prometheus text format |

**What healthy means.** The app is healthy once it has reached Plex, or has started retrying a Plex that was unreachable at start, and until it begins to shut down. Each of its three loops must also show progress within its own limit:

- While connected, the WebSocket listener reads a message, or drops an idle connection and reconnects, at least every 2 hours. That is twice its one-hour read deadline.
- While it reconnects, the reconnect loop makes an attempt at least every 5 minutes 30 seconds.
- The deep scan starts or finishes a pass at least every two intervals plus 2 hours, 50 hours at the default. With the scan off, it is not checked.

A loop that misses its limit makes the app unhealthy, and `/healthz` names that loop in `reasons`, for example `the deep scan stopped making progress`. Plex being unreachable never makes the app unhealthy, because each reconnect attempt counts as progress. The outage shows on `/metrics` as `plex_language_sync_websocket_connected` at 0, and in the log.

**Docker health.** The image checks itself every 30 seconds with `/plex-language-sync health`, which reads the health file the app refreshes at `/dev/shm/plex-language-sync.healthy`. The app refreshes that file every 10 seconds while the checks above pass, and removes it when one fails. The check fails when the file is missing or older than 30 seconds. If that file cannot be written, the check passes and the log says so once, while `/healthz` on the metrics listener still gives the true answer.

Docker marks the container unhealthy after 3 failures, and allows 15 seconds after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container. `/healthz` answers from the app's memory and gives the same verdict as the image's check.

**Who is told.**

| You run | A dead app | A wedged app |
| --- | --- | --- |
| Prometheus with this app's rules | `PlexLanguageSyncDown` | `PlexLanguageSyncDeepScanStalled` for the deep scan, and `PlexLanguageSyncWebSocketDown` for a stuck reconnect loop. For a stuck WebSocket listener, an uptime prober on `/healthz` |
| Loki only | `PlexLanguageSyncErrorLog` for a restart loop, and `PlexLanguageSyncDeepScanStalled` once 50 hours pass with no complete deep scan | `PlexLanguageSyncDeepScanStalled` for the deep scan. For the WebSocket listener and the reconnect loop, something that watches Docker health |
| Docker or Unraid only | an uptime prober on `/healthz`, or something that watches Docker health | the same |

**Probing from outside.** Check `GET http://<host>:<port>/healthz` and pass on status 200 only. Do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 15 seconds after a first start. A Kubernetes liveness probe that must ignore readiness uses a `tcpSocket` probe on the port. With `METRICS_LISTEN_ADDR` unset, the app opens no port, so nothing outside the container can probe it. Watch Docker health, or set `METRICS_LISTEN_ADDR` and probe its `/healthz`.
