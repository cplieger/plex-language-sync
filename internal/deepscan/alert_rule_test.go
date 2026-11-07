package deepscan

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/testsupport/alertrules"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// passLog runs one deep-analysis pass against plx and returns what the
// production handler wrote at LOG_LEVEL=info, the level the LogQL rules need.
func passLog(t *testing.T, plx plexReader) []string {
	t.Helper()
	var buf bytes.Buffer
	h, _ := slogx.NewHandler(slogx.Options{Output: &buf, Level: slog.LevelInfo})
	capture.SwapDefault(t, slog.New(h))
	sched := New(Config{Enable: true}, &Deps{
		Plex: plx, Cache: fakeapi.NewCache(), Stamp: testStamp(t),
		UserClient: func(string) EpisodeReader { return nil }, Sync: &fakeSyncer{},
	})
	sched.deepAnalysisCore(t.Context())
	return strings.Split(strings.TrimSpace(buf.String()), "\n")
}

// The deadman of alerts/logql.yaml resets only on a complete pass: an aborted
// pass leaves its window unswept, so it must not count as a heartbeat. Not
// parallel: it swaps the process-global default logger.
func TestDeepScanStalledRuleMatchesCompletePassesOnly(t *testing.T) {
	rules := alertrules.Load(t, "../../alerts/logql.yaml")
	filters := alertrules.LineFilters(t, alertrules.Expr(t, rules, "PlexLanguageSyncDeepScanStalled"))

	tests := []struct {
		plx  plexReader
		name string
		want int
	}{
		{name: "complete pass", plx: &fakeapi.Plex{}, want: 1},
		{name: "incomplete pass", plx: &fetchErrPlex{Plex: &fakeapi.Plex{}, historyErr: errors.New("boom")}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := passLog(t, tt.plx)
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
