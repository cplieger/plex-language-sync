package notify

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/coder/websocket"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"pgregory.net/rapid"
)

// TestClassifyError covers the substring-free, typed-sentinel
// classification path. Each case wraps the typed sentinel with %w so
// ClassifyError resolves via errors.Is rather than err.Error()
// substring matching.
func TestClassifyError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ReasonUnknown},
		{
			"read_limit wrapped",
			fmt.Errorf("websocket read: %w", ErrReadLimit),
			ReasonReadLimit,
		},
		{
			"dial_failed wrapped",
			fmt.Errorf("%w: connection refused", ErrDialFailed),
			ReasonDialFailed,
		},
		{
			"server_close wrapped",
			fmt.Errorf("%w: EOF", ErrServerClose),
			ReasonServerClose,
		},
		{
			"read_error wrapped",
			fmt.Errorf("%w: i/o timeout", ErrReadError),
			ReasonReadError,
		},
		{"unknown", errors.New("something else"), ReasonUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyError(tt.err); got != tt.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestClassifyError_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	if got := ClassifyError(context.DeadlineExceeded); got != ReasonReadError {
		t.Errorf("ClassifyError(DeadlineExceeded) = %q, want %q", got, ReasonReadError)
	}
}

// TestClassifyError_CloseError proves the typed matching on
// *websocket.CloseError works end-to-end without any substring
// matching on the error text.
func TestClassifyError_CloseError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
		code websocket.StatusCode
	}{
		{"normal_closure_1000", ReasonServerClose, websocket.StatusNormalClosure},
		{"going_away_1001", ReasonServerClose, websocket.StatusGoingAway},
		{"abnormal_closure_1006", ReasonServerClose, websocket.StatusAbnormalClosure},
		{"protocol_error_1002", ReasonUnknown, websocket.StatusProtocolError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := websocket.CloseError{Code: tt.code, Reason: "fixture"}
			if got := ClassifyError(err); got != tt.want {
				t.Errorf("ClassifyError(CloseError{%d}) = %q, want %q", tt.code, got, tt.want)
			}
			// Also verify matching when wrapped.
			wrapped := fmt.Errorf("surrounding context: %w", err)
			if got := ClassifyError(wrapped); got != tt.want {
				t.Errorf("ClassifyError(wrapped CloseError{%d}) = %q, want %q",
					tt.code, got, tt.want)
			}
		})
	}
}

// TestIsRelevantPlayEvent mirrors the table-driven assertions the main
// package used to own before the extraction.
func TestIsRelevantPlayEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ev   PlayEvent
		want bool
	}{
		{"playing with key", PlayEvent{State: "playing", RatingKey: "123"}, true},
		{"paused with key", PlayEvent{State: "paused", RatingKey: "456"}, true},
		{"stopped with key", PlayEvent{State: "stopped", RatingKey: "789"}, false},
		{"playing empty key", PlayEvent{State: "playing", RatingKey: ""}, false},
		{"empty state with key", PlayEvent{State: "", RatingKey: "123"}, false},
		{"buffering with key", PlayEvent{State: "buffering", RatingKey: "123"}, false},
		{"both empty", PlayEvent{State: "", RatingKey: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRelevantPlayEvent(tt.ev); got != tt.want {
				t.Errorf("IsRelevantPlayEvent(%+v) = %v, want %v", tt.ev, got, tt.want)
			}
		})
	}
}

func TestBuildStreamCacheKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		userID    string
		ratingKey string
		want      string
		audioID   int
		subID     int
	}{
		{name: "typical", userID: "42", ratingKey: "1234", want: "streams:42:1234:100:200", audioID: 100, subID: 200},
		{name: "zero IDs", userID: "1", ratingKey: "999", want: "streams:1:999:0:0", audioID: 0, subID: 0},
		{name: "large IDs", userID: "100", ratingKey: "99999", want: "streams:100:99999:65535:32768", audioID: 65535, subID: 32768},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BuildStreamCacheKey(StreamSelectionKey{UserID: tt.userID, RatingKey: tt.ratingKey, AudioID: tt.audioID, SubtitleID: tt.subID})
			if got != tt.want {
				t.Errorf("BuildStreamCacheKey(%q, %q, %d, %d) = %q, want %q",
					tt.userID, tt.ratingKey, tt.audioID, tt.subID, got, tt.want)
			}
		})
	}
}

// TestBuildStreamCacheKeyDistinguishesSelection characterizes the property
// handlePlayEvent's selection-aware dedup relies on: a changed audio or
// subtitle selection on the same (user, episode) must produce a DISTINCT
// stream cache key, while an unchanged selection must produce an IDENTICAL
// key. This is what lets the CheckAndMark gate detect a mid-playback
// correction (different key => not yet processed) without re-propagating an
// unchanged selection (same key => already processed). Without this, the
// removed session pre-filter would have to guard duplicate work.
func TestBuildStreamCacheKeyDistinguishesSelection(t *testing.T) {
	t.Parallel()
	const (
		userID    = "42"
		ratingKey = "1234"
		audioA    = 100
		audioB    = 101
		subA      = 200
		subB      = 201
	)
	base := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioA, SubtitleID: subA})

	if changedAudio := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioB, SubtitleID: subA}); changedAudio == base {
		t.Errorf("changed audio selection must yield a distinct key: both = %q", base)
	}
	if changedSub := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioA, SubtitleID: subB}); changedSub == base {
		t.Errorf("changed subtitle selection must yield a distinct key: both = %q", base)
	}
	if changedBoth := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioB, SubtitleID: subB}); changedBoth == base {
		t.Errorf("changed audio+subtitle selection must yield a distinct key: both = %q", base)
	}
	if same := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioA, SubtitleID: subA}); same != base {
		t.Errorf("identical selection must yield an identical key: %q != %q", same, base)
	}
}

func TestBuildTimelineCacheKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		itemID string
		want   string
	}{
		{name: "typical", itemID: "1234", want: "timeline:1234"},
		{name: "empty", itemID: "", want: "timeline:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BuildTimelineCacheKey(tt.itemID)
			if got != tt.want {
				t.Errorf("BuildTimelineCacheKey(%q) = %q, want %q", tt.itemID, got, tt.want)
			}
		})
	}
}

func TestIsRelevantTimelineEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry TimelineEntry
		want  bool
	}{
		{
			"episode metadata created",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MetadataState: stateCreated, ItemID: "123"},
			true,
		},
		{
			"episode metadata updated",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MetadataState: stateUpdated, ItemID: "456"},
			true,
		},
		{
			"episode media created",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MediaState: stateCreated, ItemID: "789"},
			true,
		},
		{
			"episode media updated",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MediaState: stateUpdated, ItemID: "101"},
			true,
		},
		{
			"non-episode type",
			TimelineEntry{Type: 1, MetadataState: stateCreated, ItemID: "123"},
			false,
		},
		{
			"episode no relevant state",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MetadataState: "deleted", ItemID: "123"},
			false,
		},
		{
			"episode created but empty ID",
			TimelineEntry{Type: plex.MetadataTypeEpisode, MetadataState: stateCreated, ItemID: ""},
			false,
		},
		{"all empty", TimelineEntry{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsRelevantTimelineEntry(&tt.entry); got != tt.want {
				t.Errorf("IsRelevantTimelineEntry(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestTimelineAction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		want  string
		entry TimelineEntry
	}{
		{name: "metadata created", entry: TimelineEntry{MetadataState: stateCreated}, want: "scan_new"},
		{name: "media created", entry: TimelineEntry{MediaState: stateCreated}, want: "scan_new"},
		{
			name:  "both created",
			entry: TimelineEntry{MetadataState: stateCreated, MediaState: stateCreated},
			want:  "scan_new",
		},
		{name: "metadata updated", entry: TimelineEntry{MetadataState: stateUpdated}, want: "scan_updated"},
		{name: "media updated", entry: TimelineEntry{MediaState: stateUpdated}, want: "scan_updated"},
		{name: "neither", entry: TimelineEntry{}, want: "scan_updated"},
		{
			name:  "metadata created media updated",
			entry: TimelineEntry{MetadataState: stateCreated, MediaState: stateUpdated},
			want:  "scan_new",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := TimelineAction(&tt.entry); got != tt.want {
				t.Errorf("TimelineAction(%+v) = %q, want %q", tt.entry, got, tt.want)
			}
		})
	}
}

// legacyStreamCacheKey is the exact pre-keyenc expression BuildStreamCacheKey
// used: a plain fmt.Sprintf concatenation with no escaping. Kept here as the
// oracle for the byte-identity tests below, so "the persisted state.json
// schema did not change" is pinned against the real former bytes rather than
// against a re-derivation of them.
func legacyStreamCacheKey(userID, ratingKey string, audioID, subID int) string {
	return fmt.Sprintf("%s%s:%s:%d:%d", cache.KeyPrefixStreams, userID, ratingKey, audioID, subID)
}

// TestBuildStreamCacheKeyByteIdenticalToLegacyFormat pins that adopting keyenc
// did NOT change the key bytes for ordinary Plex input. This key is PERSISTED
// (the cache's ProcessedEpisodes map in state.json), so a byte change would
// orphan every entry written by a previous version and cost a round of
// redundant re-propagation on upgrade.
//
// Byte-identity holds because keyenc.Join emits a component containing neither
// ':' nor '\' verbatim, and because the leading component is always "streams":
// the escaped join therefore never begins with keyenc's hashed-identity
// prefix, which is the one in-bound case that would route an ordinary set
// through the hash instead. Scoped to in-bound sizes; a component set over
// keyenc.MaxComponentBytes hashes by design and no Plex ID approaches 8 KiB.
func TestBuildStreamCacheKeyByteIdenticalToLegacyFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		userID    string
		ratingKey string
		audioID   int
		subID     int
	}{
		{name: "typical", userID: "42", ratingKey: "1234", audioID: 100, subID: 200},
		{name: "zero IDs", userID: "1", ratingKey: "999", audioID: 0, subID: 0},
		{name: "large IDs", userID: "100", ratingKey: "99999", audioID: 65535, subID: 32768},
		{name: "negative IDs", userID: "7", ratingKey: "8", audioID: -1, subID: -2},
		{name: "empty userID", userID: "", ratingKey: "1234", audioID: 1, subID: 2},
		{name: "empty ratingKey", userID: "42", ratingKey: "", audioID: 1, subID: 2},
		{name: "both string fields empty", userID: "", ratingKey: "", audioID: 0, subID: 0},
		{name: "non-numeric but separator-free", userID: "user-a_1", ratingKey: "rk.9", audioID: 3, subID: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := legacyStreamCacheKey(tt.userID, tt.ratingKey, tt.audioID, tt.subID)
			got := BuildStreamCacheKey(StreamSelectionKey{UserID: tt.userID, RatingKey: tt.ratingKey, AudioID: tt.audioID, SubtitleID: tt.subID})
			if got != want {
				t.Errorf("BuildStreamCacheKey(%q, %q, %d, %d) = %q, want %q (byte-identity with the persisted state.json schema is broken)",
					tt.userID, tt.ratingKey, tt.audioID, tt.subID, got, want)
			}
		})
	}
}

// TestBuildStreamCacheKeySeparatorFieldCannotCollide pins the bug the adoption
// fixes. userID (resolved from Plex's sessions response) and ratingKey (read
// off the WebSocket notification) are both upstream strings that may contain a
// literal ':'. Under the old concatenation, moving a ':' from the end of
// userID to the front of ratingKey produced the SAME key, so two distinct
// (user, episode) identities shared one dedup marker: CheckAndMark reported
// "already processed" for a selection change it had never seen and the
// propagation was silently skipped.
//
// The test asserts both halves — that the legacy form really did collapse
// these tuples (otherwise it proves nothing) and that the keyenc form keeps
// them apart.
func TestBuildStreamCacheKeySeparatorFieldCannotCollide(t *testing.T) {
	t.Parallel()
	const (
		audioID = 100
		subID   = 200
	)
	tests := []struct {
		name       string
		userA, rkA string
		userB, rkB string
	}{
		{
			name:  "colon at end of userID vs start of ratingKey",
			userA: "42:1234", rkA: "5",
			userB: "42", rkB: "1234:5",
		},
		{
			name:  "two colons shifted across the field boundary",
			userA: "42:1234:5", rkA: "6",
			userB: "42", rkB: "1234:5:6",
		},
		{
			name:  "colon shifting a numeric ID into ratingKey",
			userA: "7:99", rkA: "8",
			userB: "7", rkB: "99:8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if legacyA, legacyB := legacyStreamCacheKey(tt.userA, tt.rkA, audioID, subID),
				legacyStreamCacheKey(tt.userB, tt.rkB, audioID, subID); legacyA != legacyB {
				t.Fatalf("premise broken: the legacy form was expected to collide, got %q and %q", legacyA, legacyB)
			}
			gotA := BuildStreamCacheKey(StreamSelectionKey{UserID: tt.userA, RatingKey: tt.rkA, AudioID: audioID, SubtitleID: subID})
			gotB := BuildStreamCacheKey(StreamSelectionKey{UserID: tt.userB, RatingKey: tt.rkB, AudioID: audioID, SubtitleID: subID})
			if gotA == gotB {
				t.Errorf("(userID %q, ratingKey %q) and (userID %q, ratingKey %q) must not share a dedup key, both = %q",
					tt.userA, tt.rkA, tt.userB, tt.rkB, gotA)
			}
		})
	}
}

// TestBuildStreamCacheKeyDistinctTuplesDistinctKeys sweeps a set of adversarial
// tuples — separator-bearing, escape-bearing, and empty fields — and requires
// every distinct tuple to map to a distinct key. This catches aliasing that a
// pairwise test would miss, in particular a field spelling an escape sequence
// colliding with the encoding of a field that genuinely contains a separator.
func TestBuildStreamCacheKeyDistinctTuplesDistinctKeys(t *testing.T) {
	t.Parallel()
	type tuple struct {
		userID    string
		ratingKey string
		audioID   int
		subID     int
	}
	tuples := []tuple{
		{"42", "1234", 100, 200},
		{"42:1234", "5", 100, 200},
		{"42", "1234:5", 100, 200},
		{"42:1234:5", "", 100, 200},
		{"", "42:1234:5", 100, 200},
		{`42\`, "1234", 100, 200},
		{"42", `\1234`, 100, 200},
		{`42\:1234`, "5", 100, 200},
		{`42\\`, ":1234", 100, 200},
		{"42", "1234", 200, 100},
		{"", "", 0, 0},
	}
	seen := make(map[string]tuple, len(tuples))
	for _, tp := range tuples {
		key := BuildStreamCacheKey(StreamSelectionKey{UserID: tp.userID, RatingKey: tp.ratingKey, AudioID: tp.audioID, SubtitleID: tp.subID})
		if prev, dup := seen[key]; dup {
			t.Errorf("distinct tuples %+v and %+v collapsed to the same key %q", prev, tp, key)
			continue
		}
		seen[key] = tp
	}
}

// TestBuildStreamCacheKeyLegacyIdentityProperty is the randomized half of the
// byte-identity guarantee: over any separator-free and escape-free field
// content, keyenc.Join must reproduce the legacy concatenation exactly. The
// table above fixes the cases that matter by name; this covers the alphabet.
func TestBuildStreamCacheKeyLegacyIdentityProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Any byte except the two keyenc reserves (':' and '\'), which are
		// exactly the inputs whose bytes are ALLOWED to change.
		safe := `[^:\\]*`
		userID := rapid.StringMatching(safe).Draw(t, "userID")
		ratingKey := rapid.StringMatching(safe).Draw(t, "ratingKey")
		audioID := rapid.Int().Draw(t, "audioID")
		subID := rapid.Int().Draw(t, "subID")

		want := legacyStreamCacheKey(userID, ratingKey, audioID, subID)
		got := BuildStreamCacheKey(StreamSelectionKey{UserID: userID, RatingKey: ratingKey, AudioID: audioID, SubtitleID: subID})
		if got != want {
			t.Fatalf("BuildStreamCacheKey(%q, %q, %d, %d) = %q, want legacy %q",
				userID, ratingKey, audioID, subID, got, want)
		}
	})
}
