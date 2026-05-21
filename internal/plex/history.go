package plex

import (
	"context"
	"fmt"
)

// History fetches recent play history since the given unix timestamp.
//
// Plex supports filter operators on many fields; the documented syntax for
// "viewedAt >= X" is literally `viewedAt>=X` with a single `>` and a literal
// (unencoded) operator. A prior version used `viewedAt>>=` (double `>`),
// which Plex silently ignores — the server returned the full history
// (21k+ entries on a long-lived server), overflowed the 10 MB read cap in
// doJSON, and surfaced as a daily WARN ("unexpected end of JSON input") in
// Loki. Go's url.Parse preserves `>=` as-is, so the single-char fix
// correctly reaches the server and the response is filtered server-side.
func (c *Client) History(ctx context.Context, sinceUnix int64) ([]HistoryItem, error) {
	path := fmt.Sprintf("/status/sessions/history/all?sort=viewedAt:desc&viewedAt>=%d", sinceUnix)
	return fetchMetadata[HistoryItem](ctx, c, path)
}

// UserFromSession finds the user associated with a clientIdentifier by
// querying active sessions. Returns the user ID and username.
func (c *Client) UserFromSession(ctx context.Context, clientIdentifier string) (userID, username string, err error) {
	sessions, err := fetchMetadata[Session](ctx, c, "/status/sessions")
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
