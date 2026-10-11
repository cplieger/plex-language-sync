package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/testsupport/plexclient"
	"github.com/cplieger/plex-language-sync/internal/users"
	"github.com/cplieger/runesafe/v2"
	"github.com/cplieger/slogx/capture"
)

// hostile carries a line break that would start a forged record in a
// line-oriented sink, a bidi override that reorders what a reader sees,
// and an ANSI escape that a terminal would execute.
const hostile = "1\nlevel=ERROR msg=forged \u202eevil\x1b[2J"

// assertNoUnsafeRunes fails for every captured attribute value that still
// carries a rune runesafe's single-line policy replaces.
func assertNoUnsafeRunes(t *testing.T, rec *capture.Recorder) {
	t.Helper()
	for _, r := range rec.Records() {
		r.Attrs(func(a slog.Attr) bool {
			if v := a.Value.String(); runesafe.SanitizeSingleLine(v) != v {
				t.Errorf("%q logged %s=%q unsanitized; a value from the notification stream reached the log raw", r.Message, a.Key, v)
			}
			return true
		})
	}
}

func sessionServer(t *testing.T, userID, title, client string) *plex.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"User":{"id":"` + userID + `","title":"` + title + `"},"Player":{"machineIdentifier":"` + client + `"}}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	return plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()})
}

func TestHandlePlayEvent_sanitizesNotificationValuesInLogs(t *testing.T) {
	const jsonHostileTitle = `bob\nlevel=ERROR \u202eevil`
	tests := []struct {
		name    string
		userID  string
		wantMsg string
	}{
		// The admin resolves to the admin client, so the episode fetch runs
		// and fails on the hostile rating key.
		{name: "episode fetch fails", userID: "1", wantMsg: "play event: failed to fetch episode"},
		// A user the manager does not know has no per-user client.
		{name: "no per-user client", userID: "9", wantMsg: "play event: no per-user client available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			c := cache.New()
			mgr := users.NewManager(c)
			mgr.Init(&plex.User{ID: "1", Name: "admin"})
			adapter := &notifyAdapter{
				cfg:    &config{triggerOnPlay: true},
				users:  mgr,
				client: sessionServer(t, tt.userID, jsonHostileTitle, "mac-A"),
				cache:  c,
			}

			adapter.OnPlay(t.Context(), notify.PlayEvent{State: "playing", RatingKey: hostile, ClientIdentifier: "mac-A"})

			if !rec.HasAttr(tt.wantMsg, "key", runesafe.SanitizeSingleLine(hostile)) {
				t.Fatalf("no %q record carrying the sanitized rating key; got %v", tt.wantMsg, rec.Messages())
			}
			if !rec.HasAttr(tt.wantMsg, "user", "bob level=ERROR  evil") {
				t.Errorf("%q did not carry the session user name sanitized to one line", tt.wantMsg)
			}
			assertNoUnsafeRunes(t, rec)
		})
	}
}

func TestSkipUnattributedPlayEvent_sanitizesNotificationValuesInLogs(t *testing.T) {
	rec := capture.Default(t)
	adapter := &notifyAdapter{cfg: &config{}, resolveStalls: &resolveStallCounter{}}

	// Distinct hostile clients reach the absence threshold, so both the
	// per-event Debug and the stall Warn are emitted.
	for i := range resolveAbsentPairThreshold {
		client := hostile + strings.Repeat("x", i)
		adapter.skipUnattributedPlayEvent(
			notify.PlayEvent{State: hostile, RatingKey: hostile, ClientIdentifier: client},
			absentFor(client),
		)
	}

	if rec.CountLevel(slog.LevelDebug, "play event: skipping, user not attributed") != resolveAbsentPairThreshold {
		t.Fatalf("skip records = %v, want %d per-event Debug lines", rec.Messages(), resolveAbsentPairThreshold)
	}
	if !rec.HasAttr("user resolution stalled", "last_key", runesafe.SanitizeSingleLine(hostile)) {
		t.Fatalf("the stall Warn did not carry the sanitized last key; got %v", rec.Messages())
	}
	assertNoUnsafeRunes(t, rec)
}

func TestHandleTimeline_sanitizesItemIDInLogs(t *testing.T) {
	rec := capture.Default(t)
	adapter := newTestAdapter(t, false, true)

	adapter.OnTimeline(t.Context(), []notify.TimelineEntry{{ItemID: hostile, Type: 4, MetadataState: "created"}})

	if !rec.HasAttr("timeline: failed to fetch episode", "id", runesafe.SanitizeSingleLine(hostile)) {
		t.Fatalf("no fetch-failure record carrying the sanitized item id; got %v", rec.Messages())
	}
	assertNoUnsafeRunes(t, rec)
}
