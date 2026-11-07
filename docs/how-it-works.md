# How plex-language-sync works

This page explains how plex-language-sync decides which tracks to set, when it sets them, and for whom. Read it when a show did not follow your choice and you want to know why, or before you change a setting such as `UPDATE_STRATEGY`.

## Following your choice as you watch

plex-language-sync keeps a WebSocket connection open to your Plex server, so Plex tells it the moment playback starts or the library changes. When you play an episode, it reads the audio and subtitle tracks you have selected. It compares each one with the track it set on that episode, or would set there. When a track differs, it records the track as your choice and sets matching tracks on the other episodes. When no choice of yours covers the episode yet, both tracks you play become your choice.

`UPDATE_LEVEL` and `UPDATE_STRATEGY` set a choice's range. With `UPDATE_LEVEL=show` a choice covers the whole show, and with `season` it covers the season you played. `UPDATE_STRATEGY` decides whether every episode changes or only the episodes after the one you played. A change of `UPDATE_LEVEL` applies only to the choices you make afterwards. Where a saved show choice and a season choice both cover an episode, the newer one wins.

A new choice is set on every episode in its range, including an episode you changed by hand. On an episode with no track matching your choice, the app leaves the track Plex had selected, and playing that track keeps your choice. For example, a show you watch in Japanese stays in Japanese when you play an episode that has only an English dub. To change the choice, pick the new track on an episode that has a matching track.

The only thing it changes in Plex is which audio and subtitle track each episode has selected. Your account's language settings stay as they are.

It handles TV episodes only. Playing a movie changes nothing, and only libraries of TV shows are scanned.

If the connection drops, it reconnects on its own, waiting 1 second at first and up to 30 seconds between tries.

## Matching tracks across episodes

Episodes of one show rarely carry identical tracks, so plex-language-sync scores each track of the target episode against the one you chose. It compares the language, the codec, the channel layout and the track title, and whether the track is forced, for the hearing impaired, for the visually impaired, or descriptive.

When several subtitle tracks match the same language, it prefers styled ASS subtitles, then image-based subtitles such as PGS, then plain text such as SRT.

Languages are matched by how close they are rather than by exact code, as [Configuration](configuration.md#how-far-a-subtitle-match-may-reach) explains. Audio never switches to another language.

## New episodes

When Plex adds an episode, plex-language-sync sets its tracks for each user in this order of preference:

1. The choice that user made for the show or its season, when the episode is in that choice's range.
2. The most recent choice another user made whose range holds the episode.
3. The language habit learned for that user, when no recorded choice covers the episode.

It skips an episode for a user who played it after the app last set its tracks there, or who has played it at all when the app has never set its tracks there and the tracks would come from another user or the habit. A choice the user made after that play is still applied.

`TRIGGER_ON_SCAN=false` turns this off, both when Plex reports a new episode and in the daily deep scan. `TRIGGER_ON_PLAY=false` stops it reacting to playback.

## Learning your habits

With `LEARN_LANGUAGE_PROFILES` on, each choice you make while watching also records which subtitle language a user pairs with which audio language, such as English subtitles with Japanese audio. A user keeps one subtitle language per audio language, the one chosen most recently, or none when they watch that audio without subtitles. A new episode that no recorded choice covers gets its subtitle from that habit, based on the audio track Plex plays by default. The habit never changes the audio of a new episode. Shows you leave out with a label or a library name are never learned from.

## The daily deep scan

Once a day by default, set by `DEEP_SCAN_INTERVAL`, plex-language-sync goes over the time since its last finished pass, 24 hours at least and 30 days at most. Unless `TRIGGER_ON_SCAN=false`, it gives the episodes Plex added in that time their tracks, which covers an episode added while the app was stopped. For each episode watched in that time, it sets your recorded choice again across its range. That repairs an earlier change that failed or a file that was replaced. It only applies choices it recorded while you watched.

The scan never infers a choice from what Plex reports later. A later read cannot tell a track you picked from one the app set or the file's default. It skips an episode you played after both your latest choice and the app's last change there. This keeps a change you made while watching. It never sets the same choice twice on one file, so a track you change on an episode's page stays once the app has set it.

The scan keeps its schedule across restarts, and a restart runs it straight away only when the last one finished more than one interval ago. `DEEP_SCAN_INTERVAL=off`, `disabled` or a zero duration such as `0` turns it off, and the app then works from live events only.

## Several users

plex-language-sync reads the admin's account with `PLEX_TOKEN`, and fetches a token for each user the server is shared with from plex.tv. It refreshes that list while running, so a user you add or remove is picked up. Each user keeps their own choices, and each change is written with that user's own token, because Plex records a track choice per user. When it has no working token for a user, it skips the change rather than writing it under the admin.

To know who is watching, it matches each playback notification with the server's list of active sessions. Plex announces playback a few seconds before the session appears in that list, so a play is sometimes attributed on a later notification. A notification it cannot attribute to a user is skipped and not repaired later, because the deep scan only re-applies choices it recorded.

## Design

plex-language-sync is one Go program. Its only third-party runtime libraries are [`coder/websocket`](https://github.com/coder/websocket) and `golang.org/x/sync`. The rest are the project's own support modules. It has no settings file and no web page, and sends no messages of its own. It opens no network port unless `METRICS_LISTEN_ADDR` is set.

## The healthcheck

The Docker healthcheck reads a file in `/tmp`, and one timer in the app writes it. The WebSocket listener, the reconnect loop and the deep scan each record their progress, and the timer updates the file every 10 seconds from all three. A loop that goes quiet for longer than its limit turns the container unhealthy, while the other loops keep working. [Knowing the app is alive](monitoring.md#knowing-the-app-is-alive) gives each limit and what each check answers.
