package obs_test

import (
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/testsupport/metrictest"
)

func TestNewSeedsEveryLabelledSeriesAtZero(t *testing.T) {
	t.Parallel()
	m := obs.New("test")
	for _, s := range []struct {
		family string
		labels []string
	}{
		{"plex_language_sync_deep_scan_runs_total", []string{"complete"}},
		{"plex_language_sync_deep_scan_runs_total", []string{"incomplete"}},
		{"plex_language_sync_user_resolution_stalled", []string{obs.StallSessionsUnreadable}},
		{"plex_language_sync_user_resolution_stalled", []string{obs.StallAllClientsAbsent}},
		{"plex_language_sync_play_events_total", []string{"attributed"}},
		{"plex_language_sync_play_events_total", []string{"skipped"}},
		{"plex_language_sync_track_updates_total", []string{"audio", "set"}},
		{"plex_language_sync_track_updates_total", []string{"audio", "failed"}},
		{"plex_language_sync_track_updates_total", []string{"subtitle", "set"}},
		{"plex_language_sync_track_updates_total", []string{"subtitle", "failed"}},
	} {
		if got := metrictest.Value(t, m, s.family, s.labels...); got != 0 {
			t.Errorf("%s%q = %v at start, want 0", s.family, s.labels, got)
		}
	}
}

func TestDeepScanFinished(t *testing.T) {
	t.Parallel()
	m := obs.New("test")
	at := time.Unix(1_800_000_000, 0)
	m.DeepScanFinished(true, at)
	m.DeepScanFinished(false, at.Add(time.Hour))

	if got := metrictest.Value(t, m, "plex_language_sync_deep_scan_last_success_timestamp_seconds"); got != float64(at.Unix()) {
		t.Errorf("last success = %v, want %d: an incomplete pass must not move it", got, at.Unix())
	}
	if got := metrictest.Value(t, m, "plex_language_sync_deep_scan_runs_total", "complete"); got != 1 {
		t.Errorf("complete runs = %v, want 1", got)
	}
	if got := metrictest.Value(t, m, "plex_language_sync_deep_scan_runs_total", "incomplete"); got != 1 {
		t.Errorf("incomplete runs = %v, want 1", got)
	}
}

func TestResolutionStallRaisesOneCauseAndRecoveryClearsAll(t *testing.T) {
	t.Parallel()
	m := obs.New("test")
	m.ResolutionStalled(obs.StallAllClientsAbsent)
	if got := metrictest.Value(t, m, "plex_language_sync_user_resolution_stalled", obs.StallAllClientsAbsent); got != 1 {
		t.Errorf("stalled{all_clients_absent} = %v after the stall, want 1", got)
	}
	if got := metrictest.Value(t, m, "plex_language_sync_user_resolution_stalled", obs.StallSessionsUnreadable); got != 0 {
		t.Errorf("stalled{sessions_unreadable} = %v, want 0", got)
	}
	m.ResolutionRecovered()
	if got := metrictest.Value(t, m, "plex_language_sync_user_resolution_stalled", obs.StallAllClientsAbsent); got != 0 {
		t.Errorf("stalled{all_clients_absent} = %v after recovery, want 0", got)
	}
}

func TestDeepScanIntervalInSeconds(t *testing.T) {
	t.Parallel()
	m := obs.New("test")
	m.DeepScanInterval(6 * time.Hour)
	if got := metrictest.Value(t, m, "plex_language_sync_deep_scan_interval_seconds"); got != 21600 {
		t.Errorf("interval = %v, want 21600", got)
	}
}

func TestBuildInfoCarriesTheVersion(t *testing.T) {
	t.Parallel()
	m := obs.New("v1.2.3")
	if got := metrictest.Value(t, m, "plex_language_sync_build_info", "v1.2.3"); got != 1 {
		t.Errorf("build_info{version=v1.2.3} = %v, want 1", got)
	}
}

// The deadline is one gap after the last complete scan: a complete pass moves
// it, an incomplete one leaves it, and with the scan off it stays 0.
func TestDeepScanDeadline(t *testing.T) {
	t.Parallel()
	const family = "plex_language_sync_deep_scan_deadline_timestamp_seconds"
	gap := 50 * time.Hour
	from := time.Unix(1_800_000_000, 0)
	t.Run("scheduled", func(t *testing.T) {
		t.Parallel()
		m := obs.New("test")
		if got := metrictest.Value(t, m, family); got != 0 {
			t.Errorf("deadline before the schedule is known = %v, want 0", got)
		}
		m.DeepScanDue(gap, from)
		if got, want := metrictest.Value(t, m, family), float64(from.Add(gap).Unix()); got != want {
			t.Errorf("deadline = %v, want %v, one gap after %v", got, want, from)
		}
		m.DeepScanFinished(false, from.Add(time.Hour))
		if got, want := metrictest.Value(t, m, family), float64(from.Add(gap).Unix()); got != want {
			t.Errorf("deadline after an incomplete pass = %v, want %v unchanged", got, want)
		}
		done := from.Add(30 * time.Hour)
		m.DeepScanFinished(true, done)
		if got, want := metrictest.Value(t, m, family), float64(done.Add(gap).Unix()); got != want {
			t.Errorf("deadline after a complete pass = %v, want %v, one gap after it", got, want)
		}
	})
	t.Run("scan off", func(t *testing.T) {
		t.Parallel()
		m := obs.New("test")
		m.DeepScanDue(0, from)
		m.DeepScanFinished(true, from)
		if got := metrictest.Value(t, m, family); got != 0 {
			t.Errorf("deadline with the scan off = %v, want 0", got)
		}
	})
}
