package notify

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/coder/websocket"

	"plex-language-sync/internal/plex"
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
		code websocket.StatusCode
		want string
	}{
		{"normal_closure_1000", websocket.StatusNormalClosure, ReasonServerClose},
		{"going_away_1001", websocket.StatusGoingAway, ReasonServerClose},
		{"abnormal_closure_1006", websocket.StatusAbnormalClosure, ReasonServerClose},
		{"protocol_error_1002", websocket.StatusProtocolError, ReasonUnknown},
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
		audioID   int
		subID     int
		want      string
	}{
		{"typical", "42", "1234", 100, 200, "streams:42:1234:100:200"},
		{"zero IDs", "1", "999", 0, 0, "streams:1:999:0:0"},
		{"large IDs", "100", "99999", 65535, 32768, "streams:100:99999:65535:32768"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BuildStreamCacheKey(tt.userID, tt.ratingKey, tt.audioID, tt.subID)
			if got != tt.want {
				t.Errorf("BuildStreamCacheKey(%q, %q, %d, %d) = %q, want %q",
					tt.userID, tt.ratingKey, tt.audioID, tt.subID, got, tt.want)
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
		entry TimelineEntry
		want  string
	}{
		{"metadata created", TimelineEntry{MetadataState: stateCreated}, "scan_new"},
		{"media created", TimelineEntry{MediaState: stateCreated}, "scan_new"},
		{
			"both created",
			TimelineEntry{MetadataState: stateCreated, MediaState: stateCreated},
			"scan_new",
		},
		{"metadata updated", TimelineEntry{MetadataState: stateUpdated}, "scan_updated"},
		{"media updated", TimelineEntry{MediaState: stateUpdated}, "scan_updated"},
		{"neither", TimelineEntry{}, "scan_updated"},
		{
			"metadata created media updated",
			TimelineEntry{MetadataState: stateCreated, MediaState: stateUpdated},
			"scan_new",
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
