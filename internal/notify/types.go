// Package notify implements the Plex WebSocket notification listener.
//
// The Listener dials Plex's /:/websockets/notifications endpoint, decodes
// the NotificationContainer JSON envelope into typed events, and delivers
// them to a caller-supplied Handler. Reconnect/backoff is test-injectable
// via Config (no global vars). Disconnect reasons are classified with
// typed sentinel errors so Loki alert rules can segment by cause without
// substring matching on error text.
//
// Inviolate contracts preserved by this package:
//   - Plex WebSocket JSON wire format (struct tags on Notification /
//     PlayEvent / TimelineEntry) — contract item 9.
//   - WARN/ERROR slog keys and the ReasonXxx string values — contract
//     item 5 (Loki alerting).
//   - /:/websockets/notifications URL path, 1 MB read limit, 5-minute
//     read deadline, X-Plex-Token header — contract item 9 (Plex API).
package notify

// Notification is the top-level envelope Plex sends over the WebSocket.
// Field names and JSON tags mirror the Plex NotificationContainer wire
// format byte-for-byte (inviolate contract item 9).
type Notification struct {
	NotificationContainer struct {
		Type                         string          `json:"type"`
		PlaySessionStateNotification []PlayEvent     `json:"PlaySessionStateNotification"`
		TimelineEntry                []TimelineEntry `json:"TimelineEntry"`
	} `json:"NotificationContainer"`
}

// PlayEvent represents a single play-session state notification from Plex.
type PlayEvent struct {
	SessionKey       string `json:"sessionKey"`
	ClientIdentifier string `json:"clientIdentifier"`
	RatingKey        string `json:"ratingKey"`
	State            string `json:"state"`
	ViewOffset       int64  `json:"viewOffset"`
}

// TimelineEntry represents a library scan timeline event from Plex.
type TimelineEntry struct {
	ItemID        string `json:"itemID"`
	Identifier    string `json:"identifier"`
	SectionID     string `json:"sectionID"`
	MetadataState string `json:"metadataState"`
	MediaState    string `json:"mediaState"`
	Type          int    `json:"type"`
	State         int    `json:"state"`
	UpdatedAt     int64  `json:"updatedAt"`
}
