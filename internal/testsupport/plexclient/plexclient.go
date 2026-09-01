// Package plexclient builds real *plex.Client values pointed at a local
// test server, so internal/plex does not need to export a test-only
// constructor.
package plexclient

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plexapi/v2"
)

// Options carries the two HTTP clients NewFromHTTP can override. Named
// fields prevent a transposition that would silently aim the server
// client at plex.tv and vice versa.
type Options struct {
	// HTTP is the client for the Plex server itself. Nil gets the
	// library's default hardened transport.
	HTTP *http.Client
	// TV overrides the client for plex.tv (shared-server) lookups.
	TV *http.Client
}

// NewFromHTTP builds a Client aimed at an httptest.Server. The zero
// Options gets the library's hardened defaults for both clients.
//
// Panics instead of returning an error: baseURL is already parsed, so
// failure here means a fixture bug, not a runtime condition.
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
