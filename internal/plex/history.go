package plex

import (
	"context"
	"fmt"

	"github.com/cplieger/plexapi"
)

// History fetches recent play history since the given unix timestamp,
// filtered server-side. The path — including Plex's literal single-char
// `viewedAt>=` operator, whose doubled form Plex silently ignores (the
// 14-day unfiltered-history outage this app once shipped) — is owned by
// the library's HistoryPath builder; this app only owns the HistoryItem
// decode shape.
func (c *Client) History(ctx context.Context, sinceUnix int64) ([]HistoryItem, error) {
	return fetchMetadata[HistoryItem](ctx, c, plexapi.HistoryPath(sinceUnix))
}

// UserFromSession finds the user associated with a clientIdentifier by
// querying active sessions. Returns the user ID and username.
func (c *Client) UserFromSession(ctx context.Context, clientIdentifier string) (userID, username string, err error) {
	sessions, err := fetchMetadata[Session](ctx, c, plexapi.SessionsPath())
	if err != nil {
		return "", "", fmt.Errorf("fetching sessions: %w", err)
	}
	for _, s := range sessions {
		if s.Player.MachineIdentifier == clientIdentifier {
			return s.User.ID, s.User.Title, nil
		}
	}
	return "", "", fmt.Errorf("no session found for client %q", clientIdentifier)
}
