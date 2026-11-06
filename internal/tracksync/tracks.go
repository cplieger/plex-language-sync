// Package tracksync records each user's track choices and puts them on the
// episodes they govern.
//
// Observe, on a resolved play, is the only code that records a choice or
// learns a profile: classify tells the user's pick from what the app or Plex
// left on the part. The history replay re-applies recorded choices, and new
// episodes are seeded from the user's own choice, another user's, or the
// learned profile. Every stream write goes through the applier, under the
// user's show lock, and leaves a record of what it put on each part.
//
// The package reaches Plex, the cache and the user list only through the
// interfaces in deps.go, so the concrete clients keep their own URLs and
// every test runs on in-memory fakes.
package tracksync

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
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
	// may reach. Audio is not configurable; the streams package fixes its floor.
	SubtitleFloor    langtag.Tier
	LanguageProfiles bool // enable learn/apply language profiles
	SeedNewEpisodes  bool // false makes SeedEpisode a no-op
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
	cache      cacheStore
	users      userLookup
	userClient userClientFunc
	apply      *applier
	cfg        Config
}

// Deps carries New's collaborators. A struct rather than a positional list:
// a transposition between collaborators with compatible method sets would
// type-check, and the composition root wires them exactly once. All three
// are required.
type Deps struct {
	// Cache persists learned profiles, recorded intents and applied records.
	Cache cacheStore
	// Users enumerates the users to fan out to and names them for logs.
	Users userLookup
	// UserClient returns the per-user read and write client for a userID;
	// Plex records selection writes against the requesting token, and the
	// user's own reads carry that user's selection and views.
	UserClient userClientFunc
}

// New constructs a Syncer from cfg and deps. Fields stay unexported so
// composition only happens here.
func New(cfg Config, deps Deps) *Syncer {
	return &Syncer{
		cfg:        cfg,
		cache:      deps.Cache,
		users:      deps.Users,
		userClient: deps.UserClient,
		apply:      newApplier(deps.Cache, deps.Users),
	}
}

// Observe classifies the user's play-time read of played against the
// user's records. Only a Choice records anything: it learns the profile,
// records the choice under the current level's key with played as the
// anchor, and applies it to every episode that record governs. The only
// error is ctx's, from the lock wait or the writes.
func (s *Syncer) Observe(ctx context.Context, userID string, played *streams.Episode, trigger string) error {
	username := s.users.Name(userID)
	if played.GrandparentRatingKey == "" {
		slog.Debug("no show rating key, skipping", "episode", played.ShortName(), "user", username)
		return nil
	}
	unlock, err := s.apply.lock(ctx, userID, played.GrandparentRatingKey)
	if err != nil {
		return err
	}
	defer unlock()

	key, in, hasIntent := s.intentFor(userID, played)
	rec, hasRec := s.cache.AppliedFor(userID, played.RatingKey)
	verdict, merged := classify(&classifyInput{
		Played: played, Intent: in, Applied: rec, Floor: s.cfg.SubtitleFloor,
		HasIntent: hasIntent, HasApplied: hasRec && rec.PartID == streams.FirstPartID(played),
	})
	switch verdict {
	case verdictNoAudio:
		slog.Debug("no audio stream selected on reference, skipping", "episode", played.ShortName(), "user", username)
		return nil
	case verdictRepeat, verdictEcho:
		slog.Debug("play keeps the recorded choice", "episode", played.ShortName(), "user", username,
			"verdict", verdict.String(), "key", key.String())
		return nil
	}
	// An ignored show learns and records nothing.
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, played) {
		return nil
	}
	s.learnProfileFromReference(userID, merged)
	slog.Info("play event detected", "episode", played.ShortName(), "user", username,
		"trigger", trigger, "verdict", verdict.String())

	// ObservedAt is the plan's basis version, so a second choice within one
	// second must still differ from the first, or appliedOnce skips its writes.
	now := s.apply.now().Unix()
	if hasIntent {
		now = max(now, in.ObservedAt+1)
	}
	key = scopeKeyFor(s.cfg.UpdateLevel, played)
	anchor := streams.AnchorOf(played, streams.Selected(played))
	s.cache.RecordIntent(userID, key.String(), streams.NewIntent(merged, anchor, now))

	client := s.userClient(userID)
	if client == nil {
		slog.Warn("observe: no per-user client, skipping user",
			"user_id", textsafe.SanitizeSingleLine(userID), "user", username)
		return nil
	}
	targets, err := s.targets(ctx, client, userID, key, key, anchor, played)
	if err != nil {
		return listFailed(ctx, played, username, err)
	}
	p := &plan{
		pick: pickForIntent(merged, s.cfg.SubtitleFloor), ref: merged, targets: targets,
		basis:      streams.Basis{Version: now, Source: streams.SourceIntent},
		observedAt: now, userList: true,
	}
	written, err := s.apply.applyLocked(ctx, client, userID, p)
	logApplied(trigger, username, played, merged, written, len(targets), "verdict", verdict.String())
	return err
}

// intentFor returns the user's record that governs ep and the key it is
// stored under: the newer of ep's show and season records, a tie to the
// show's. A record's coverage comes from its key, never from UPDATE_LEVEL.
func (s *Syncer) intentFor(userID string, ep *streams.Episode) (scopeKey, streams.Intent, bool) {
	show, season := coveringKeys(ep)
	showIn, showOK := s.cache.IntentFor(userID, show.String())
	seasonIn, seasonOK := s.cache.IntentFor(userID, season.String())
	switch {
	case seasonOK && (!showOK || seasonIn.ObservedAt > showIn.ObservedAt):
		return season, seasonIn, true
	case showOK:
		return show, showIn, true
	default:
		return scopeKey{}, streams.Intent{}, false
	}
}

// targets lists, through the user's client, the episodes the user's record
// under key with anchor writes: scopeOf cover, less the episodes another
// record of the user governs. cover is key, or the season a legacy show
// record covers (see Reconcile). ep is an episode of cover's season, whose
// season list a season key reads.
func (s *Syncer) targets(ctx context.Context, client PlexReadWriter, userID string,
	key, cover scopeKey, anchor streams.Anchor, ep *streams.Episode,
) ([]streams.Episode, error) {
	var list []streams.Episode
	var err error
	if cover.Season == showSeason {
		list, err = client.ShowEpisodes(ctx, plex.RatingKey(cover.Show))
	} else {
		list, err = client.SeasonEpisodes(ctx, plex.RatingKey(ep.ParentRatingKey))
	}
	if err != nil {
		return nil, err
	}
	scoped := scopeOf(cover, s.cfg.UpdateStrategy, anchor, list)
	out := scoped[:0]
	for i := range scoped {
		if governing, _, ok := s.intentFor(userID, &scoped[i]); ok && governing == key {
			out = append(out, scoped[i])
		}
	}
	return out, nil
}

// listFailed returns ctx.Err() for a list the context ended; any other
// failure is logged and no episode is written. A choice Observe already
// recorded stays, and the history replay retries the writes.
func listFailed(ctx context.Context, ep *streams.Episode, username string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	slog.Warn("failed to fetch episodes for update",
		"show", ep.GrandparentTitle, "user", username, "error", err)
	return nil
}

// logApplied logs a decision that changed at least one episode.
func logApplied(trigger, username string, ref *streams.Episode, choice streams.Pair, written, total int, extra ...any) {
	if written == 0 {
		return
	}
	attrs := append([]any{
		"trigger", trigger,
		"user", username,
		"show", ref.GrandparentTitle,
		"reference", ref.ShortName(),
		"audio", streams.Desc(choice.Audio),
		"subtitle", streams.Desc(choice.Subtitle),
		"episodes_updated", written,
		"episodes_total", total,
	}, extra...)
	slog.Info("language update complete", attrs...)
}

// Reconcile re-applies, on the history replay, the user's recorded choice
// that governs ep, to every episode that record governs. It never derives a
// choice from ep's current selection: a later read cannot tell the user's
// pick from the app's own write or Plex's default. A show with no record is
// skipped, and the applier leaves alone each episode the user played since
// the app's last decision there, ep included. The client comes from userID,
// since Plex records a write against the requesting token's user. The only
// error is ctx's.
func (s *Syncer) Reconcile(ctx context.Context, userID string, ep *streams.Episode, trigger string) error {
	username := s.users.Name(userID)
	client := s.userClient(userID)
	if client == nil {
		slog.Warn("reconcile: no per-user client, skipping user",
			"user_id", textsafe.SanitizeSingleLine(userID), "user", username)
		return nil
	}
	if ep.GrandparentRatingKey == "" {
		slog.Debug("no show rating key, skipping", "episode", ep.ShortName(), "user", username)
		return nil
	}
	unlock, err := s.apply.lock(ctx, userID, ep.GrandparentRatingKey)
	if err != nil {
		return err
	}
	defer unlock()

	key, in, ok := s.intentFor(userID, ep)
	if !ok {
		slog.Debug("reconcile: no intent recorded for show, skipping", "episode", ep.ShortName(), "user", username)
		return nil
	}
	// After the in-memory read, since the check costs a show-metadata fetch.
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, ep) {
		return nil
	}
	ref := in.RefStreams()
	anchor, cover := in.Anchor, key
	if anchor.IsZero() {
		anchor = streams.AnchorOf(ep, ref)
		// Older versions kept no anchor and stored every choice under the
		// show key, applying it at UPDATE_LEVEL=season to one season only.
		if key.Season == showSeason {
			cover = scopeKeyFor(s.cfg.UpdateLevel, ep)
		}
	}
	targets, err := s.targets(ctx, client, userID, key, cover, anchor, ep)
	if err != nil {
		return listFailed(ctx, ep, username, err)
	}
	p := &plan{
		pick: pickForIntent(ref, s.cfg.SubtitleFloor), ref: ref, targets: targets,
		basis:      streams.Basis{Version: in.ObservedAt, Source: streams.SourceIntent},
		observedAt: in.ObservedAt, userList: true,
	}
	written, err := s.apply.applyLocked(ctx, client, userID, p)
	logApplied(trigger, username, ep, ref, written, len(targets))
	return err
}

// logSubstitution records a track change and the language distance it was
// accepted at, so a surprising choice is diagnosable from logs. A subtitle
// past langtag.TierOtherRegion logs at INFO, with the table's recorded
// justification when there is one: only a SUBTITLE_MATCH_TIER widened past the
// default (defaultSubtitleTier in package main) admits it. Other subtitle
// matches, and audio at any tier (its floor is fixed), log at DEBUG, where a
// line per episode costs nothing.
func logSubstitution(ep *streams.Episode, username, kind string, ref, matched *streams.Stream) {
	tier := streams.MatchTier(ref, matched)
	if kind == kindAudio {
		slog.Debug("track language matched", logAttrs(ep, username, kind, ref, matched, tier)...)
		return
	}
	attrs := logAttrs(ep, username, kind, ref, matched, tier)
	if tier <= langtag.TierOtherRegion {
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

// logAttrs builds the attribute list shared by both log levels. The language
// fields are sanitized because Plex passes a file's own tag through, malformed
// or not.
func logAttrs(ep *streams.Episode, username, kind string, ref, matched *streams.Stream, tier langtag.Tier) []any {
	return []any{
		"episode", ep.ShortName(),
		"user", username,
		"kind", kind,
		"from", textsafe.SanitizeSingleLine(ref.LanguageCode),
		"to", textsafe.SanitizeSingleLine(matched.LanguageCode),
		"from_tag", textsafe.SanitizeSingleLine(ref.LanguageTag),
		"to_tag", textsafe.SanitizeSingleLine(matched.LanguageTag),
		"match_tier", tier.String(),
	}
}

// learnProfileFromReference records the user's active audio→subtitle
// pairing into the cache when language profiles are enabled and the
// audio has a language code.
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
