package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/testsupport/alertrules"
)

// PlexLanguageSyncResolutionStalled reads the stall WARN as the production
// handler writes it at LOG_LEVEL=info. Not parallel: it swaps the
// process-global default logger.
func TestResolutionStalledRuleMatchesTheStallLine(t *testing.T) {
	rules := alertrules.Load(t, "alerts/logql.yaml")
	filters := alertrules.LineFilters(t, alertrules.Expr(t, rules, "PlexLanguageSyncResolutionStalled"))

	var buf bytes.Buffer
	h, _ := slogx.NewHandler(slogx.Options{Output: &buf, Level: slog.LevelInfo})
	capture.SwapDefault(t, slog.New(h))
	adapter := &notifyAdapter{cfg: &config{}, resolveStalls: &resolveStallCounter{}}
	ev := notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "client-A"}
	for range resolveStallThreshold {
		adapter.skipUnattributedPlayEvent(ev, errSessionsUnreadable)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !alertrules.Match(lines[0], filters) {
		t.Errorf("want one stall line satisfying the rule's filters %+v; log:\n%s", filters, buf.String())
	}
}
