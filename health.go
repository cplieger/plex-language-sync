package main

import "github.com/cplieger/health"

// healthMarkerPath is the default marker location. Docker healthchecks
// stat this path; the app creates and removes it at lifecycle points.
// /tmp is conventional because strict-tier compose services mount
// /tmp as tmpfs (see base.yaml -strict templates).
const healthMarkerPath = "/tmp/.healthy"

// healthMarker wraps *health.Marker for backward-compatible usage in main.
type healthMarker = health.Marker

// newHealthMarker constructs a marker for path and probes the parent
// directory for writability. On failure it logs a single Warn with a
// fix hint and returns a marker in degraded mode.
func newHealthMarker(path string) *healthMarker {
	return health.NewMarker(path)
}

// runProbe runs in the separate `health` subcommand process.
func runProbe(path string) {
	health.RunProbe(path)
}

// probeCheck implements the health-probe decision without calling
// os.Exit, so it can be unit-tested.
func probeCheck(path string) int {
	return health.ProbeCheck(path)
}
