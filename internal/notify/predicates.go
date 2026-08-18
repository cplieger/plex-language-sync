package notify

import (
	"strconv"
	"strings"

	"github.com/cplieger/keyenc"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// streamsKeyKind is the leading component of the stream dedup key: the
// on-disk prefix constant minus the trailing separator that keyenc.Join
// re-inserts between components. Derived rather than spelled out so the
// state.json prefix stays defined in exactly one place (cache.KeyPrefixStreams).
var streamsKeyKind = strings.TrimSuffix(cache.KeyPrefixStreams, string(keyenc.Separator))

// Plex wire-format constants used by the event predicates. These mirror
// the Plex notification schema. Kept unexported here because they are
// implementation details of the notify package's classification logic;
// callers should use IsRelevantPlayEvent / IsRelevantTimelineEntry /
// TimelineAction rather than branching on these values directly. The
// episode metadata type is imported from internal/plex (single source of
// truth).
const (
	stateCreated = "created"
	stateUpdated = "updated"
	statePlaying = "playing"
	statePaused  = "paused"

	scanActionNew     = "scan_new"
	scanActionUpdated = "scan_updated"
)

// IsRelevantPlayEvent returns true if a play event should be processed
// (state is playing/paused and has a rating key).
func IsRelevantPlayEvent(ev PlayEvent) bool {
	if ev.State != statePlaying && ev.State != statePaused {
		return false
	}
	return ev.RatingKey != ""
}

// IsRelevantTimelineEntry returns true if a timeline entry should be
// processed (episode type, metadata/media created or updated, non-empty
// item ID).
func IsRelevantTimelineEntry(entry *TimelineEntry) bool {
	if entry.Type != plex.MetadataTypeEpisode {
		return false
	}
	if entry.MetadataState != stateCreated && entry.MetadataState != stateUpdated &&
		entry.MediaState != stateCreated && entry.MediaState != stateUpdated {
		return false
	}
	return entry.ItemID != ""
}

// TimelineAction returns "scan_new" if the entry represents a newly
// created item, or "scan_updated" otherwise. The returned strings are
// byte-for-byte frozen — they are emitted as log/metric values consumed
// by dashboards.
func TimelineAction(entry *TimelineEntry) string {
	if entry.MetadataState == stateCreated || entry.MediaState == stateCreated {
		return scanActionNew
	}
	return scanActionUpdated
}

// BuildStreamCacheKey builds a deduplication key from user, episode, and
// current stream IDs so we only process when the selection actually
// changes. The "streams:" prefix and colon-separated layout are part of
// the on-disk state.json schema.
//
// The key is escaped with keyenc rather than concatenated because two of its
// components are upstream strings, not integers: userID comes from Plex's
// sessions response (via UserFromSession) and ratingKey straight off the
// WebSocket notification. Neither is validated against a separator-free
// alphabet before it reaches this function, so either can carry a literal
// ':'. Under plain concatenation that lets one tuple forge another's key —
// (userID "42:1234", ratingKey "5") and (userID "42", ratingKey "1234:5")
// both render "streams:42:1234:5:100:200".
//
// A collision here is a wrong dedup hit, and it fails silently in the
// direction that loses work: the forged key is already in the cache's
// ProcessedEpisodes map, so CheckAndMark reports "recently processed" for a
// selection change it has never actually seen, and handlePlayEvent returns
// without propagating. Two distinct (user, episode, audio, sub) identities
// merge into one, and the user's language choice is silently dropped for the
// rest of the 5-minute dedup window. keyenc.Join escapes each component
// before inserting the separators, so distinct tuples always produce distinct
// keys.
//
// Components free of ':' and '\' are emitted verbatim, so for ordinary Plex
// input this is byte-for-byte the string the previous fmt.Sprintf produced —
// the state.json schema and every already-persisted key are unaffected
// (pinned by TestBuildStreamCacheKey and
// TestBuildStreamCacheKeyByteIdenticalToLegacyFormat).
func BuildStreamCacheKey(sel StreamSelectionKey) string {
	return keyenc.Join(streamsKeyKind, sel.UserID, sel.RatingKey,
		strconv.Itoa(sel.AudioID), strconv.Itoa(sel.SubtitleID))
}

// StreamSelectionKey names the four values that identify one user's stream
// selection on one episode. Two adjacent strings followed by two adjacent
// ints: every transposition type-checked and produced a WELL-FORMED key for
// the wrong stream, so the dedup check silently passed or silently suppressed.
// Named fields make the mistake visible where it is made.
type StreamSelectionKey struct {
	// UserID is the Plex account the selection belongs to.
	UserID string
	// RatingKey is the episode.
	RatingKey string
	// AudioID is the selected audio stream's id, 0 when none.
	AudioID int
	// SubtitleID is the selected subtitle stream's id, 0 when none.
	SubtitleID int
}

// BuildTimelineCacheKey builds the per-episode timeline (library-scan)
// dedup key. The "timeline:" prefix is part of the on-disk state.json
// schema.
//
// Deliberately NOT escaped, unlike BuildStreamCacheKey: itemID is the whole
// remainder of the key rather than one field among several, so the mapping
// from itemID to key is injective no matter what itemID contains. There is no
// following component for a ':' inside itemID to be mistaken for, hence no
// forgeable collision to prevent.
func BuildTimelineCacheKey(itemID string) string {
	return cache.KeyPrefixTimeline + itemID
}
