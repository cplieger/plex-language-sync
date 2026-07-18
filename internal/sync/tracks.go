// Package sync holds the per-episode track-synchronization orchestrator.
//
// The package is organized around two planes:
//
//   - EVENT PLANE (ObserveAndPropagate): a resolved play session is the
//     only moment a selection read is attributable to a user. It learns
//     profiles, records per-show intents, and propagates the observed
//     selection. This is the only plane that creates knowledge.
//   - RECONCILE PLANE (ReconcileWithIntent): the scheduler's history
//     replay re-applies RECORDED intents. It never derives a user's
//     choice from a delayed metadata read (whose per-user attribution
//     is unreliable after the fact), never learns, never records.
//
// Additional responsibilities:
//   - Seed a new/updated episode for all users
//     (ProcessNewOrUpdatedEpisodeAllUsers): per-user intent first, then
//     a lazily-searched shared reference episode, then the learned
//     language profile (ApplyLanguageProfile).
//
// Inviolate contracts preserved (see refactor-agent-guide.md):
//   - Plex HTTP URL paths and query parameters — the sync package never
//     constructs URLs directly; it calls through api.PlexReader /
//     api.PlexWriter, so the concrete plex.Client's verbatim path
//     strings remain the single source of truth (inviolate item 1/9).
//   - WARN / ERROR slog keys ("failed to set audio stream", "failed to
//     set subtitle stream", "failed to disable subtitles", "language
//     update complete", "new/updated episode language set", "failed to
//     fetch episodes for update", "failed to fetch show episodes for
//     reference") are byte-for-byte identical to the pre-extraction
//     log lines (inviolate item 5).
//
// Consumer note: sync depends on api.PlexReader, api.PlexWriter,
// api.Cache, and api.UserLookup (not on the concrete internal/plex,
// internal/cache, or internal/users types). This keeps the package
// trivially testable with in-memory fakes.
package sync

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Config captures the subset of application configuration the Syncer
// actually reads. Decoupling from the full main.config keeps the
// package boundary clean and lets tests construct a Syncer without
// mimicking the app's full env-var surface.
type Config struct {
	Ignore           api.IgnoreChecker // library/label skip rules; nil means "never skip"
	UpdateLevel      string            // "show" (default) or "season"
	UpdateStrategy   string            // "all" (default) or "next"
	LanguageProfiles bool              // enable learn/apply language profiles
}

// UPDATE_LEVEL accepted values. Shared with the main/config package
// which parses the env var into one of these.
const (
	LevelShow   = "show"
	LevelSeason = "season"
)

// UPDATE_STRATEGY accepted values.
const (
	StrategyAll  = "all"
	StrategyNext = "next"
)

// Syncer owns the per-episode orchestration. Construct via NewSyncer in
// the composition root; *Syncer is safe for concurrent use because all
// mutation goes through api.Cache (which is itself safe for concurrent
// use) and the Plex clients handled below are concurrency-safe (net/http
// transport + method-local state).
type Syncer struct {
	plex       api.PlexReader // admin-scoped reader
	cache      api.Cache
	users      api.UserLookup
	userClient api.UserClientFunc
	cfg        Config
}

// NewSyncer constructs a Syncer with the given collaborators. Callers
// must supply a non-nil PlexReader, Cache, UserLookup, and
// UserClientFunc; fields are intentionally unexported so composition
// only happens here.
func NewSyncer(cfg Config, reader api.PlexReader, c api.Cache, lookup api.UserLookup, userClient api.UserClientFunc) *Syncer {
	return &Syncer{
		cfg:        cfg,
		plex:       reader,
		cache:      c,
		users:      lookup,
		userClient: userClient,
	}
}

// ObserveAndPropagate is the EVENT-PLANE entry point: it handles a
// deliberately observed selection (a resolved play session, sampled
// within seconds of the user's action — the only moment a server read
// is attributable to a user). It learns the user's language profile,
// records the per-show intent, and propagates the observed selection
// across the show (or season).
//
// This is the ONLY path that creates knowledge (profiles, intents).
// The reconcile plane (ReconcileWithIntent) and the new-episode seeding
// path only re-apply what was recorded here.
func (s *Syncer) ObserveAndPropagate(
	ctx context.Context,
	userClient api.PlexReadWriter,
	userID string,
	reference *streams.Episode,
	trigger string,
) {
	username := s.users.Name(userID)
	refAudio, refSub := streams.Selected(reference)
	if refAudio == nil {
		slog.Debug("no audio stream selected on reference, skipping",
			"episode", reference.ShortName(), "user", username)
		return
	}

	// Check ignore rules first (admin client — labels/libraries are
	// server-level). An ignored show is treated as if it does not exist:
	// we return before learning a language profile from it, before
	// recording an intent, AND before propagating to other episodes.
	// Learning/recording must come after this gate so an ignored show
	// never contributes to the user's global profile or intent ledger.
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, s.plex, reference) {
		return
	}

	// Learn language profile from the user's active choice.
	s.learnProfileFromReference(userID, refAudio, refSub)

	// Record the per-show intent — the app's own durable record of this
	// user's choice, captured at the only moment it is attributable.
	// Recorded before propagation because the observation is valid
	// regardless of downstream write outcomes. (Unlike profile learning,
	// commentary/descriptive tracks ARE recorded: an intent is per-show,
	// not a cross-show generalization, so an atypical deliberate choice
	// for THIS show is exactly what it should remember.)
	if showKey := reference.GrandparentRatingKey; showKey != "" {
		s.cache.RecordIntent(userID, showKey,
			streams.NewIntent(refAudio, refSub, time.Now().Unix()))
	}

	s.propagate(ctx, userClient, username, reference, refAudio, refSub, trigger)
}

// ReconcileWithIntent is the RECONCILE-PLANE entry point (scheduler
// history replay): it re-applies the user's RECORDED intent for the
// episode's show, and deliberately never derives the user's choice from
// the episode's current selection state. A delayed metadata read joins
// a historical identity to an ambient current selection whose per-user
// attribution is unreliable — the fabrication this design retires.
//
// viewedAt is the replayed play's unix timestamp (0 when unknown). A
// play NEWER than the recorded intent means the app provably missed an
// interaction it never observed; applying the older intent could revert
// a manual change the user made during that unobserved window, so the
// item is skipped. The user's next play re-establishes the intent via
// the event plane.
//
// No intent recorded for the show → skip: the safety net only replays
// knowledge, it never invents it.
func (s *Syncer) ReconcileWithIntent(
	ctx context.Context,
	userClient api.PlexReadWriter,
	userID string,
	episode *streams.Episode,
	viewedAt int64,
	trigger string,
) {
	username := s.users.Name(userID)
	showKey := episode.GrandparentRatingKey
	if showKey == "" {
		slog.Debug("no show rating key, skipping",
			"episode", episode.ShortName(), "user", username)
		return
	}
	intent, ok := s.cache.IntentFor(userID, showKey)
	if !ok {
		slog.Debug("reconcile: no intent recorded for show, skipping",
			"episode", episode.ShortName(), "user", username)
		return
	}
	if viewedAt > intent.ObservedAt {
		slog.Debug("reconcile: play newer than recorded intent, skipping "+
			"(unobserved interaction; will not re-apply stale state)",
			"episode", episode.ShortName(), "user", username,
			"viewed_at", viewedAt, "observed_at", intent.ObservedAt)
		return
	}
	// Ignore gate after the cheap ledger checks (it costs a show-metadata
	// fetch) but before any write, preserving "an ignored show is never
	// propagated to".
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, s.plex, episode) {
		return
	}

	refAudio, refSub := intent.RefStreams()
	s.propagate(ctx, userClient, username, episode, refAudio, refSub, trigger)
}

// propagate is the shared propagation core for both planes: it applies
// the given reference selection (live streams on the event plane, an
// intent's reconstructed streams on the reconcile plane) to the other
// episodes of the anchor's show or season, honoring UpdateLevel and
// UpdateStrategy relative to the anchor episode.
func (s *Syncer) propagate(
	ctx context.Context,
	userClient api.PlexReadWriter,
	username string,
	anchor *streams.Episode,
	refAudio, refSub *streams.Stream,
	trigger string,
) {
	showRatingKey := anchor.GrandparentRatingKey
	if showRatingKey == "" {
		slog.Debug("no show rating key, skipping",
			"episode", anchor.ShortName(), "user", username)
		return
	}

	// Get episodes to update using the user's client.
	var episodes []streams.Episode
	var err error
	if s.cfg.UpdateLevel == LevelSeason {
		episodes, err = userClient.SeasonEpisodes(ctx, plex.RatingKey(anchor.ParentRatingKey))
	} else {
		episodes, err = userClient.ShowEpisodes(ctx, plex.RatingKey(showRatingKey))
	}
	if err != nil {
		slog.Warn("failed to fetch episodes for update",
			"show", anchor.GrandparentTitle, "user", username, "error", err)
		return
	}

	// Filter by strategy.
	if s.cfg.UpdateStrategy == StrategyNext {
		episodes = filterEpisodesAfter(episodes, anchor)
	}

	changes := 0
	for i := range episodes {
		if ctx.Err() != nil {
			break
		}
		ep := &episodes[i]
		if s.UpdateEpisodeStreams(ctx, userClient, username, ep.RatingKey, refAudio, refSub) {
			changes++
		}
	}

	if changes > 0 {
		slog.Info("language update complete",
			"trigger", trigger,
			"user", username,
			"show", anchor.GrandparentTitle,
			"reference", anchor.ShortName(),
			"audio", streams.Desc(refAudio),
			"subtitle", streams.Desc(refSub),
			"episodes_updated", changes,
			"episodes_total", len(episodes))
	}
}

// UpdateEpisodeStreams applies reference audio/subtitle streams to a
// single episode using the provided per-user client. Returns true when
// any change was written.
func (s *Syncer) UpdateEpisodeStreams(
	ctx context.Context,
	userClient api.PlexReadWriter,
	username, ratingKey string,
	refAudio, refSub *streams.Stream,
) bool {
	full, err := userClient.Episode(ctx, plex.RatingKey(ratingKey))
	if err != nil {
		slog.Warn("failed to reload episode", "key", ratingKey, "user", username, "error", err)
		return false
	}

	partID := streams.FirstPartID(full)
	if partID == 0 {
		return false
	}

	curAudio, curSub := streams.Selected(full)
	changed := false

	changed = s.applyAudioStream(ctx, userClient, username, full, partID, refAudio, curAudio) || changed
	changed = s.applySubtitleStream(ctx, userClient, username, full, partID, refAudio, refSub, curSub) || changed
	return changed
}

func (s *Syncer) applyAudioStream(
	ctx context.Context,
	userClient api.PlexWriter,
	username string,
	ep *streams.Episode,
	partID int,
	ref, cur *streams.Stream,
) bool {
	matched := streams.MatchAudio(ref, streams.Audio(ep))
	if matched == nil || (cur != nil && matched.ID == cur.ID) {
		return false
	}
	if err := userClient.SetAudioStream(ctx, partID, matched.ID); err != nil {
		slog.Warn("failed to set audio stream",
			"episode", ep.ShortName(), "user", username, "error", err)
		return false
	}
	return true
}

func (s *Syncer) applySubtitleStream(
	ctx context.Context,
	userClient api.PlexWriter,
	username string,
	ep *streams.Episode,
	partID int,
	refAudio, refSub, curSub *streams.Stream,
) bool {
	if streams.ShouldSkipSubtitleForCommentary(refAudio, streams.Audio(ep)) {
		return false
	}

	// Policy: "no subtitle means no subtitle." If the reference episode
	// has no subtitle selected, disable any subtitle currently selected
	// on the target. streams.MatchSubtitle will return nil for
	// refSub==nil (see streams.SubtitleCriteria) so we never auto-
	// enable forced subs in the audio language — that would override the
	// user's explicit choice of "no subtitles".
	if refSub == nil {
		if curSub == nil {
			return false
		}
		if err := userClient.DisableSubtitles(ctx, partID); err != nil {
			slog.Warn("failed to disable subtitles",
				"episode", ep.ShortName(), "user", username, "error", err)
			return false
		}
		return true
	}

	matched := streams.MatchSubtitle(refSub, refAudio, streams.Subtitle(ep))
	if matched == nil {
		// Reference has a subtitle selected but no matching sub on
		// target. Leave the target's current selection alone — we have
		// no way to infer the right target.
		return false
	}
	if curSub != nil && matched.ID == curSub.ID {
		return false
	}
	if err := userClient.SetSubtitleStream(ctx, partID, matched.ID); err != nil {
		slog.Warn("failed to set subtitle stream",
			"episode", ep.ShortName(), "user", username, "error", err)
		return false
	}
	return true
}

// learnProfileFromReference records the user's active audio→subtitle
// pairing into the cache when language profiles are enabled and the
// audio has a language code.
//
// Placed after the exported methods of *Syncer to satisfy funcorder
// (ObserveAndPropagate is its only caller).
func (s *Syncer) learnProfileFromReference(userID string, refAudio, refSub *streams.Stream) {
	if !s.cfg.LanguageProfiles || refAudio == nil || refAudio.LanguageCode == "" {
		return
	}
	// Do not learn language profiles from commentary/descriptive tracks.
	// These tracks have atypical subtitle pairings that should not be
	// generalized to other shows.
	if streams.ContainsDescriptive(strings.ToLower(refAudio.TitleForMatch())) {
		return
	}
	subLang := ""
	if refSub != nil {
		subLang = refSub.LanguageCode
	}
	s.cache.LearnLanguageProfile(userID, refAudio.LanguageCode, subLang)
}

// filterEpisodesAfter returns the subset of episodes strictly after the
// reference episode's (season, index) pair.
func filterEpisodesAfter(episodes []streams.Episode, ref *streams.Episode) []streams.Episode {
	refSeason := ref.SeasonNum()
	refEp := ref.EpisodeNum()
	var out []streams.Episode
	for i := range episodes {
		ep := &episodes[i]
		sNum := ep.SeasonNum()
		eNum := ep.EpisodeNum()
		if sNum > refSeason || (sNum == refSeason && eNum > refEp) {
			out = append(out, *ep)
		}
	}
	return out
}
