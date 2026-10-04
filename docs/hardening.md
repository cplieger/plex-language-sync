# Security

This page covers how plex-language-sync handles your Plex token, a hardened compose setup, and what the image contains. Read it when you want to lock the container down further than the quick start does.

## Network exposure

plex-language-sync listens on no port, so there is nothing to expose or put behind a proxy. It connects out to your Plex server, and to plex.tv to fetch the shared users' tokens.

Leave it on a network that can reach the internet. On a Docker network created with `internal: true`, every plex.tv refresh fails while the app keeps working for the users it already knows, so a user you add or remove later is never picked up.

## Tokens

The admin's Plex token is never logged and never written to `/config`. The start-up log shows it only as `configured`. Set `PLEX_TOKEN_FILE` to read it from a Docker secret rather than the environment.

The shared users' tokens are stored in `/config/tokens.json`, encrypted with a key derived from the admin token, so the app can restart while plex.tv is unreachable. Protect the `/config` folder like any folder that holds credentials.

A plain `http://` Plex address to another machine sends the token unencrypted, and the app logs a warning at start when you use one. [Configuration](configuration.md#connecting-to-plex-over-https) shows how to use https with your own certificate.

## What the app checks

- Plex responses are read up to 10 MB, and full library listings up to 40 MB. WebSocket messages are read up to 1 MB.
- Plex item IDs must be numeric before they go into a request URL.
- https connections use TLS 1.2 or newer, and certificate checks are always on.
- Each file in `/config` is written to a temporary file and renamed, so a crash never leaves half a file.

## Hardened compose settings

The image runs without root and needs no extra privileges, so it also runs with a read-only root filesystem and no Linux capabilities. Add these lines to the service in `compose.yaml`:

```yaml
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - "/tmp:size=1m,mode=1777,noexec,nosuid,nodev"
```

The `tmpfs` line is needed with `read_only`, because the healthcheck marker lives in `/tmp`.

## What the image contains

The image holds one statically built Go program on the `gcr.io/distroless/static-debian13:nonroot` base, which has no shell and no package manager. It runs as the `nonroot` user, UID 65532, unless you set `user:`. The license text of every bundled component is under `/usr/share/licenses/`.

The Go toolchain that builds it and the base image are pinned by digest or version, and [Renovate](https://github.com/renovatebot/renovate) updates them and every Go module automatically.

Current scan results are on the repository's Security tab.
