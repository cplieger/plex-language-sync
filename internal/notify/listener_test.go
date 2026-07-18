package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// RedirectPolicy mirrors the production accessor: the policy off the
// fake's own client when one was supplied, nil otherwise (net/http's
// default follow behavior — what an unset CheckRedirect meant before).
func (f *fakePlexClient) RedirectPolicy() func(*http.Request, []*http.Request) error {
	if f.client != nil {
		return f.client.CheckRedirect
	}
	return nil
}

// BaseTransport returns nil: the fake owns its http.Client (like any
// WithHTTPClient-built plexapi client), so dialClient exercises its
// DefaultTransport-clone fallback path.
func (f *fakePlexClient) BaseTransport() *http.Transport { return nil }

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

		if !strings.HasPrefix(got, "streams:") {
			t.Errorf("BuildStreamCacheKey(...) = %q, want 'streams:' prefix", got)
		}
		if !strings.Contains(got, userID) {
			t.Errorf("BuildStreamCacheKey result %q does not contain userID %q", got, userID)
		}
		if !strings.Contains(got, ratingKey) {
			t.Errorf("BuildStreamCacheKey result %q does not contain ratingKey %q", got, ratingKey)
		}
	})
}

// TestConnectAndListen_SlowDialNotStable is the regression guard for the
// stability-timing fix: connection stability must be measured from
// handshake SUCCESS, not from before the dial. A slow-but-successful
// dial followed by a short real uptime previously inflated the elapsed
// time with dial latency and was falsely classified "stable", which
// resets the backoff and zeros the persistent-reconnect counter that
// drives the sole health ERROR.
//
// The server delays the websocket Accept (slow handshake), then closes
// immediately so the client's read returns almost at once (near-zero
// real uptime). StableThreshold sits between the dial delay and the
// uptime: time-from-handshake is BELOW it (correctly not stable), while
// time-from-dial-start would be ABOVE it (the old bug). Asserting the
// connection is NOT stable under the production formula pins the fix and
// fails against the pre-fix `time.Since(connectStart)` code.
func TestConnectAndListen_SlowDialNotStable(t *testing.T) {
	t.Parallel()

	const dialDelay = 150 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow handshake: stall before completing the upgrade.
		time.Sleep(dialDelay)
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		// Short real uptime: close cleanly right after the handshake so
		// the client's first Read returns promptly.
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	cfg := DefaultConfig()
	// Threshold between the dial delay and the (near-zero) post-handshake
	// uptime: the fixed code measures from handshake and stays under it;
	// the buggy code measured from dial start and would exceed it.
	cfg.StableThreshold = dialDelay / 2
	l := NewListener(&fakePlexClient{base: base, token: "t", client: srv.Client()}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connected, handshakeAt, err := l.connectAndListen(ctx, &fakeHandler{})

	if !connected {
		t.Fatalf("connected = false, want true (handshake should succeed)")
	}
	if handshakeAt.IsZero() {
		t.Fatalf("handshakeAt is zero, want the post-handshake instant")
	}
	if err == nil {
		t.Errorf("expected a disconnect error from the server close, got nil")
	}

	// Reproduce the production stability decision exactly (listener.go
	// Listen): stable iff connected AND the time held open since the
	// handshake exceeds StableThreshold. A slow dial must NOT make this
	// true.
	stable := connected && time.Since(handshakeAt) > cfg.StableThreshold
	if stable {
		t.Errorf("connection classified stable after a slow dial + short uptime; "+
			"stability must be measured from handshake success, not dial start "+
			"(time-since-handshake=%v, StableThreshold=%v)",
			time.Since(handshakeAt), cfg.StableThreshold)
	}

	// Belt-and-suspenders: the handshake instant must be recent (it was
	// stamped after the slow Accept), not back at the dial start. Allow
	// generous slack for the immediate close + CI scheduling, but it must
	// be well under the dial delay that the old code would have counted.
	if sinceHandshake := time.Since(handshakeAt); sinceHandshake >= dialDelay {
		t.Errorf("time since handshake = %v, want < dialDelay %v "+
			"(handshakeAt appears stamped before the dial completed)",
			sinceHandshake, dialDelay)
	}
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
	connected, handshakeAt, err := l.connectAndListen(ctx, &fakeHandler{})
	elapsed := time.Since(start)

	if !connected {
		t.Errorf("connected = false, want true (handshake should succeed)")
	}
	if handshakeAt.IsZero() {
		t.Errorf("handshakeAt is zero, want the post-handshake instant")
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

// TestWrapReadError pins the producer side of the disconnect-classification
// contract. classify_test.go verifies ClassifyError given already-wrapped
// sentinels; this verifies wrapReadError PRODUCES the right sentinel from a
// raw conn.Read error, end-to-end through ClassifyError. Without it, a
// regression that wrapped the wrong sentinel would leave classify_test.go
// green while silently corrupting the frozen Loki reason codes (contract
// item 5). Each case also confirms the original cause survives in the error
// chain (the documented double-%w wrap).
func TestWrapReadError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw        error
		wantSent   error
		name       string
		wantReason string
	}{
		{websocket.ErrMessageTooBig, ErrReadLimit, "message too big", ReasonReadLimit},
		{websocket.CloseError{Code: websocket.StatusNormalClosure}, ErrServerClose, "normal closure", ReasonServerClose},
		{websocket.CloseError{Code: websocket.StatusGoingAway}, ErrServerClose, "going away", ReasonServerClose},
		{websocket.CloseError{Code: websocket.StatusAbnormalClosure}, ErrServerClose, "abnormal closure", ReasonServerClose},
		{websocket.CloseError{Code: websocket.StatusProtocolError}, ErrReadError, "non-server-close code", ReasonReadError},
		{io.EOF, ErrServerClose, "plain EOF", ReasonServerClose},
		{errors.New("connection reset"), ErrReadError, "generic transport error", ReasonReadError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := wrapReadError(tc.raw)
			if !errors.Is(got, tc.wantSent) {
				t.Errorf("wrapReadError(%v): errors.Is(_, %v) = false, want true", tc.raw, tc.wantSent)
			}
			if !errors.Is(got, tc.raw) {
				t.Errorf("wrapReadError(%v) dropped the original cause from the error chain", tc.raw)
			}
			if r := ClassifyError(got); r != tc.wantReason {
				t.Errorf("ClassifyError(wrapReadError(%v)) = %q, want %q", tc.raw, r, tc.wantReason)
			}
		})
	}
}

// TestLogDisconnect_LevelAndEscalation pins the two frozen Loki-alerting
// behaviors in logDisconnect (inviolate contract item 5): server_close
// disconnects log at INFO while every other reason logs at WARN, and a
// single ERROR ("websocket reconnecting persistently") escalates once
// consecutive reconnects reach persistentReconnectThreshold. The
// file-marker health never reflects WebSocket state, so this ERROR is the
// only alertable signal that the container is healthy-but-processing-
// nothing; a regression flipping the level branch or the threshold
// comparison would otherwise leave the suite green. Serial (no t.Parallel):
// logDisconnect logs through the process-global slog.Default().
func TestLogDisconnect_LevelAndEscalation(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := NewListener(&fakePlexClient{}, DefaultConfig())
	ctx := context.Background()

	// server_close is INFO; no escalation below the threshold.
	buf.Reset()
	l.logDisconnect(ctx, fmt.Errorf("%w: EOF", ErrServerClose), time.Second, false, 1)
	if got := buf.String(); !strings.Contains(got, "level=INFO") {
		t.Errorf("server_close disconnect: want level=INFO, got %q", got)
	}
	if strings.Contains(buf.String(), "reconnecting persistently") {
		t.Errorf("escalated below threshold: %q", buf.String())
	}

	// dial_failed is WARN.
	buf.Reset()
	l.logDisconnect(ctx, fmt.Errorf("%w: refused", ErrDialFailed), time.Second, false, 1)
	if got := buf.String(); !strings.Contains(got, "level=WARN") {
		t.Errorf("dial_failed disconnect: want level=WARN, got %q", got)
	}

	// At the threshold, a single ERROR escalation fires.
	buf.Reset()
	l.logDisconnect(ctx, fmt.Errorf("%w: refused", ErrDialFailed), 30*time.Second, false, persistentReconnectThreshold)
	if n := strings.Count(buf.String(), "reconnecting persistently"); n != 1 {
		t.Errorf("escalation at threshold fired %d times, want 1: %q", n, buf.String())
	}
	if got := buf.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("escalation: want level=ERROR, got %q", got)
	}
}

func TestBackoffBounds_Defaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		wantMin time.Duration
		wantMax time.Duration
	}{
		{"zero both defaults to 1s/30s", Config{}, time.Second, 30 * time.Second},
		{"negative both defaults to 1s/30s", Config{MinBackoff: -1, MaxBackoff: -5 * time.Second}, time.Second, 30 * time.Second},
		{"zero min keeps positive max", Config{MinBackoff: 0, MaxBackoff: 12 * time.Second}, time.Second, 12 * time.Second},
		{"positive min zero max defaults", Config{MinBackoff: 2 * time.Second, MaxBackoff: 0}, 2 * time.Second, 30 * time.Second},
		{"both positive pass through", Config{MinBackoff: 500 * time.Millisecond, MaxBackoff: 45 * time.Second}, 500 * time.Millisecond, 45 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := NewListener(&fakePlexClient{}, tt.cfg)
			gotMin, gotMax := l.backoffBounds()
			if gotMin != tt.wantMin {
				t.Errorf("backoffBounds() min = %v, want %v", gotMin, tt.wantMin)
			}
			if gotMax != tt.wantMax {
				t.Errorf("backoffBounds() max = %v, want %v", gotMax, tt.wantMax)
			}
		})
	}
}

// TestConnectAndListen_DispatchesReceivedMessage exercises the live
// read->decode->dispatch happy path of connectAndListen end to end: a
// websocket server writes one real "playing" Notification frame and the
// handler must receive exactly one decoded OnPlay. The isolated
// TestDispatch_* tests and FuzzNotificationUnmarshal call dispatch()
// directly and never reach the in-loop dispatch call inside
// connectAndListen, so nulling that call would leave them green while
// breaking event delivery.
func TestConnectAndListen_DispatchesReceivedMessage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		msg := []byte(`{"NotificationContainer":{"type":"playing",` +
			`"PlaySessionStateNotification":[{"ratingKey":"42","state":"playing"}]}}`)
		if err := c.Write(r.Context(), websocket.MessageText, msg); err != nil {
			t.Errorf("server write: %v", err)
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	}))
	defer srv.Close()

	base, _ := url.Parse(srv.URL)
	cfg := DefaultConfig()
	cfg.ReadIdleTimeout = 2 * time.Second
	l := NewListener(&fakePlexClient{base: base, token: "t", client: srv.Client()}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h := &fakeHandler{}
	connected, handshakeAt, err := l.connectAndListen(ctx, h)

	if !connected {
		t.Fatalf("connected = false, want true (handshake should succeed)")
	}
	if handshakeAt.IsZero() {
		t.Fatalf("handshakeAt is zero, want the post-handshake instant")
	}
	if err == nil {
		t.Errorf("expected a server-close disconnect error after the message, got nil")
	}
	if len(h.plays) != 1 {
		t.Fatalf("OnPlay fired %d times, want 1 (received message not decoded+dispatched)", len(h.plays))
	}
	if h.plays[0].RatingKey != "42" || h.plays[0].State != "playing" {
		t.Errorf("dispatched play event = %+v, want RatingKey=42 state=playing", h.plays[0])
	}
	if len(h.timelines) != 0 {
		t.Errorf("OnTimeline fired %d times, want 0", len(h.timelines))
	}
}

// TestConnectAndListen_PinnedCAHandshake is the regression guard for the
// CA-pin loss on the WebSocket dial: a production *plex.Client wraps its
// base transport in a retry round-tripper, so the old dialClient (which
// type-asserted HTTPClient().Transport to *http.Transport) silently fell
// back to a DefaultTransport clone and DISCARDED a PLEX_CA_CERT_PATH pin —
// TLS-pinned deployments could make synchronous API calls but never
// connect the listener. dialClient now derives the dial transport from
// PlexClient.BaseTransport (the hardened base under the retry wrapper),
// so the pinned trust carries into the wss handshake.
//
// The server is a TLS httptest server whose self-signed certificate is
// pinned as the client's sole trust anchor via plex.NewClient's caCertPath
// — the exact production construction shape. With the fix the handshake
// succeeds; with the old fallback it fails TLS verification and
// connected=false.
func TestConnectAndListen_PinnedCAHandshake(t *testing.T) {
	t.Parallel()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			t.Errorf("server accept: %v", err)
			return
		}
		c.Close(websocket.StatusNormalClosure, "")
	}))
	srv.StartTLS()
	defer srv.Close()

	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "plex-ca.pem")
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	client, err := plex.NewClient(srv.URL, "test-token", caPath)
	if err != nil {
		t.Fatalf("NewClient with pinned CA: %v", err)
	}

	cfg := DefaultConfig()
	cfg.ReadIdleTimeout = 2 * time.Second
	l := NewListener(client, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connected, handshakeAt, err := l.connectAndListen(ctx, &fakeHandler{})
	if !connected {
		t.Fatalf("wss handshake over the pinned CA failed (connected=false, err=%v); "+
			"the dial transport dropped the CA pin", err)
	}
	if handshakeAt.IsZero() {
		t.Error("handshakeAt is zero, want the post-handshake instant")
	}
}
