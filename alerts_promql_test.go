package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/testsupport/alertrules"
)

// logqlSuccessors maps every alert alerts/logql.yaml ships to the alerts of
// alerts/promql.yaml that cover its condition, so dropping a condition from
// either file is a decision in this table. PlexLanguageSyncErrorLog has no
// metric form; the metric rules for its causes cover it.
var logqlSuccessors = map[string][]string{
	"PlexLanguageSyncDeepScanStalled":   {"PlexLanguageSyncDeepScanStalled"},
	"PlexLanguageSyncResolutionStalled": {"PlexLanguageSyncResolutionStalled"},
	"PlexLanguageSyncErrorLog":          {"PlexLanguageSyncDown", "PlexLanguageSyncWebSocketDown"},
}

// promqlOnly are the metric alerts with no log form: a dead process or an
// absent target leaves no line, and a cache save failing during a deep scan
// logs only a WARN that no log rule reads.
var promqlOnly = []string{"PlexLanguageSyncDown", "PlexLanguageSyncCacheSaveFailed"}

var exposedType = regexp.MustCompile(`(?m)^# TYPE (\S+) (\S+)$`)

// exportedSeries is every series the registry run builds serves, histogram
// suffixes included, plus the scrape's own up.
func exportedSeries(t *testing.T) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	obs.New("test").Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	body, err := io.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"up"}
	for _, m := range exposedType.FindAllStringSubmatch(string(body), -1) {
		names = append(names, m[1])
		if m[2] == "histogram" {
			names = append(names, m[1]+"_bucket", m[1]+"_sum", m[1]+"_count")
		}
	}
	return names
}

func TestPromQLRulesReadExportedSeries(t *testing.T) {
	t.Parallel()
	series := exportedSeries(t)
	for _, r := range alertrules.Load(t, "alerts/promql.yaml") {
		for _, name := range alertrules.SeriesNames(r.Expr) {
			if !slices.Contains(series, name) {
				t.Errorf("%s reads %s, which plex-language-sync does not export", r.Alert, name)
			}
		}
	}
}

func TestAlertParity(t *testing.T) {
	t.Parallel()
	var promql []string
	for _, r := range alertrules.Load(t, "alerts/promql.yaml") {
		promql = append(promql, r.Alert)
	}
	covered := slices.Clone(promqlOnly)
	for _, r := range alertrules.Load(t, "alerts/logql.yaml") {
		successors, ok := logqlSuccessors[r.Alert]
		if !ok {
			t.Errorf("%s in alerts/logql.yaml has no entry in logqlSuccessors", r.Alert)
		}
		for _, s := range successors {
			if !slices.Contains(promql, s) {
				t.Errorf("%s names %s as its successor, which alerts/promql.yaml does not hold", r.Alert, s)
			}
		}
		covered = append(covered, successors...)
	}
	for _, a := range promql {
		if !slices.Contains(covered, a) {
			t.Errorf("%s in alerts/promql.yaml is neither a successor nor listed in promqlOnly", a)
		}
	}
	for _, a := range promqlOnly {
		if !slices.Contains(promql, a) {
			t.Errorf("promqlOnly names %s, which alerts/promql.yaml does not hold", a)
		}
	}
}
