package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeLoop is a liveness entry that records its beats and whether it is parked.
type fakeLoop struct {
	mu     sync.Mutex
	beats  int
	parked bool
}

func (f *fakeLoop) Beat() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats++
	f.parked = false
}

func (f *fakeLoop) Park() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parked = true
}

func (f *fakeLoop) state() (beats int, parked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats, f.parked
}

// An unreachable Plex is the reconnect loop's work: every attempt beats it, and
// the read loop, which never runs, stays parked.
func TestListen_UnreachablePlexBeatsTheReconnectLoop(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("http://127.0.0.1:1/")
	read, reconnect := &fakeLoop{parked: true}, &fakeLoop{parked: true}
	cfg := Config{MinBackoff: time.Millisecond, MaxBackoff: time.Millisecond, ReadLoop: read, ReconnectLoop: reconnect}
	l := NewListener(&fakePlexClient{base: base, token: "t"}, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	drops := 0
	l.Listen(ctx, &fakeHandler{onConnection: func(connected bool) {
		if !connected {
			if drops++; drops == 3 {
				cancel()
			}
		}
	}})

	if beats, parked := reconnect.state(); beats < 3 || parked {
		t.Errorf("reconnect loop after 3 failed dials: beats=%d parked=%v, want at least 3 beats and armed", beats, parked)
	}
	if beats, parked := read.state(); beats != 0 || !parked {
		t.Errorf("read loop with no connection: beats=%d parked=%v, want 0 beats and parked", beats, parked)
	}
}

// Only one of Listen's loops runs at a time, so the handshake hands liveness
// to the read loop, every read beats it, and a drop hands it back.
func TestListen_HandsLivenessBetweenItsLoops(t *testing.T) {
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
	read, reconnect := &fakeLoop{parked: true}, &fakeLoop{parked: true}
	cfg := DefaultConfig()
	cfg.MinBackoff, cfg.MaxBackoff = time.Millisecond, time.Millisecond
	cfg.ReadLoop, cfg.ReconnectLoop = read, reconnect
	l := NewListener(&fakePlexClient{base: base, token: "t", client: srv.Client()}, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	type snapshot struct {
		readBeats                   int
		readParked, reconnectParked bool
	}
	take := func() snapshot {
		rb, rp := read.state()
		_, cp := reconnect.state()
		return snapshot{readBeats: rb, readParked: rp, reconnectParked: cp}
	}
	var atConnect, atPlay, atDrop snapshot
	h := &fakeHandler{
		onConnection: func(connected bool) {
			if connected {
				atConnect = take()
				return
			}
			atDrop = take()
			cancel()
		},
		onPlay: func() { atPlay = take() },
	}
	l.Listen(ctx, h)

	if want := (snapshot{readBeats: 1, readParked: false, reconnectParked: true}); atConnect != want {
		t.Errorf("at the handshake: %+v, want %+v", atConnect, want)
	}
	if atPlay.readBeats != 2 {
		t.Errorf("read beats when the first message is handled = %d, want 2 (handshake and one read)", atPlay.readBeats)
	}
	if !atDrop.readParked || atDrop.reconnectParked {
		t.Errorf("after the drop: read parked=%v reconnect parked=%v, want the read loop parked and the reconnect loop armed",
			atDrop.readParked, atDrop.reconnectParked)
	}
}
