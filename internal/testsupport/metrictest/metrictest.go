// Package metrictest reads one series of an obs.Metrics for tests, from the
// page its Handler serves. Import from _test.go files only.
package metrictest

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/obs"
)

// Value returns the value of the series of family whose label values are
// labelValues, in the family's declared order. It fails the test when the
// family or the series does not exist. Label values must hold no quote,
// comma or backslash, which every obs label set satisfies.
func Value(t testing.TB, m *obs.Metrics, family string, labelValues ...string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	seen := false
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, raw, ok := strings.Cut(line, " ")
		if !ok {
			t.Fatalf("malformed sample line %q", line)
		}
		name, labels, _ := strings.Cut(series, "{")
		if name != family {
			continue
		}
		seen = true
		if !slices.Equal(labelValuesOf(t, strings.TrimSuffix(labels, "}")), labelValues) {
			continue
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		return v
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatalf("%s has no series %q", family, labelValues)
	}
	t.Fatalf("no family %s", family)
	return 0
}

func labelValuesOf(t testing.TB, labels string) []string {
	t.Helper()
	if labels == "" {
		return nil
	}
	pairs := strings.Split(labels, ",")
	vals := make([]string, len(pairs))
	for i, p := range pairs {
		_, quoted, ok := strings.Cut(p, "=")
		if !ok || len(quoted) < 2 || quoted[0] != '"' || quoted[len(quoted)-1] != '"' {
			t.Fatalf("malformed label pair %q", p)
		}
		vals[i] = quoted[1 : len(quoted)-1]
	}
	return vals
}
