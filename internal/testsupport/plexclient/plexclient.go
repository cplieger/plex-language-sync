// Package plexclient builds real *plex.Client values pointed at a local
// test server.
//
// It exists so internal/plex does not have to export a constructor that
// only tests call. The production package offers exactly one way to
// build a client — plex.NewClient(plex.Options{...}) — and
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
	"github.com/cplieger/plexapi/v2"
)

// Options carries the two HTTP clients NewFromHTTP can override. Named
// fields rather than two adjacent *http.Client parameters: the pair
// type-checks transposed, and swapping them aims the server client at
// plex.tv and the plex.tv client at the server — a shared-server test
// would then pass or fail for the wrong reason, with nothing in the type
// system objecting. Same reason plex.Options exists on the production side.
type Options struct {
	// HTTP is the client for the Plex server itself. Nil gets the library's
	// default hardened transport (the production default), so a test can
	// also exercise the real transport against a local listener.
	HTTP *http.Client
	// TV overrides the client for plex.tv (shared-server) lookups. Carried
	// per Client rather than through a process global, so tests using it
	// stay parallel-safe.
	TV *http.Client
}

// NewFromHTTP builds a Client from an already-parsed base URL and the HTTP
// clients in opts — the shape a test needs to aim a client at an
// httptest.Server. The zero Options gets the library's hardened defaults for
// both.
//
// It panics rather than returning an error: the URL has already been
// parsed by the caller, so construction can only fail on a non-http(s)
// scheme, which is a fixture bug and should fail the test loudly at the
// point of construction.
func NewFromHTTP(baseURL *url.URL, token plexapi.Token, opts Options) *plex.Client {
	var apiOpts []plexapi.Option
	if opts.HTTP != nil {
		apiOpts = append(apiOpts, plexapi.WithHTTPClient(opts.HTTP))
	}
	api, err := plexapi.New(baseURL.String(), token, apiOpts...)
	if err != nil {
		panic(fmt.Sprintf("plexclient.NewFromHTTP: %v", err))
	}
	return &plex.Client{Client: api, TVClient: opts.TV}
}
