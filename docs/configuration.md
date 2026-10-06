# Configuration

This page explains the settings of plex-language-sync in more depth than the README table. It covers how subtitle languages are matched, how to leave shows out, how to connect to a Plex server with its own certificate, and what the app keeps in `/config`. Read it when the quick start works and you want to change how it behaves.

## Where settings live

Every setting is an environment variable, set in `.env` or in the `environment:` list of `compose.yaml`. The app reads them once at start, so restart the container after a change. There is no settings file and no web page. The [README's configuration reference](../README.md#configuration-reference) lists every variable with its default.

A misspelled value for `UPDATE_LEVEL`, `UPDATE_STRATEGY`, `SUBTITLE_MATCH_TIER`, `DEEP_SCAN_INTERVAL` or `LOG_LEVEL` logs a warning and falls back to the default, so the app still starts. A negative `DEEP_SCAN_INTERVAL` is treated the same way. Set `DEEP_SCAN_INTERVAL` to `off`, `disabled` or a zero duration such as `0` or `0s` to turn the deep scan off. `LOG_LEVEL` ignores case, accepts `warning` for `warn`, and accepts an offset such as `info+2`. A missing or blank `PLEX_URL` or `PLEX_TOKEN` stops the start with an error that names the variable.

## Keeping the token and address in files

Set `PLEX_URL_FILE` and `PLEX_TOKEN_FILE` to the path of a file inside the container, instead of `PLEX_URL` and `PLEX_TOKEN`, to read each value from [a Docker secret](https://github.com/cplieger/docs/blob/main/docs/hardening.md#secrets-in-files). One trailing line ending is removed, and so is any space around the value.

## Leaving shows and libraries out

Add a label to a show in Plex to leave it out. By default the labels are `PAL_IGNORE` and `PLS_IGNORE`. `IGNORE_LABELS` takes a comma-separated list. Setting it replaces both defaults, so list them again if you still want them. `IGNORE_LIBRARIES` takes a comma-separated list of library names to leave out entirely. Both match the exact name, with upper and lower case counted.

A show you leave out is never changed, and plex-language-sync does not learn your language habits from it either.

## How far a subtitle match may reach

Media files disagree about how to name a language. One episode carries a subtitle Plex labels "Norwegian Bokmål" and reports as `nob`. The next carries one labelled "Norwegian" and reports as `nor`. Both name the same thing, but comparing the codes as text finds no match, so the second episode would be left alone.

plex-language-sync reads language codes with [`langtag`](https://github.com/cplieger/langtag), which brings the several published spellings of one language to one form and grades how close a candidate track is to the one you chose. `SUBTITLE_MATCH_TIER` sets how far a subtitle substitution may reach when no exact match exists:

| Value | Accepts | Example |
| --- | --- | --- |
| `identical` | Only the same language, written another way | `ger` and `deu` |
| `same-language` (default) | One language a reader takes in without effort | `nob` and `nor`, `es-ES` and `es-419`, Serbian in either script |
| `other-script` | One language in another script | Simplified and Traditional Chinese |
| `intelligible` | A different but close language | Bokmål and Nynorsk, Norwegian and Danish |
| `shared-literacy` | A different language its readers are usually schooled in | Catalan and Spanish |

The default never gives you a different language. It covers the Norwegian case above and leaves every debatable substitution off.

The default does allow one script difference, for Serbian, which is written in both Latin and Cyrillic and whose readers are taught both. That is the only pair the Unicode locale data rates as close, at 5 against the 50 it gives a script substitution it does not vouch for.

Each tier past the default costs something specific. `other-script` asks you to read Traditional Chinese when you chose Simplified. The Unicode locale data scores that kind of substitution at 50, where every close-language pair it carries scores between 4 and 20. `intelligible` puts Danish subtitles on a Norwegian selection when no Norwegian track exists. `shared-literacy` puts Spanish on a Catalan one.

Audio has no setting and never switches to another language. It accepts a regional variant, so audio tagged `zh-CN` still matches `zh-TW`, but Danish never plays for a Norwegian selection. Subtitles you can read past or turn off, while wrong audio is the thing you notice.

A subtitle set in another script or another language is logged at INFO with how close the match was, so a surprising one can be traced. Closer matches, and every audio match, log `track language matched` at DEBUG. An INFO line looks like this:

```text
level=INFO msg="track language substituted" kind=subtitle from=nob to=nno match_tier=intelligible
```

## Connecting to Plex over https

Pick the case that matches your `PLEX_URL`:

| Your `PLEX_URL` looks like | What to do |
| --- | --- |
| `http://192.0.2.10:32400` | Nothing. The connection is not encrypted, so keep it on your local network. |
| `https://<hash>.plex.direct:32400`, Plex's own certificate | Nothing. Its certificate is trusted by default. |
| `https://192.0.2.10:32400` or `https://plex.local` with a self-signed or private certificate | Mount the PEM file of the certificate authority that signed it, and set `PLEX_CA_CERT_PATH` to its path inside the container. |

With `PLEX_CA_CERT_PATH` set, the app still checks the certificate, and trusts only that authority. A certificate it cannot verify stops the start with an error.

A plain `http://` address to another machine works, but the app logs a warning at start, because your Plex token then crosses the network unencrypted.

## What is kept in `/config`

| File | Holds |
| --- | --- |
| `profiles.json` | The language habits learned per user, and the choice each user made for each show. This is the state you would lose. |
| `tokens.json` | The shared users' Plex tokens, encrypted, so a restart works while plex.tv is unreachable |
| `state.json` | Which changes were already made recently, so one event is not handled twice |
| `.plex-language-sync-last-run` | When the last deep scan finished, so a restart keeps the daily schedule |

Each file is written to a temporary file first and then renamed, so a crash never leaves half a file. A damaged file resets only its own contents, and the others load normally. A single `cache.json` from an older layout is split into these files on the first start.

## Stopping the container

On stop, plex-language-sync gives its background work up to 10 seconds to finish, then saves the files above and exits. Docker's default stop timeout is also 10 seconds, which leaves no time for the save, so the shipped `compose.yaml` sets `stop_grace_period: 20s`. On Unraid, set the Docker stop time-out to 20 seconds or more. If a save is cut short, the learned habits and per-show choices come back as you watch.
