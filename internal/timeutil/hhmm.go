// Package timeutil parses time-shaped strings used across the app
// (currently HH:MM schedule times). Named package — do NOT rename to
// util/helpers/common. As the app accretes more time-domain helpers
// (relative-duration parsing, timezone coercion, etc.) they belong
// here, not in a generic bucket.
package timeutil

import (
	"strconv"
	"strings"
)

// ParseHHMM parses "HH:MM" into (hour, minute). Returns ok=false if the
// input doesn't match or the values are out of range (hour 0-23, minute
// 0-59, input must be exactly "HH:MM" with a single colon).
//
// Single source of truth for HH:MM parsing across the app. Consumers:
// validateScheduleTime (config load) and scheduler.Run (daily tick).
func ParseHHMM(raw string) (hour, minute int, ok bool) {
	hh, mm, found := strings.Cut(raw, ":")
	if !found {
		return 0, 0, false
	}
	h, hErr := strconv.Atoi(hh)
	m, mErr := strconv.Atoi(mm)
	if hErr != nil || mErr != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
