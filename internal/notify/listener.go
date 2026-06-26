package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

// wsTypeTimeline is the NotificationContainer.Type value Plex uses for
// library scan events. Kept unexported; the Listener routes based on it
// internally and the value is part of the Plex JSON wire format
// (inviolate item 9).
const wsTypeTimeline = "timeline"

// wsReadLimitBytes is the per-message size cap enforced by the
// WebSocket layer. Preserves the main.go behaviour (1 MB).
const wsReadLimitBytes = 1 << 20

// persistentReconnectThreshold is the number of consecutive non-stable
// reconnect attempts after which Listen escalates from the per-cycle
// Warn line to a single ERROR. By this depth the backoff has reached
// MaxBackoff (default 30s) and the outage is clearly sustained (Plex
// unreachable, token revoked, network partition) rather than a transient
// blip. The file-marker health intentionally never reflects WebSocket
// state, so this ERROR is the only alertable signal that the container is
// healthy-but-processing-nothing.
const persistentReconnectThreshold = 5

// Handler receives decoded events from the Listener. Implementations
// live in the composition root (main package) and typically fan out
// per-event work to the sync subsystem.
//
// OnPlay is called for each relevant PlaySessionStateNotification
// in a received Notification. OnTimeline is called once per
// Notification with the full TimelineEntry slice so the caller can
// apply its own per-entry dedup or batching policy.
//
// The Handler is invoked synchronously from the read loop; a slow
// handler delays the next Read and can trigger the keepalive timeout.
// Handlers that need to perform long work should hand it off to a
// goroutine or worker pool.
type Handler interface {
	OnPlay(ctx context.Context, ev PlayEvent)
	OnTimeline(ctx context.Context, entries []TimelineEntry)
}

// PlexClient is the subset of an HTTP-based Plex client the Listener
// needs. Satisfied by *plex.Client; declared here to keep notify
// decoupled from the plex package (no import cycle, easy to fake in
// tests).
type PlexClient interface {
	BaseURL() *url.URL
	Token() string
	HTTPClient() *http.Client
}

// Listener dials Plex's /:/websockets/notifications endpoint, decodes
// the JSON envelope, and delivers events to a Handler with an outer
// reconnect loop and stable-connection backoff reset.
type Listener struct {
	client PlexClient
	cfg    Config
}

// NewListener builds a Listener from a Plex client and a Config.
// Production callers pass DefaultConfig(); tests pass a shrunk Config
// so the reconnect-loop assertions run quickly.
func NewListener(client PlexClient, cfg Config) *Listener {
	return &Listener{client: client, cfg: cfg}
}

// Listen runs the reconnect loop: dial the WebSocket, decode and
// dispatch messages until a read error or the context is cancelled,
// then wait for a bounded backoff and reconnect. Returns when the
// supplied context is cancelled.
func (l *Listener) Listen(ctx context.Context, h Handler) {
	defer slog.Info("websocket listener stopped")

	backoff := l.cfg.MinBackoff
	reconnecting := false
	reconnects := 0

	for {
		if ctx.Err() != nil {
			return
		}
		if reconnecting {
			reconnects++
			slog.Info("attempting websocket reconnect",
				"attempt", reconnects,
				"backoff", backoff.String())
		}
		connectStart := time.Now()
		connected, err := l.connectAndListen(ctx, h)
		if ctx.Err() != nil {
			return
		}
		// Only reset backoff if the connection was held open long enough
		// to consider it stable. Avoids the "accept handshake then
		// immediately close" flap hammering Plex at MinBackoff
		// indefinitely.
		stable := connected && time.Since(connectStart) > l.cfg.StableThreshold
		backoff = nextBackoff(backoff, l.cfg.MinBackoff, l.cfg.MaxBackoff, stable)
		if stable {
			reconnects = 0
		}

		// Clean cancellation is not noteworthy; every other disconnect is
		// logged (and escalated when it persists).
		if errors.Is(err, context.Canceled) {
			return
		}
		l.logDisconnect(ctx, err, backoff, stable, reconnects)

		delay := time.NewTimer(backoff)
		select {
		case <-delay.C:
		case <-ctx.Done():
			delay.Stop()
			return
		}
		reconnecting = true
	}
}

// logDisconnect emits the per-cycle disconnect log at the level the
// disconnect reason warrants and escalates to a single ERROR once
// consecutive reconnects pile up past the threshold. Clean server-close
// is expected info-level; dial/read errors stay warnings. A sustained
// outage leaves the file-marker health green while zero events are
// processed, so the escalation is the only alertable signal that the
// container is healthy-but-processing-nothing. Split out of Listen to
// keep the reconnect loop within the cognitive-complexity budget; the
// log messages and keys are unchanged (inviolate item 5).
func (l *Listener) logDisconnect(ctx context.Context, err error, backoff time.Duration, stable bool, reconnects int) {
	level := slog.LevelWarn
	reason := ClassifyError(err)
	if reason == ReasonServerClose {
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "websocket disconnected, reconnecting",
		"reason", reason,
		"error", err,
		"backoff", backoff.String(),
		"stable", stable)

	if reconnects >= persistentReconnectThreshold {
		slog.Error("websocket reconnecting persistently",
			"attempt", reconnects,
			"reason", reason,
			"error", err)
	}
}

// connectAndListen performs a single dial + read-loop cycle. Returns
// (connected, err). `connected` is true iff the WebSocket handshake
// succeeded — the outer loop uses it to decide whether to treat the
// elapsed time as a stability signal.
func (l *Listener) connectAndListen(ctx context.Context, h Handler) (bool, error) {
	base := l.client.BaseURL()
	scheme := "ws"
	if base.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := url.URL{
		Scheme: scheme,
		Host:   base.Host,
		Path:   "/:/websockets/notifications",
	}
	slog.Debug("connecting to websocket", "url", wsURL.String())

	opts := &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Plex-Token": {l.client.Token()}},
		HTTPClient: l.dialClient(),
	}

	conn, resp, err := websocket.Dial(ctx, wsURL.String(), opts)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrDialFailed, err)
	}
	defer func() {
		if err := conn.CloseNow(); err != nil {
			slog.Debug("websocket close error", "error", err)
		}
	}()

	slog.Info("websocket connected", "host", base.Host)

	// Limit WebSocket message size to prevent OOM from oversized messages.
	conn.SetReadLimit(wsReadLimitBytes)

	// Dead-connection detection layering:
	//   1. TCP keepalive (set on the dialer below at 30s probe interval).
	//      Detects truly dead sockets within ~90s without disrupting
	//      idle-but-alive connections.
	//   2. ReadIdleTimeout (Config-driven, default 1 hour) is a backstop
	//      for the rare case where the OS reports the socket alive but
	//      the server has silently stopped sending. Plex doesn't send
	//      heartbeats and may legitimately be quiet for tens of minutes
	//      during off-peak windows; a short timeout here only churns
	//      the connection without improving correctness.
	idle := l.cfg.ReadIdleTimeout
	if idle <= 0 {
		idle = time.Hour
	}
	for {
		readCtx, cancelRead := context.WithDeadline(ctx, time.Now().Add(idle))
		_, message, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			return true, wrapReadError(readErr)
		}
		var notif Notification
		if jsonErr := json.Unmarshal(message, &notif); jsonErr != nil {
			slog.Debug("invalid websocket message", "error", jsonErr)
			continue
		}
		dispatch(ctx, h, &notif)
	}
}

// dialClient returns the HTTP client used to perform the websocket
// upgrade. Wraps the Plex client's Transport with a custom net.Dialer
// that enables TCP keepalive (30s probe interval). Stdlib's
// http.DefaultTransport sets KeepAlive: 30s by default, but the
// Plex client may install a custom Transport (for skipTLS) that
// inherits the zero-value Dialer with NO keepalive — overriding the
// DialContext here is belt-and-suspenders so this listener works
// regardless of how the Plex client built its transport.
func (l *Listener) dialClient() *http.Client {
	src := l.client.HTTPClient()
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	// Clone or build a transport with our DialContext so we don't
	// mutate the shared Plex client's transport (which is also used
	// for synchronous API calls with their own per-request timeouts).
	var t *http.Transport
	if src.Transport != nil {
		if existing, ok := src.Transport.(*http.Transport); ok {
			t = existing.Clone()
		}
	}
	if t == nil {
		t = http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck // stdlib guarantee: DefaultTransport is *http.Transport
	}
	t.DialContext = dialer.DialContext
	return &http.Client{
		Transport: t,
		// No overall Timeout: it would apply to the upgrade response
		// body which the websocket library hijacks. The dialer's own
		// Timeout above bounds the connect phase.
		CheckRedirect: src.CheckRedirect,
	}
}

// wrapReadError wraps a raw conn.Read error with a typed sentinel so
// ClassifyError can match without substring search. The returned error
// still wraps readErr (double %w) so callers retain access to the
// original cause via errors.Unwrap chains.
func wrapReadError(readErr error) error {
	// Read-limit exceeded: the websocket library wraps
	// websocket.ErrMessageTooBig when the frame exceeds SetReadLimit.
	if errors.Is(readErr, websocket.ErrMessageTooBig) {
		slog.Warn("websocket message exceeded read limit",
			"limit_bytes", wsReadLimitBytes, "error", readErr)
		return fmt.Errorf("%w: %w", ErrReadLimit, readErr)
	}
	// Clean server-close signals: close frames (normal/going-away/
	// abnormal) via typed CloseError, or plain io.EOF.
	var ce websocket.CloseError
	if errors.As(readErr, &ce) {
		switch ce.Code {
		case websocket.StatusNormalClosure,
			websocket.StatusGoingAway,
			websocket.StatusAbnormalClosure:
			return fmt.Errorf("%w: %w", ErrServerClose, readErr)
		}
	}
	if errors.Is(readErr, io.EOF) {
		return fmt.Errorf("%w: %w", ErrServerClose, readErr)
	}
	return fmt.Errorf("%w: %w", ErrReadError, readErr)
}

// dispatch routes a decoded Notification to the Handler.
func dispatch(ctx context.Context, h Handler, notif *Notification) {
	switch notif.NotificationContainer.Type {
	case statePlaying:
		for _, ev := range notif.NotificationContainer.PlaySessionStateNotification {
			h.OnPlay(ctx, ev)
		}
	case wsTypeTimeline:
		h.OnTimeline(ctx, notif.NotificationContainer.TimelineEntry)
	}
}
