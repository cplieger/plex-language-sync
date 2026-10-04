# How plex-language-sync works

This page explains how plex-language-sync decides which tracks to set, when it sets them, and for whom. Read it when a show did not follow your choice and you want to know why, or before you change a setting such as `UPDATE_STRATEGY`.

## Following your choice as you watch

plex-language-sync keeps a WebSocket connection open to your Plex server, so Plex tells it the moment playback starts or the library changes. When you play an episode, it reads the audio and subtitle tracks you have selected, records them as your choice for that show, and sets matching tracks on the other episodes. `UPDATE_LEVEL` decides whether that means the whole show or the current season. `UPDATE_STRATEGY` decides whether every episode changes or only the episodes after the one you played.

The only thing it changes in Plex is which audio and subtitle track each episode has selected. Your account's language settings stay as they are.

It handles TV episodes only. Playing a movie changes nothing, and only libraries of TV shows are scanned.

If the connection drops, it reconnects on its own, waiting 1 second at first and up to 30 seconds between tries.

## Matching tracks across episodes

Episodes of one show rarely carry identical tracks, so plex-language-sync scores each track of the target episode against the one you chose. It compares the language, the codec, the channel layout and the track title, and whether the track is forced, for the hearing impaired, for the visually impaired, or descriptive.

When several subtitle tracks match the same language, it prefers styled ASS subtitles, then image-based subtitles such as PGS, then plain text such as SRT.

Languages are matched by how close they are rather than by exact code, as [Configuration](configuration.md#how-far-a-subtitle-match-may-reach) explains. Audio never switches to another language.

## New episodes

When Plex adds an episode, plex-language-sync sets its tracks for each user in this order of preference:

1. The choice that user last made for the show.
2. The tracks other viewers already settled on for the show.
3. The language habit learned for that user, for a show nobody has watched yet.

`TRIGGER_ON_SCAN=false` turns this off. `TRIGGER_ON_PLAY=false` stops it reacting to playback.

## Learning your habits

With `LEARN_LANGUAGE_PROFILES` on, each play also records which subtitle language a user pairs with which audio language, such as English subtitles with Japanese audio. A user keeps one subtitle language per audio language, the one chosen most recently, or none when they watch that audio without subtitles. A new show that nobody has played yet gets its subtitle from that habit, based on the audio track Plex plays by default. The habit never changes the audio of a new show. Shows you leave out with a label or a library name are never learned from.

## The daily deep scan

Once a day by default, set by `DEEP_SCAN_INTERVAL`, plex-language-sync goes over the time since its last finished pass, 24 hours at least and 30 days at most. It gives the episodes Plex added in that time their tracks, which covers an episode added while the app was stopped. For each episode watched in that time, it sets your recorded choice again on the other episodes of the show, for example where an earlier change failed or a file was replaced. It only applies choices it recorded while you watched.

The scan never infers a choice from what Plex reports later, because Plex cannot say afterwards which user selected a track. It also skips an episode you watched after your last recorded choice, so it never undoes a change you made by hand.

The scan keeps its schedule across restarts, and a restart runs it straight away only when the last one finished more than one interval ago. `DEEP_SCAN_INTERVAL=off`, `disabled` or a zero duration such as `0` turns it off, and the app then works from live events only.

## Several users

plex-language-sync reads the admin's account with `PLEX_TOKEN`, and fetches a token for each user the server is shared with from plex.tv. It refreshes that list while running, so a user you add or remove is picked up. Each user keeps their own choices, and each change is written with that user's own token, because Plex records a track choice per user. When it has no working token for a user, it skips the change rather than writing it under the admin.

To know who is watching, it matches each playback notification with the server's list of active sessions. Plex announces playback a few seconds before the session appears in that list, so a play is sometimes attributed on a later notification. A notification it cannot attribute to a user is skipped and not repaired later, because the deep scan only re-applies choices it recorded.

## Design

plex-language-sync is one Go program. Its only third-party runtime libraries are [`coder/websocket`](https://github.com/coder/websocket) and `golang.org/x/sync`. The rest are the project's own support modules. It has no settings file and no web page, sends no messages of its own, and opens no network port.
