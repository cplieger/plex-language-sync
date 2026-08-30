package plex

import (
	"context"
	"errors"
	"fmt"

	"github.com/cplieger/plexapi/v2"
)

// ErrNoSessionForClient reports that the active-session list was read
// and carried no session for the requested client.
//
// Distinct from a failure to read that list at all, and the caller must
// keep them apart: this one is the EXPECTED outcome of two measured Plex
// behaviours (a play notification arriving before the session is
// queryable, and a client that keeps announcing an item whose session
// Plex has already removed), so it says nothing about whether resolution
// works. A read failure does.
var ErrNoSessionForClient = errors.New("no session for client")

// History fetches recent play history since the given unix timestamp,
// filtered server-side. The path — including Plex's literal single-char
// `viewedAt>=` operator, whose doubled form Plex silently ignores (the
// 14-day unfiltered-history outage this app once shipped) — is owned by
// the library's HistoryPath builder; this app only owns the HistoryItem
// decode shape.
func (c *Client) History(ctx context.Context, sinceUnix int64) ([]HistoryItem, error) {
	return c.fetchMetadata[HistoryItem](ctx, plexapi.HistoryPath(sinceUnix))
}

// UserFromSession finds the user associated with a clientIdentifier by
// querying active sessions. Returns the user ID and username.
//
// The two failure modes are deliberately distinguishable: a session list
// that could not be read is returned wrapped, and a list that was read
// without the client in it matches ErrNoSessionForClient.
func (c *Client) UserFromSession(ctx context.Context, clientIdentifier string) (userID, username string, err error) {
	sessions, err := c.fetchMetadata[Session](ctx, plexapi.SessionsPath())
	if err != nil {
		return "", "", fmt.Errorf("fetching sessions: %w", err)
	}
	for _, s := range sessions {
		if s.Player.MachineIdentifier == clientIdentifier {
			return s.User.ID, s.User.Title, nil
		}
	}
	return "", "", fmt.Errorf("%w: %q", ErrNoSessionForClient, clientIdentifier)
}
