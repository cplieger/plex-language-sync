package plex

import (
	"context"

	"github.com/cplieger/plexapi/v2"
)

// SharedUserTokens fetches shared user tokens from the plex.tv
// shared_servers endpoint. This calls the plex.tv API (not the local
// server) through the library's TV client, which never skips TLS
// verification and never follows redirects — the admin token must not be
// forwarded anywhere but plex.tv.
func (c *Client) SharedUserTokens(ctx context.Context, machineIdentifier string) ([]SharedServerXML, error) {
	var opts []plexapi.TVOption
	if c.TVClient != nil {
		opts = append(opts, plexapi.WithTVHTTPClient(c.TVClient))
	}
	tv := plexapi.NewTV(c.Token(), opts...)
	return tv.SharedServers(ctx, machineIdentifier)
}
