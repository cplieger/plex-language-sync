package plex

import (
	"context"
	"net/http"

	"github.com/cplieger/plexapi"
)

// tvClient overrides the plex.tv HTTP client when non-nil — a test seam
// (SwapTVClient) so shared-server lookups can be pointed at a local
// httptest server. Production leaves it nil and uses the library's own
// hardened default (30s timeout, refuse-all redirects, OS trust store,
// no verification-skip option).
var tvClient *http.Client

// SwapTVClient replaces the package-level plex.tv HTTP client with the
// supplied one and returns a function that restores the original. Intended
// for tests that point shared-server lookups at a local httptest server;
// production code never calls this.
func SwapTVClient(replacement *http.Client) (restore func()) {
	orig := tvClient
	tvClient = replacement
	return func() { tvClient = orig }
}

// SharedUserTokens fetches shared user tokens from the plex.tv
// shared_servers endpoint. This calls the plex.tv API (not the local
// server) through the library's TV client, which never skips TLS
// verification and never follows redirects — the admin token must not be
// forwarded anywhere but plex.tv.
func (c *Client) SharedUserTokens(ctx context.Context, machineIdentifier string) ([]SharedServerXML, error) {
	var opts []plexapi.TVOption
	if tvClient != nil {
		opts = append(opts, plexapi.WithTVHTTPClient(tvClient))
	}
	tv := plexapi.NewTV(c.Token(), opts...)
	return tv.SharedServers(ctx, machineIdentifier)
}
