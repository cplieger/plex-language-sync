// Package notify implements the Plex WebSocket notification listener.
//
// The Listener dials Plex's /:/websockets/notifications endpoint, decodes
// the NotificationContainer JSON envelope into typed events, and delivers
// them to a caller-supplied Handler. Reconnect/backoff is test-injectable
// via Config (no global vars). Disconnect reasons are classified with
// typed sentinel errors so Loki alert rules can segment by cause without
// substring matching on error text.
//
// Stable contracts preserved by this package:
//   - Plex WebSocket JSON wire format (struct tags on Notification /
//     PlayEvent / TimelineEntry).
//   - WARN/ERROR slog keys and the ReasonXxx string values, which Loki
//     alert rules match on.
//   - /:/websockets/notifications URL path, 1 MB read limit, the
//     X-Plex-Token header. The read-idle backstop is Config-driven
//     (ReadIdleTimeout, default 1 hour), not a fixed 5-minute deadline.
package notify

// Notification is the top-level envelope Plex sends over the WebSocket.
// Field names and JSON tags mirror the Plex NotificationContainer wire
// format byte-for-byte.
type Notification struct {
	NotificationContainer struct {
		Type                         string          `json:"type"`
		PlaySessionStateNotification []PlayEvent     `json:"PlaySessionStateNotification"`
		TimelineEntry                []TimelineEntry `json:"TimelineEntry"`
	} `json:"NotificationContainer"`
}

// PlayEvent represents a single play-session state notification from Plex.
// Only the fields the event plane consumes are declared: the decoder is
// non-strict, so Plex's other PlaySessionStateNotification fields
// (sessionKey, viewOffset, …) are ignored on the wire rather than
// decoded into unread struct members.
type PlayEvent struct {
	ClientIdentifier string `json:"clientIdentifier"`
	RatingKey        string `json:"ratingKey"`
	State            string `json:"state"`
}

// TimelineEntry represents a library scan timeline event from Plex.
// Only the fields the scan predicates consume are declared; see PlayEvent
// for the non-strict-decode rationale.
type TimelineEntry struct {
	ItemID        string `json:"itemID"`
	MetadataState string `json:"metadataState"`
	MediaState    string `json:"mediaState"`
	Type          int    `json:"type"`
}
