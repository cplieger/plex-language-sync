# plex-language-sync

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/size.json)](https://github.com/cplieger/plex-language-sync/pkgs/container/plex-language-sync) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/plex-language-sync/pkgs/container/plex-language-sync) [![base: Distroless](https://img.shields.io/badge/base-Distroless_nonroot-4285F4?logo=google)](https://github.com/cplieger/plex-language-sync/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/plex-language-sync/badges/mutation.json)](https://github.com/cplieger/plex-language-sync/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/plex-language-sync/releases)

<!-- hub-overview BEGIN -->
plex-language-sync remembers the audio and subtitle languages you pick for each TV show in Plex and sets them on every other episode, for each user of your server. It chooses between the tracks your files already have and downloads nothing.

## What it does

plex-language-sync keeps a show on the audio and subtitles you pick once, in four ways:

- Sets your choice on the rest of the show as soon as you pick it on one episode.
- Gives new episodes the same tracks when Plex adds them.
- Learns which subtitles you pair with each audio language, and sets them on new shows Plex adds.
- Keeps a separate choice for every user your server is shared with.

You can limit it to the current season or to later episodes, and leave out shows or libraries.

## Who it is for

plex-language-sync is built for Plex servers where people watch TV shows in more than one language, such as anime in Japanese with English subtitles, or a household with different preferences. It handles TV episodes only, never movies, and has no web page. Without it, you would pick the audio and subtitle track in the player on every episode.

You need a Plex Media Server and its owner's Plex token. To follow shared users, the container also needs internet access to reach plex.tv.

plex-language-sync is free software under the GPL-3.0-or-later license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  plex-language-sync:
    image: ghcr.io/cplieger/plex-language-sync:latest
    container_name: plex-language-sync
    restart: unless-stopped
    stop_grace_period: 20s  # time to save what it learned on stop. Docker's 10s default is too short
    # Run "sudo mkdir -p /opt/appdata/plex-language-sync && sudo chown 1000:1000 /opt/appdata/plex-language-sync"
    # before the first start, or nothing it learns is saved. If .env sets PUID and PGID, use those numbers.
    user: "${PUID:-1000}:${PGID:-1000}"

    environment:
      - PLEX_URL  # from .env, the address you open Plex at, such as http://192.0.2.10:32400
      - PLEX_TOKEN  # from .env, the server owner's Plex token, or set PLEX_TOKEN_FILE to a Docker secret
      - "UPDATE_LEVEL=show"  # show = whole show, season = current season only
      - "UPDATE_STRATEGY=all"  # all = every episode, next = only the episodes after the one you played

    volumes:
      - "/opt/appdata/plex-language-sync:/config"  # what it learns, the shared users' tokens and its schedule
```

1. Create the config folder and give it to the container user: `sudo mkdir -p /opt/appdata/plex-language-sync && sudo chown 1000:1000 /opt/appdata/plex-language-sync`. If your `.env` sets `PUID` and `PGID`, use those numbers instead of `1000`.
2. Find your Plex token with Plex's guide, [Finding an authentication token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/). Sign in as the server's owner.
3. Create a file named `.env` beside `compose.yaml` with these two lines:

   ```sh
   PLEX_URL=http://192.0.2.10:32400
   PLEX_TOKEN=your-plex-token
   ```

   `PLEX_URL` is the address you open Plex at from another device on your network, with `http://` and the port, not `localhost`.
4. Run `docker compose up -d`.

Run `docker logs plex-language-sync`. You should see `connected to plex server`. When you play an episode, the log shows `play event detected`. If you see `cannot establish initial plex connection`, Plex rejected the token or the address reaches a different server.

On Unraid, open the **Apps** tab, search for plex-language-sync and click **Install**. Enter your Plex URL and token. Under Settings, Docker, in the advanced view, set the Docker stop time-out to 20 seconds or more.

## Configuration reference

Settings are environment variables. The app reads them once at start, so restart the container after a change. An unrecognized value logs a warning and falls back to its default.

| Variable | Description | Default |
| --- | --- | --- |
| `PLEX_URL` | Address of your Plex server, with scheme and port, such as `http://192.0.2.10:32400`. `PLEX_URL_FILE` reads it from a file instead | required |
| `PLEX_TOKEN` | Plex token of the server's owner. `PLEX_TOKEN_FILE` reads it from a file instead | required |
| `UPDATE_LEVEL` | `show` changes the whole show, `season` only the current season | `show` |
| `UPDATE_STRATEGY` | `all` changes every episode in scope, `next` only the episodes after the one played | `all` |
| `TRIGGER_ON_PLAY` | Follow the tracks you choose while watching | `true` |
| `TRIGGER_ON_SCAN` | Set tracks on episodes Plex adds or updates | `true` |
| `LEARN_LANGUAGE_PROFILES` | Learn audio and subtitle pairs, and use them on new shows Plex adds | `true` |
| `SUBTITLE_MATCH_TIER` | How close a subtitle's language must be to your choice: `identical`, `same-language`, `other-script`, `intelligible` or `shared-literacy`. The default never switches language | `same-language` |
| `DEEP_SCAN_INTERVAL` | How often a scan of recent history repairs missed episodes, such as `24h` or `12h`. `off` turns it off | `24h` |
| `IGNORE_LABELS` | Comma-separated Plex labels that leave a show out. Setting it replaces the defaults | `PAL_IGNORE,PLS_IGNORE` |
| `IGNORE_LIBRARIES` | Comma-separated library names to leave out | _(unset)_ |
| `PLEX_CA_CERT_PATH` | Certificate authority file for an `https://` Plex address with a self-signed or private certificate | _(unset)_ |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. `debug` shows why each event was skipped | `info` |

Labels and library names match exactly, with case counted. Audio never switches to another language, whatever the tier. [Configuration](docs/configuration.md) explains each tier, https setup and the files kept in `/config`.

| Mount | Description |
| --- | --- |
| `/config` | Learned languages, the choice each user made per show, the shared users' encrypted tokens and the deep-scan schedule. Keep it across updates. |

plex-language-sync opens no port.

## Security

plex-language-sync listens on no port, and connects out only to your Plex server and plex.tv. It never logs your Plex token or writes it to `/config`. The shared users' tokens are kept encrypted in `/config/tokens.json`, so protect that folder. A plain `http://` address to another machine sends the token unencrypted, and the app logs a warning at start when you use one. The image runs as a non-root user on a distroless base, which has no shell. [Security](docs/hardening.md) has a hardened compose setup.

## Troubleshooting

The healthcheck checks a file the app writes once it reaches Plex, or once it starts retrying because Plex is unreachable. It retries every 1 to 30 seconds and does not restart while Plex boots. A setting Plex rejects stops the app before it writes the file, so the container restarts and never turns healthy. A lost connection later never makes it unhealthy, because the app reconnects on its own.

- The container restarts in a loop with `cannot establish initial plex connection`. This means Plex rejected the token, the address reaches another server, or an `https://` certificate failed. Check `.env`, or set `PLEX_CA_CERT_PATH` for your own certificate.
- The log says `required environment variable is missing or unreadable`. `PLEX_URL` or `PLEX_TOKEN` did not reach the container. Check `.env`, then restart.
- The log keeps saying `plex still unreachable; retrying`. Use Plex's network address, not `localhost`, and check that Plex is running.
- A user you shared the server with is not followed, and the log says `failed to refresh shared user tokens`. The container cannot reach plex.tv. Give it a network with internet access.
- The container restarts in a loop with `cache directory is not writable`, or the log says `cache save on shutdown failed`. The container user cannot write to `/config`. Run the `chown` from the quick start. The app checks this once it reaches Plex, before it loads anything from `/config`.
- The container restarts in a loop with `cache directory cannot hold owner-only files`. The storage under `/config` gives new files wider permissions than the app asks for, usually through an inherited ACL. Remove that ACL from the folder, because the cache keeps your users' Plex tokens.

## Monitoring

plex-language-sync writes text logs and has no metrics endpoint. Three Loki alert rules ship in [`alerts/logql.yaml`](alerts/logql.yaml). [Monitoring and alerts](docs/monitoring.md) lists them, describes the log lines, and shows how to load them.

## Documentation

- [How plex-language-sync works](docs/how-it-works.md) explains how tracks are matched and chosen, for anyone asking why a show did not follow.
- [Configuration](docs/configuration.md) covers subtitle matching tiers, leaving shows out, https and the files in `/config`.
- [Monitoring and alerts](docs/monitoring.md) covers the log lines, the healthcheck and the alert rules.
- [Security](docs/hardening.md) covers token handling, a hardened compose setup and what the image contains.

## Credits

- The way plex-language-sync matches tracks across episodes and reacts to Plex's events follows [Plex-Auto-Languages](https://github.com/RemiRigal/Plex-Auto-Languages) by [@RemiRigal](https://github.com/RemiRigal), the original per-show language project for Plex.
- Its stream scoring and its handling of tracks for the visually impaired follow the [JourneyDocker fork](https://github.com/JourneyDocker/Plex-Auto-Languages) of that project.
- It talks to Plex through the [Plex Media Server API](https://developer.plex.tv/pms/) and over [coder/websocket](https://github.com/coder/websocket).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE). The image carries the license text of
every bundled component under `/usr/share/licenses/`.
