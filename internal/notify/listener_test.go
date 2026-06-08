package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"pgregory.net/rapid"
)

// fakeHandler records the events delivered by dispatch / Listen so
// tests can assert on the observed sequence without standing up a
// full WebSocket server.
type fakeHandler struct {
	plays     []PlayEvent
	timelines [][]TimelineEntry
	mu        sync.Mutex
}

func (f *fakeHandler) OnPlay(_ context.Context, ev PlayEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plays = append(f.plays, ev)
}

func (f *fakeHandler) OnTimeline(_ context.Context, entries []TimelineEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timelines = append(f.timelines, entries)
}

// TestDispatch_Playing verifies a "playing" NotificationContainer
// fans each PlaySessionStateNotification entry out to OnPlay.
func TestDispatch_Playing(t *testing.T) {
	t.Parallel()
	var notif Notification
	notif.NotificationContainer.Type = "playing"
	notif.NotificationContainer.PlaySessionStateNotification = []PlayEvent{
		{RatingKey: "1", State: "playing"},
		{RatingKey: "2", State: "paused"},
	}

	h := &fakeHandler{}
	dispatch(context.Background(), h, &notif)

	if len(h.plays) != 2 {
		t.Fatalf("dispatch delivered %d plays, want 2", len(h.plays))
	}
	if h.plays[0].RatingKey != "1" || h.plays[1].RatingKey != "2" {
		t.Errorf("play order mismatch: got %+v", h.plays)
	}
	if len(h.timelines) != 0 {
		t.Errorf("dispatch delivered %d timelines, want 0", len(h.timelines))
	}
}

// TestDispatch_Timeline verifies a "timeline" NotificationContainer
// delivers the full TimelineEntry slice in one OnTimeline call.
func TestDispatch_Timeline(t *testing.T) {
	t.Parallel()
	var notif Notification
	notif.NotificationContainer.Type = "timeline"
	notif.NotificationContainer.TimelineEntry = []TimelineEntry{
		{ItemID: "a", Type: plex.MetadataTypeEpisode, MetadataState: stateCreated},
		{ItemID: "b", Type: plex.MetadataTypeEpisode, MetadataState: stateUpdated},
	}

	h := &fakeHandler{}
	dispatch(context.Background(), h, &notif)

	if len(h.timelines) != 1 {
		t.Fatalf("dispatch delivered %d timelines, want 1", len(h.timelines))
	}
	if len(h.timelines[0]) != 2 {
		t.Errorf("timeline slice len = %d, want 2", len(h.timelines[0]))
	}
	if len(h.plays) != 0 {
		t.Errorf("dispatch delivered %d plays, want 0", len(h.plays))
	}
}

// TestDispatch_UnknownTypeIgnored proves NotificationContainer.Type
// values outside {"playing","timeline"} are silently dropped so an
// upstream schema evolution does not spam the Handler.
func TestDispatch_UnknownTypeIgnored(t *testing.T) {
	t.Parallel()
	var notif Notification
	notif.NotificationContainer.Type = "activity"

	h := &fakeHandler{}
	dispatch(context.Background(), h, &notif)

	if len(h.plays)+len(h.timelines) != 0 {
		t.Errorf("unknown type produced %d plays + %d timelines, want 0 total",
			len(h.plays), len(h.timelines))
	}
}

// TestNotificationRoundTripJSON pins the wire-format JSON tags: a
// Notification marshals to the Plex field names Plex sends, and
// unmarshal of a realistic Plex payload populates the expected fields.
func TestNotificationRoundTripJSON(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
        "NotificationContainer": {
            "type": "playing",
            "PlaySessionStateNotification": [
                {"sessionKey": "7", "ratingKey": "42", "state": "playing", "viewOffset": 100}
            ],
            "TimelineEntry": [
                {"itemID": "99", "type": 4, "metadataState": "created"}
            ]
        }
    }`)

	var n Notification
	if err := json.Unmarshal(payload, &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.NotificationContainer.Type != "playing" {
		t.Errorf("type = %q, want 'playing'", n.NotificationContainer.Type)
	}
	if len(n.NotificationContainer.PlaySessionStateNotification) != 1 {
		t.Fatalf("plays len = %d, want 1", len(n.NotificationContainer.PlaySessionStateNotification))
	}
	ev := n.NotificationContainer.PlaySessionStateNotification[0]
	if ev.SessionKey != "7" || ev.RatingKey != "42" || ev.ViewOffset != 100 {
		t.Errorf("play event round-trip = %+v", ev)
	}
	if len(n.NotificationContainer.TimelineEntry) != 1 {
		t.Fatalf("timelines len = %d, want 1", len(n.NotificationContainer.TimelineEntry))
	}
	if n.NotificationContainer.TimelineEntry[0].ItemID != "99" {
		t.Errorf("timeline itemID = %q, want '99'", n.NotificationContainer.TimelineEntry[0].ItemID)
	}
}

// fakePlexClient is a minimal PlexClient for Listener construction
// tests. Listener exercise via httptest + real Dial is deferred to
// integration-style tests; the unit layer covers dispatch + classify +
// backoff, and the listen loop is exercised in main's integration test
// surface.
type fakePlexClient struct {
	base   *url.URL
	client *http.Client
	token  string
}

func (f *fakePlexClient) BaseURL() *url.URL { return f.base }
func (f *fakePlexClient) Token() string     { return f.token }
func (f *fakePlexClient) HTTPClient() *http.Client {
	if f.client == nil {
		f.client = &http.Client{Timeout: 2 * time.Second}
	}
	return f.client
}

// TestNewListener_UsesProvidedConfig confirms the Config shrinkage
// tests can pass without mutating globals.
func TestNewListener_UsesProvidedConfig(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://plex.example:32400")
	cfg := Config{
		MinBackoff:      10 * time.Millisecond,
		MaxBackoff:      50 * time.Millisecond,
		StableThreshold: 20 * time.Millisecond,
	}
	l := NewListener(&fakePlexClient{base: base, token: "t"}, cfg)
	if l == nil {
		t.Fatal("NewListener returned nil")
	}
	if l.cfg != cfg {
		t.Errorf("listener cfg = %+v, want %+v", l.cfg, cfg)
	}
}

// TestListen_ReturnsOnCancelledContext exercises the outer loop exit
// path without needing a real websocket server: a nil-pointer dial
// target returns an error, the loop sleeps for MinBackoff, and the
// context cancellation terminates the loop.
func TestListen_ReturnsOnCancelledContext(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://127.0.0.1:1/")
	cfg := Config{
		MinBackoff:      5 * time.Millisecond,
		MaxBackoff:      10 * time.Millisecond,
		StableThreshold: 1 * time.Millisecond,
	}
	l := NewListener(&fakePlexClient{base: base, token: "t"}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		l.Listen(ctx, &fakeHandler{})
		close(done)
	}()

	select {
	case <-done:
		// pass
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Listen did not return within 500ms of context cancellation")
	}
}

// Property-based: never-panics mirror of the main_test.go PBT.
func TestIsRelevantPlayEventNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ev := PlayEvent{
			State:     rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "state"),
			RatingKey: rapid.StringMatching(`[0-9]{0,5}`).Draw(t, "key"),
		}
		_ = IsRelevantPlayEvent(ev)
	})
}

func TestIsRelevantTimelineEntryNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		entry := TimelineEntry{
			Type:          rapid.IntRange(0, 10).Draw(t, "type"),
			MetadataState: rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "metaState"),
			MediaState:    rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "mediaState"),
			ItemID:        rapid.StringMatching(`[0-9]{0,5}`).Draw(t, "itemID"),
		}
		_ = IsRelevantTimelineEntry(&entry)
	})
}

func TestTimelineActionAlwaysReturnsValidAction(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		entry := TimelineEntry{
			MetadataState: rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "metaState"),
			MediaState:    rapid.StringMatching(`[a-z]{0,10}`).Draw(t, "mediaState"),
		}
		got := TimelineAction(&entry)
		if got != "scan_new" && got != "scan_updated" {
			t.Errorf("TimelineAction(%+v) = %q, want 'scan_new' or 'scan_updated'", entry, got)
		}
	})
}

func TestBuildStreamCacheKeyFormat(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		userID := rapid.StringMatching(`[0-9]{1,5}`).Draw(t, "userID")
		ratingKey := rapid.StringMatching(`[0-9]{1,5}`).Draw(t, "ratingKey")
		audioID := rapid.IntRange(0, 100000).Draw(t, "audioID")
		subID := rapid.IntRange(0, 100000).Draw(t, "subID")

		got := BuildStreamCacheKey(userID, ratingKey, audioID, subID)

		if !containsASCII(got, "streams:") || got[:len("streams:")] != "streams:" {
			t.Errorf("BuildStreamCacheKey(...) = %q, want 'streams:' prefix", got)
		}
		if !containsASCII(got, userID) {
			t.Errorf("BuildStreamCacheKey result %q does not contain userID %q", got, userID)
		}
		if !containsASCII(got, ratingKey) {
			t.Errorf("BuildStreamCacheKey result %q does not contain ratingKey %q", got, ratingKey)
		}
	})
}

// containsASCII is a local substring check so this file does not need
// to import "strings" just for two invariant assertions.
func containsASCII(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestConnectAndListen_ReadIdleTimeoutFires verifies that a websocket
// server which accepts the upgrade but never sends a message causes
// the listener's read to return an error (via the configurable
// ReadIdleTimeout backstop) rather than hanging forever. Pins the
// keepalive design promised by Config.ReadIdleTimeout.
func TestConnectAndListen_ReadIdleTimeoutFires(t *testing.T) {
	t.Parallel()

	// Server that accepts the websocket and then sleeps until the
	// client disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		// Block until the client (listener) cancels the read and tears
		// down the connection. Reading from the client side returns an
		// error when our caller closes; we just need to keep the
		// goroutine alive so the upgrade resp body stays open.
		_, _, _ = c.Read(r.Context())
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	cfg := DefaultConfig()
	cfg.ReadIdleTimeout = 100 * time.Millisecond
	l := NewListener(&fakePlexClient{base: base, token: "t", client: srv.Client()}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	connected, err := l.connectAndListen(ctx, &fakeHandler{})
	elapsed := time.Since(start)

	if !connected {
		t.Errorf("connected = false, want true (handshake should succeed)")
	}
	if err == nil {
		t.Errorf("expected read-idle error, got nil")
	}
	// Should fire shortly after ReadIdleTimeout, well before the test
	// context's 5-second cap. Allow generous slack for CI scheduling.
	if elapsed > 2*time.Second {
		t.Errorf("read-idle backstop took %v, expected ~100ms", elapsed)
	}
}
