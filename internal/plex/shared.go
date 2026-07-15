package plex

import (
	"context"
	"net/http"
	"time"

	"github.com/cplieger/httpx/v2"
	"github.com/cplieger/plexapi"
)

// tvClient is the shared HTTP client for plex.tv API calls. Always uses
// full TLS verification (there is no verification-skip option) and refuses
// redirects, so the admin token can never be forwarded off plex.tv by a
// compromised redirect or CDN front. Swappable for tests via SwapTVClient.
var tvClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: httpx.RefuseAllRedirects,
}

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
	tv := plexapi.NewTV(c.Token(), plexapi.WithTVHTTPClient(tvClient))
	return tv.SharedServers(ctx, machineIdentifier)
}
