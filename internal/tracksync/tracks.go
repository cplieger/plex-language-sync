// Package tracksync holds the per-episode track-synchronization
// orchestrator. Named for the capability, not "sync": the old name shadowed
// the standard library, forcing a stdsync alias inside it and a syncpkg
// alias at every consumer.
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
//   - Plex HTTP URL paths and query parameters — this package never
//     constructs URLs directly; it calls through plexReader /
//     plexWriter, so the concrete plex.Client's verbatim path
//     strings remain the single source of truth (inviolate item 1/9).
//   - WARN / ERROR slog keys ("failed to set audio stream", "failed to
//     set subtitle stream", "failed to disable subtitles", "language
//     update complete", "new/updated episode language set", "failed to
//     fetch episodes for update", "failed to fetch show episodes for
//     reference") are byte-for-byte identical to the pre-extraction
//     log lines (inviolate item 5).
//
// Consumer note: tracksync depends on plexReader, plexWriter,
// cacheStore, and userLookup (not on the concrete internal/plex,
// internal/cache, or internal/users types). This keeps the package
// trivially testable with in-memory fakes.
package tracksync

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v2"
)

// Config captures the subset of application configuration the Syncer
// actually reads. Decoupling from the full main.config keeps the
// package boundary clean and lets tests construct a Syncer without
// mimicking the app's full env-var surface.
type Config struct {
	Ignore         episodeSkipper // library/label skip rules; nil means "never skip"
	UpdateLevel    string         // "show" (default) or "season"
	UpdateStrategy string         // "all" (default) or "next"
	// SubtitleFloor is the furthest language distance a subtitle substitution
	// may reach. Audio is not configurable and is fixed at streams.AudioFloor.
	SubtitleFloor    langtag.Tier
	LanguageProfiles bool // enable learn/apply language profiles
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

// Syncer owns the per-episode orchestration. Construct via New in
// the composition root; *Syncer is safe for concurrent use because all
// mutation goes through cacheStore (which is itself safe for concurrent
// use) and the Plex clients handled below are concurrency-safe (net/http
// transport + method-local state).
type Syncer struct {
	plex       plexReader // admin-scoped reader
	cache      cacheStore
	users      userLookup
	userClient UserClientFunc
	cfg        Config
}

// Deps carries New's collaborators. A struct rather than a positional list:
// four of the five are interfaces this package declares, so a transposition
// among the ones with compatible method sets would type-check, and the
// composition root wires them exactly once. All four are required.
type Deps struct {
	// Plex reads sessions, metadata and history.
	Plex plexReader
	// Cache persists learned profiles and recorded intents.
	Cache cacheStore
	// Users resolves a play session's account to a known user.
	Users userLookup
	// UserClient returns the per-user write client for a username; the write
	// path is user-scoped because Plex records selection writes against the
	// requesting token.
	UserClient UserClientFunc
}

// New constructs a Syncer from cfg and deps. Fields stay unexported so
// composition only happens here.
func New(cfg Config, deps Deps) *Syncer {
	return &Syncer{
		cfg:        cfg,
		plex:       deps.Plex,
		cache:      deps.Cache,
		users:      deps.Users,
		userClient: deps.UserClient,
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
	userClient PlexReadWriter,
	userID string,
	reference *streams.Episode,
	trigger string,
) {
	username := s.users.Name(userID)
	ref := streams.Selected(reference)
	if ref.Audio == nil {
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
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, reference) {
		return
	}

	// Learn language profile from the user's active choice.
	s.learnProfileFromReference(userID, ref)

	// Record the per-show intent — the app's own durable record of this
	// user's choice, captured at the only moment it is attributable.
	// Recorded before propagation because the observation is valid
	// regardless of downstream write outcomes. (Unlike profile learning,
	// commentary/descriptive tracks ARE recorded: an intent is per-show,
	// not a cross-show generalization, so an atypical deliberate choice
	// for THIS show is exactly what it should remember.)
	if showKey := reference.GrandparentRatingKey; showKey != "" {
		s.cache.RecordIntent(userID, showKey,
			streams.NewIntent(ref, time.Now().Unix()))
	}

	s.propagate(ctx, userClient, username, reference, ref, trigger)
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
//
// It takes the userID and derives the client itself rather than accepting both.
// Accepting both let them disagree: nothing tied the passed client to the passed
// userID, so a caller could apply user A's recorded intent through user B's
// token — and because Plex records a selection write against the REQUESTING
// token's user, that writes the wrong account. Deriving the client here makes
// the mismatch unrepresentable. A user with no client is skipped, never
// silently retried through the admin token.
func (s *Syncer) ReconcileWithIntent(
	ctx context.Context,
	userID string,
	episode *streams.Episode,
	viewedAt int64,
	trigger string,
) {
	username := s.users.Name(userID)
	userClient := s.userClient(userID)
	if userClient == nil {
		slog.Warn("reconcile: no per-user client, skipping user",
			"user_id", userID, "user", username)
		return
	}
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
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, episode) {
		return
	}

	s.propagate(ctx, userClient, username, episode, intent.RefStreams(), trigger)
}

// propagate is the shared propagation core for both planes: it applies
// the given reference selection (live streams on the event plane, an
// intent's reconstructed streams on the reconcile plane) to the other
// episodes of the anchor's show or season, honoring UpdateLevel and
// UpdateStrategy relative to the anchor episode.
func (s *Syncer) propagate(
	ctx context.Context,
	userClient PlexReadWriter,
	username string,
	anchor *streams.Episode,
	ref streams.Pair,
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
		if s.UpdateEpisodeStreams(ctx, userClient, username, plex.RatingKey(ep.RatingKey), ref) {
			changes++
		}
	}

	if changes > 0 {
		slog.Info("language update complete",
			"trigger", trigger,
			"user", username,
			"show", anchor.GrandparentTitle,
			"reference", anchor.ShortName(),
			"audio", streams.Desc(ref.Audio),
			"subtitle", streams.Desc(ref.Subtitle),
			"episodes_updated", changes,
			"episodes_total", len(episodes))
	}
}

// UpdateEpisodeStreams applies reference audio/subtitle streams to a
// single episode using the provided per-user client. Returns true when
// any change was written.
//
// ratingKey is plex.RatingKey, not a string, and that closes the second half
// of this signature's hazard: username and ratingKey were adjacent and both
// strings, so a transposed pair compiled and looked up the episode whose
// rating key is a username. Every method on plexReader already takes the
// typed key, so the conversion belongs at the caller, where a wire-decoded
// Episode.RatingKey becomes one.
func (s *Syncer) UpdateEpisodeStreams(
	ctx context.Context,
	userClient PlexReadWriter,
	username string,
	ratingKey plex.RatingKey,
	ref streams.Pair,
) bool {
	full, err := userClient.Episode(ctx, ratingKey)
	if err != nil {
		slog.Warn("failed to reload episode", "key", ratingKey, "user", username, "error", err)
		return false
	}

	partID := streams.FirstPartID(full)
	if partID == 0 {
		return false
	}

	cur := streams.Selected(full)
	changed := false

	changed = s.applyAudioStream(ctx, userClient, username, full, partID, ref, cur.Audio) || changed
	changed = s.applySubtitleStream(ctx, userClient, username, full, partID, ref, cur.Subtitle) || changed
	return changed
}

func (s *Syncer) applyAudioStream(
	ctx context.Context,
	userClient plexWriter,
	username string,
	ep *streams.Episode,
	partID int,
	ref streams.Pair,
	curAudio *streams.Stream,
) bool {
	matched := streams.MatchAudio(ref.Audio, streams.Audio(ep))
	if matched == nil || (curAudio != nil && matched.ID == curAudio.ID) {
		return false
	}
	if err := userClient.SetAudioStream(ctx, plexapi.StreamSelection{PartID: partID, StreamID: int(matched.ID)}); err != nil {
		slog.Warn("failed to set audio stream",
			"episode", ep.ShortName(), "user", username, "error", err)
		return false
	}
	logSubstitution(ep, username, kindAudio, ref.Audio, matched)
	return true
}

func (s *Syncer) applySubtitleStream(
	ctx context.Context,
	userClient plexWriter,
	username string,
	ep *streams.Episode,
	partID int,
	ref streams.Pair,
	curSub *streams.Stream,
) bool {
	if streams.ShouldSkipSubtitleForCommentary(ref.Audio, streams.Audio(ep)) {
		return false
	}

	// Policy: "no subtitle means no subtitle." If the reference episode
	// has no subtitle selected, disable any subtitle currently selected
	// on the target. streams.MatchSubtitle will return nil for
	// ref.Subtitle==nil (see streams.SubtitleCriteria) so we never auto-
	// enable forced subs in the audio language — that would override the
	// user's explicit choice of "no subtitles".
	if ref.Subtitle == nil {
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

	matched := streams.MatchSubtitle(ref.Subtitle, streams.Subtitle(ep), s.cfg.SubtitleFloor)
	if matched == nil {
		// Reference has a subtitle selected but no matching sub on
		// target. Leave the target's current selection alone — we have
		// no way to infer the right target.
		return false
	}
	if curSub != nil && matched.ID == curSub.ID {
		return false
	}
	if err := userClient.SetSubtitleStream(ctx, plexapi.StreamSelection{PartID: partID, StreamID: int(matched.ID)}); err != nil {
		slog.Warn("failed to set subtitle stream",
			"episode", ep.ShortName(), "user", username, "error", err)
		return false
	}
	logSubstitution(ep, username, kindSubtitle, ref.Subtitle, matched)
	return true
}

// logSubstitution records a track change and the language distance it was
// accepted at, so a surprising choice is diagnosable from logs without
// reproducing it.
//
// Only a substitution a user might question reaches INFO. The audio path cannot
// exceed its own floor, and every tier within that floor names one language, so
// audio is always routine however it matched; logging it at INFO would emit a
// line per episode for a regional variant nobody would notice. On the subtitle
// path anything past same-language crossed a boundary worth recording, and
// carries the table's recorded justification when there was one.
func logSubstitution(ep *streams.Episode, username, kind string, ref, matched *streams.Stream) {
	tier := streams.MatchTier(ref, matched)
	if kind == kindAudio {
		slog.Debug("track language matched", logAttrs(ep, username, kind, ref, matched, tier)...)
		return
	}
	attrs := logAttrs(ep, username, kind, ref, matched, tier)
	if tier <= langtag.TierSameLanguage {
		slog.Debug("track language matched", attrs...)
		return
	}
	if reason, ok := langtag.Prefer(ref.Lang()).Reason(matched.Lang()); ok {
		attrs = append(attrs, "reason", reason)
	}
	slog.Info("track language substituted", attrs...)
}

// Track kinds as they appear in the match log.
const (
	kindAudio    = "audio"
	kindSubtitle = "subtitle"
)

// logAttrs builds the attribute list shared by both log levels.
func logAttrs(ep *streams.Episode, username, kind string, ref, matched *streams.Stream, tier langtag.Tier) []any {
	return []any{
		"episode", ep.ShortName(),
		"user", username,
		"kind", kind,
		"from", ref.LanguageCode,
		"to", matched.LanguageCode,
		"from_tag", ref.LanguageTag,
		"to_tag", matched.LanguageTag,
		"match_tier", tier.String(),
	}
}

// learnProfileFromReference records the user's active audio→subtitle
// pairing into the cache when language profiles are enabled and the
// audio has a language code.
//
// Placed after the exported methods of *Syncer to satisfy funcorder
// (ObserveAndPropagate is its only caller).
func (s *Syncer) learnProfileFromReference(userID string, ref streams.Pair) {
	if !s.cfg.LanguageProfiles || ref.Audio == nil || ref.Audio.LanguageCode == "" {
		return
	}
	// Do not learn language profiles from commentary/descriptive tracks.
	// These tracks have atypical subtitle pairings that should not be
	// generalized to other shows.
	if streams.ContainsDescriptive(strings.ToLower(ref.Audio.TitleForMatch())) {
		return
	}
	subLang := ""
	if ref.Subtitle != nil {
		subLang = ref.Subtitle.LanguageCode
	}
	s.cache.LearnLanguageProfile(userID, streams.LanguageChoice{Audio: ref.Audio.LanguageCode, Subtitle: subLang})
}

// filterEpisodesAfter returns the subset of episodes strictly after the
// reference episode's (season, index) pair.
func filterEpisodesAfter(episodes []streams.Episode, ref *streams.Episode) []streams.Episode {
	refSeason := ref.SeasonNum()
	refEp := ref.Num()
	var out []streams.Episode
	for i := range episodes {
		ep := &episodes[i]
		sNum := ep.SeasonNum()
		eNum := ep.Num()
		if sNum > refSeason || (sNum == refSeason && eNum > refEp) {
			out = append(out, *ep)
		}
	}
	return out
}
