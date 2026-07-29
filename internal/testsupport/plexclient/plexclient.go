// Package plexclient builds real *plex.Client values pointed at a local
// test server.
//
// It exists so internal/plex does not have to export a constructor that
// only tests call. The production package offers exactly one way to
// build a client — plex.NewClient(serverURL, token, caCertPath) — and
// every production call site uses it; pointing a client at an
// httptest.Server is a test concern, so the constructor for that shape
// lives here, next to the other fixtures, rather than on the production
// surface.
//
// Construction goes through the exported plex.Client shape (an embedded
// *plexapi.Client), so this package needs no privileged access to
// internal/plex.
package plexclient

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plexapi"
)

// NewFromHTTP builds a Client from an already-parsed base URL and a
// caller-supplied http.Client — the shape a test needs to aim a client at
// an httptest.Server. A nil hc gets the library's default hardened
// transport (the production default), so a test can also exercise the
// real transport against a local listener.
//
// It panics rather than returning an error: the URL has already been
// parsed by the caller, so construction can only fail on a non-http(s)
// scheme, which is a fixture bug and should fail the test loudly at the
// point of construction.
func NewFromHTTP(baseURL *url.URL, token string, hc *http.Client) *plex.Client {
	var opts []plexapi.Option
	if hc != nil {
		opts = append(opts, plexapi.WithHTTPClient(hc))
	}
	api, err := plexapi.New(baseURL.String(), token, opts...)
	if err != nil {
		panic(fmt.Sprintf("plexclient.NewFromHTTP: %v", err))
	}
	return &plex.Client{Client: api}
}
