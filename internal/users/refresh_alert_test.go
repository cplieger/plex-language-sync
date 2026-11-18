package users

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/testsupport/alertrules"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/testsupport/metrictest"
	"github.com/cplieger/plex-language-sync/internal/testsupport/plexclient"
)

const (
	refreshLast     = "plex_language_sync_token_refresh_last_success_timestamp_seconds"
	refreshDeadline = "plex_language_sync_token_refresh_deadline_timestamp_seconds"
)

// flakyTV answers the shared-server list with one shared user, or 500 while
// failing is set.
func flakyTV(t *testing.T, failing *atomic.Bool) *plex.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer><SharedServer id="1" username="friend1" userID="100" accessToken="token-100"/></MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	parsed, _ := url.Parse("http://plex:32400")
	return plexclient.NewFromHTTP(parsed, "admin-token", plexclient.Options{HTTP: &http.Client{}, TV: plexTVClientFor(srv)})
}

func within(got float64, from, to time.Time) bool {
	return got >= float64(from.Unix()) && got <= float64(to.Unix())
}

// The deadline starts two refresh intervals after the first refresh, a
// success moves it and the last-success time, and a failure moves neither.
func TestTokenRefreshMetricsFollowSuccesses(t *testing.T) {
	t.Parallel()
	var failing atomic.Bool
	failing.Store(true)
	client := flakyTV(t, &failing)
	metrics := obs.New("test")
	m := NewManager(fakeapi.NewCache(), metrics)
	m.Init(&plex.User{ID: "1", Name: "admin"})

	start := time.Now()
	m.InitialRefreshWithRetry(t.Context(), client, "machine-id-123", RefreshConfig{MaxAttempts: 1})
	end := time.Now()
	if got := metrictest.Value(t, metrics, refreshLast); got != 0 {
		t.Errorf("last success after a failed refresh = %v, want 0", got)
	}
	startDeadline := metrictest.Value(t, metrics, refreshDeadline)
	if !within(startDeadline, start.Add(24*time.Hour), end.Add(24*time.Hour)) {
		t.Errorf("deadline after a failed first refresh = %v, want 24h after the start (%d)", startDeadline, start.Add(24*time.Hour).Unix())
	}

	failing.Store(false)
	before := time.Now()
	if err := m.refreshTokens(t.Context(), client, "machine-id-123"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if got := metrictest.Value(t, metrics, refreshLast); !within(got, before, after) {
		t.Errorf("last success = %v, want the refresh's time (%d)", got, before.Unix())
	}
	okDeadline := metrictest.Value(t, metrics, refreshDeadline)
	if !within(okDeadline, before.Add(24*time.Hour), after.Add(24*time.Hour)) {
		t.Errorf("deadline after a success = %v, want 24h after it (%d)", okDeadline, before.Add(24*time.Hour).Unix())
	}

	failing.Store(true)
	_ = m.refreshTokens(t.Context(), client, "machine-id-123")
	if got := metrictest.Value(t, metrics, refreshDeadline); got != okDeadline {
		t.Errorf("deadline after a failure = %v, want %v unchanged", got, okDeadline)
	}
}

// refreshLog runs one refresh against client and returns what the production
// handler wrote at LOG_LEVEL=info, the level the LogQL rules need.
func refreshLog(t *testing.T, m *Manager, client *plex.Client) []string {
	t.Helper()
	var buf bytes.Buffer
	h, _ := slogx.NewHandler(slogx.Options{Output: &buf, Level: slog.LevelInfo})
	capture.SwapDefault(t, slog.New(h))
	_ = m.refreshTokens(t.Context(), client, "machine-id-123")
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

// The log rule's deadman resets only on a successful refresh. Not parallel:
// it swaps the process-global default logger.
func TestTokenRefreshStalledRuleMatchesSuccessesOnly(t *testing.T) {
	rules := alertrules.Load(t, "../../alerts/logql.yaml")
	filters := alertrules.LineFilters(t, alertrules.Expr(t, rules, "PlexLanguageSyncTokenRefreshStalled"))
	for _, tt := range []struct {
		name    string
		failing bool
		want    int
	}{
		{name: "success", want: 1},
		{name: "failure", failing: true, want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var failing atomic.Bool
			failing.Store(tt.failing)
			m := NewManager(fakeapi.NewCache(), nil)
			m.Init(&plex.User{ID: "1", Name: "admin"})
			lines := refreshLog(t, m, flakyTV(t, &failing))
			got := 0
			for _, line := range lines {
				if alertrules.Match(line, filters) {
					got++
				}
			}
			if got != tt.want {
				t.Errorf("%d lines satisfy the deadman's filters %+v, want %d; log:\n%s", got, filters, tt.want, strings.Join(lines, "\n"))
			}
		})
	}
}

// A success after failures logs the recovery once, with how long refreshes
// failed; a success after a success logs none. Not parallel: it swaps the
// process-global default logger.
func TestTokenRefreshRecoveryIsLoggedOnce(t *testing.T) {
	var failing atomic.Bool
	client := flakyTV(t, &failing)
	m := NewManager(fakeapi.NewCache(), nil)
	m.Init(&plex.User{ID: "1", Name: "admin"})

	failing.Store(true)
	_ = refreshLog(t, m, client)
	failing.Store(false)
	recovered := strings.Join(refreshLog(t, m, client), "\n")
	if strings.Count(recovered, "shared user token refresh recovered") != 1 || !strings.Contains(recovered, "duration=") {
		t.Errorf("first success after a failure must log one recovery line with its duration; log:\n%s", recovered)
	}
	again := strings.Join(refreshLog(t, m, client), "\n")
	if strings.Contains(again, "recovered") {
		t.Errorf("a success after a success logged a recovery; log:\n%s", again)
	}
}
