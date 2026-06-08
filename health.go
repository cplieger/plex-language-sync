package main

import "github.com/cplieger/health"

// healthMarkerPath is the marker location, sourced from the library so
// the literal lives in exactly one place.
const healthMarkerPath = health.DefaultPath

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
