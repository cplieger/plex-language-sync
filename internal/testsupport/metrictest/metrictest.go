// Package metrictest reads one series of an obs.Metrics for tests. Import from
// _test.go files only.
package metrictest

import (
	"slices"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/obs"
)

// Value returns the value of the series of family whose label values are
// labelValues, in the family's declared order. It fails the test when the
// family or the series does not exist.
func Value(t testing.TB, m *obs.Metrics, family string, labelValues ...string) float64 {
	t.Helper()
	fams, err := m.Gather(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.Name != family {
			continue
		}
		for _, s := range f.Samples {
			vals := make([]string, len(s.Labels))
			for i, l := range s.Labels {
				vals[i] = l.Value
			}
			if slices.Equal(vals, labelValues) {
				return s.Value
			}
		}
		t.Fatalf("%s has no series %q", family, labelValues)
	}
	t.Fatalf("no family %s", family)
	return 0
}
