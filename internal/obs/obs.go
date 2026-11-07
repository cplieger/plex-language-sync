// Package obs defines plex-language-sync's Prometheus metrics, served on the
// opt-in METRICS_LISTEN_ADDR listener under the prefix plex_language_sync_.
// Every label takes a value from a closed set this package names, and every
// labelled series is seeded at 0 so its first event shows in increase().
//
// A nil *Metrics records nothing, so a collaborator built without one works.
package obs

import (
	"net/http"
	"time"

	"github.com/cplieger/go/measure"
)

// TrackKind is the kind label of the track-update counter.
type TrackKind string

// The kinds of stream a track update writes.
const (
	TrackAudio    TrackKind = "audio"
	TrackSubtitle TrackKind = "subtitle"
)

// The causes of a user-resolution stall, the cause label of its gauge.
const (
	StallSessionsUnreadable = "sessions_unreadable"
	StallAllClientsAbsent   = "all_clients_absent"
)

var (
	trackKinds  = []TrackKind{TrackAudio, TrackSubtitle}
	stallCauses = []string{StallSessionsUnreadable, StallAllClientsAbsent}
)

// Metrics holds the app's instruments and the registry that serves them.
type Metrics struct {
	reg               *measure.Registry
	wsConnected       *measure.Gauge
	wsReconnects      *measure.Counter
	scanLastSuccess   *measure.Gauge
	scanInterval      *measure.Gauge
	scanRuns          *measure.LabeledCounter
	resolutionStalled *measure.LabeledGauge
	playEvents        *measure.LabeledCounter
	trackUpdates      *measure.LabeledCounter
	cacheSaveFailures *measure.Counter
	sharedUsers       *measure.Gauge
}

// New builds and registers every instrument, with each labelled series at 0.
func New() *Metrics {
	m := &Metrics{
		reg:          measure.NewRegistry("plex_language_sync"),
		wsConnected:  measure.NewGauge("websocket_connected", "1 while the Plex WebSocket notification stream is connected, else 0."),
		wsReconnects: measure.NewCounter("websocket_reconnects_total", "Reconnect attempts after the Plex WebSocket stream dropped or failed to connect."),
		scanLastSuccess: measure.NewGauge("deep_scan_last_success_timestamp_seconds",
			"Unix time the last complete deep scan finished, read from the last-run record at startup; 0 when none was ever recorded."),
		scanInterval: measure.NewGauge("deep_scan_interval_seconds", "DEEP_SCAN_INTERVAL in seconds; 0 when the deep scan is off."),
		scanRuns: measure.NewLabeledCounter("deep_scan_runs_total",
			"Deep scans finished, by result: complete covered its whole window, incomplete stopped early.", []string{"result"}),
		resolutionStalled: measure.NewLabeledGauge("user_resolution_stalled",
			"1 while no play event can be attributed to a user, by cause, else 0.", []string{"cause"}),
		playEvents: measure.NewLabeledCounter("play_events_total",
			"Play events handled, by outcome: attributed to a user, or skipped as unattributable.", []string{"outcome"}),
		trackUpdates: measure.NewLabeledCounter("track_updates_total",
			"Stream selection writes to Plex, by kind (audio, subtitle) and result (set, failed).", []string{"kind", "result"}),
		cacheSaveFailures: measure.NewCounter("cache_save_failures_total", "Failed saves of the cache to /config by the deep scan or at shutdown."),
		sharedUsers:       measure.NewGauge("shared_users", "Shared Plex users the app currently follows, besides the admin."),
	}
	m.reg.MustRegister(m.wsConnected, m.wsReconnects, m.scanLastSuccess, m.scanInterval, m.scanRuns,
		m.resolutionStalled, m.playEvents, m.trackUpdates, m.cacheSaveFailures, m.sharedUsers)
	for _, r := range []string{"complete", "incomplete"} {
		m.scanRuns.Add(0, r)
	}
	for _, c := range stallCauses {
		m.resolutionStalled.Set(0, c)
	}
	for _, o := range []string{"attributed", "skipped"} {
		m.playEvents.Add(0, o)
	}
	for _, k := range trackKinds {
		m.trackUpdates.Add(0, string(k), "set")
		m.trackUpdates.Add(0, string(k), "failed")
	}
	return m
}

// Handler serves the metrics page, Go runtime and process families included.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }

// WebSocketConnected records whether the notification stream is connected.
func (m *Metrics) WebSocketConnected(connected bool) {
	if m != nil {
		m.wsConnected.Set(boolValue(connected))
	}
}

// WebSocketReconnect counts one reconnect attempt.
func (m *Metrics) WebSocketReconnect() {
	if m != nil {
		m.wsReconnects.Inc()
	}
}

// DeepScanInterval records the configured interval; pass 0 when the scan is off.
func (m *Metrics) DeepScanInterval(d time.Duration) {
	if m != nil {
		m.scanInterval.Set(d.Seconds())
	}
}

// DeepScanFinished counts one pass and, when it was complete, records at as
// the last complete scan.
func (m *Metrics) DeepScanFinished(complete bool, at time.Time) {
	if m == nil {
		return
	}
	if !complete {
		m.scanRuns.Inc("incomplete")
		return
	}
	m.scanLastSuccess.Set(float64(at.Unix()))
	m.scanRuns.Inc("complete")
}

// DeepScanSeeded records at, a complete scan an earlier process finished, as
// the last complete scan without counting a run.
func (m *Metrics) DeepScanSeeded(at time.Time) {
	if m != nil {
		m.scanLastSuccess.Set(float64(at.Unix()))
	}
}

// ResolutionStalled raises the stall gauge for cause, one of the Stall
// constants.
func (m *Metrics) ResolutionStalled(cause string) {
	if m != nil {
		m.resolutionStalled.Set(1, cause)
	}
}

// ResolutionRecovered lowers the stall gauge for every cause.
func (m *Metrics) ResolutionRecovered() {
	if m == nil {
		return
	}
	for _, c := range stallCauses {
		m.resolutionStalled.Set(0, c)
	}
}

// PlayEvent counts one relevant play event by whether its user was attributed.
func (m *Metrics) PlayEvent(attributed bool) {
	if m == nil {
		return
	}
	if attributed {
		m.playEvents.Inc("attributed")
		return
	}
	m.playEvents.Inc("skipped")
}

// TrackUpdate counts one stream selection write.
func (m *Metrics) TrackUpdate(kind TrackKind, ok bool) {
	if m == nil {
		return
	}
	result := "failed"
	if ok {
		result = "set"
	}
	m.trackUpdates.Inc(string(kind), result)
}

// CacheSaveFailed counts one failed cache save.
func (m *Metrics) CacheSaveFailed() {
	if m != nil {
		m.cacheSaveFailures.Inc()
	}
}

// SharedUsers records how many shared users the app follows.
func (m *Metrics) SharedUsers(n int) {
	if m != nil {
		m.sharedUsers.Set(float64(n))
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
