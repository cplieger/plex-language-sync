// Package notify implements the Plex WebSocket notification listener.
//
// Stable wire contracts: the Plex NotificationContainer JSON format
// (struct tags below), the WARN/ERROR slog keys and ReasonXxx values
// Loki alert rules match on, and the /:/websockets/notifications path,
// 1 MB read limit, and X-Plex-Token header.
package notify

// Notification is the top-level envelope Plex sends over the WebSocket.
// Field names and tags mirror the wire format byte-for-byte.
type Notification struct {
	NotificationContainer struct {
		Type                         string          `json:"type"`
		PlaySessionStateNotification []PlayEvent     `json:"PlaySessionStateNotification"`
		TimelineEntry                []TimelineEntry `json:"TimelineEntry"`
	} `json:"NotificationContainer"`
}

// PlayEvent represents a single play-session state notification from
// Plex. Only the fields the event plane consumes are declared; the
// decoder is non-strict.
type PlayEvent struct {
	ClientIdentifier string `json:"clientIdentifier"`
	RatingKey        string `json:"ratingKey"`
	State            string `json:"state"`
}

// TimelineEntry represents a library scan timeline event from Plex.
// Only the fields the scan predicates consume are declared.
type TimelineEntry struct {
	ItemID        string `json:"itemID"`
	MetadataState string `json:"metadataState"`
	MediaState    string `json:"mediaState"`
	Type          int    `json:"type"`
}
