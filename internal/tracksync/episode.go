package tracksync

import (
	"context"
	"log/slog"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// SeedEpisode puts a choice on ep, a new or updated episode as the admin
// reader returned it, for every user through that user's own client: their
// own choice that governs ep, else the newest other user's choice that governs
// ep for its owner, else their learned profile. The applier leaves alone an
// episode the user played since the app's last decision there. Callers apply
// the ignore policy first, which saves a second show-metadata read here. With
// Config.SeedNewEpisodes off it does nothing.
func (s *Syncer) SeedEpisode(ctx context.Context, ep *streams.Episode, trigger string) {
	if !s.cfg.SeedNewEpisodes {
		return
	}
	for _, u := range s.users.All() {
		if ctx.Err() != nil {
			return
		}
		client := s.userClient(u.ID)
		if client == nil {
			slog.Warn("sync: no per-user client, skipping user", "user_id", textsafe.SanitizeSingleLine(u.ID))
			continue
		}
		if err := s.seedForUser(ctx, client, u.ID, ep, trigger); err != nil {
			return
		}
	}
}

// SeedsNewEpisodes reports whether SeedEpisode writes anything, so a caller
// can skip the reads that would only feed it. SeedEpisode enforces the switch
// itself; no caller decides a write from this.
func (s *Syncer) SeedsNewEpisodes() bool { return s.cfg.SeedNewEpisodes }

// seedForUser applies userID's seed plan to ep under the user's show lock. The
// only error is ctx's.
func (s *Syncer) seedForUser(ctx context.Context, client PlexReadWriter, userID string, ep *streams.Episode, trigger string) error {
	unlock, err := s.apply.lock(ctx, userID, ep.GrandparentRatingKey)
	if err != nil {
		return err
	}
	defer unlock()

	username := s.users.Name(userID)
	p, src := s.seedPlan(userID, ep)
	if p == nil {
		slog.Debug("no choice to seed new episode", "episode", ep.ShortName(), "user", username)
		return nil
	}
	p.targets = []streams.Episode{*ep}
	written, err := s.apply.applyLocked(ctx, client, userID, p)
	if written > 0 {
		src.log(trigger, username, ep)
	}
	return err
}

// seedSource is the tier a seed plan came from, for its log line.
type seedSource struct {
	choice   streams.Pair
	from     string // the household tier's source user, by name
	audio    string // the profile tier's audio and subtitle language
	subtitle string
	tier     streams.Source
}

func (src *seedSource) log(trigger, username string, ep *streams.Episode) {
	if src.tier == streams.SourceProfile {
		slog.Info("language profile applied to new show",
			"trigger", trigger, "user", username, "episode", ep.ShortName(),
			"audio_lang", textsafe.SanitizeSingleLine(src.audio),
			"subtitle_lang", textsafe.SanitizeSingleLine(src.subtitle))
		return
	}
	source, logger := "intent", slog.Default()
	if src.tier == streams.SourceHousehold {
		source, logger = "household", logger.With("from", src.from)
	}
	logger.Info("new/updated episode language set",
		"trigger", trigger, "user", username, "episode", ep.ShortName(), "source", source,
		"audio", streams.Desc(src.choice.Audio), "subtitle", streams.Desc(src.choice.Subtitle))
}

// seedPlan is userID's plan for ep from the first tier that has one, or nil.
// The caller holds the user's show lock.
func (s *Syncer) seedPlan(userID string, ep *streams.Episode) (*plan, seedSource) {
	floor, strategy := s.cfg.SubtitleFloor, s.cfg.UpdateStrategy
	if key, in, ok := s.intentFor(userID, ep); ok && InScope(key, strategy, in.Anchor, ep) {
		ref := in.RefStreams()
		return &plan{
			pick: pickForIntent(ref, floor), ref: ref,
			basis:      streams.Basis{Version: in.ObservedAt, Source: streams.SourceIntent},
			observedAt: in.ObservedAt,
		}, seedSource{choice: ref, tier: streams.SourceIntent}
	}

	covers := coversKey(ep)
	in, key, from, ok := s.cache.LatestIntentFor(userID, func(owner, key string, in streams.Intent) bool {
		if !covers(key) {
			return false
		}
		governing, _, ok := s.intentFor(owner, ep)
		return ok && governing.String() == key && InScope(governing, strategy, in.Anchor, ep)
	})
	if ok {
		fromName := s.users.Name(from)
		slog.Debug("seeding from another user's choice", "episode", ep.ShortName(),
			"from", fromName, "key", textsafe.SanitizeSingleLine(key))
		ref := in.RefStreams()
		return &plan{
			pick: pickForIntent(ref, floor), ref: ref,
			basis: streams.Basis{From: from, Version: in.ObservedAt, Source: streams.SourceHousehold},
		}, seedSource{choice: ref, from: fromName, tier: streams.SourceHousehold}
	}

	if s.cfg.LanguageProfiles {
		return s.profilePlan(userID, ep)
	}
	return nil, seedSource{}
}
