// Package plex adapts the shared github.com/cplieger/plexapi/v2 client
// for plex-language-sync. Transport hardening (redirects, CA pinning,
// retry, bounded reads) is the library's; this package owns construction
// shapes, decode types (internal/streams), and the app-facing method
// vocabulary.
package plex

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/plexapi/v2"
)

// ErrNotFound is the library's 404 sentinel, re-exported so callers can
// use errors.Is(err, plex.ErrNotFound).
var ErrNotFound = plexapi.ErrNotFound

// HTTPStatusError is the library's non-200 error, aliased so the startup
// fatal-vs-transient classifier matches it with errors.As.
type HTTPStatusError = plexapi.StatusError

// Client is an HTTP client for a single Plex Media Server base URL + auth
// token. Build one with NewClient; derive per-user clients with ForToken.
type Client struct {
	*plexapi.Client
	// TVClient overrides the HTTP client for plex.tv (shared-server)
	// lookups; nil leaves the library's hardened default. Carried on the
	// value rather than a package global, which forced serial tests for
	// a seam production never used. ForToken propagates it.
	TVClient *http.Client
}

// Token is plexapi's Plex-token type, re-exported to avoid a second
// import for one conversion. An alias, not a new type: it IS the
// library's type.
type Token = plexapi.Token

// Options configures NewClient. Field order is govet fieldalignment's,
// not editorial.
type Options struct {
	// TVClient overrides the HTTP client for plex.tv (shared-server)
	// lookups. Nil leaves the library's hardened default; set only by
	// tests pointing those lookups at a local httptest server.
	TVClient *http.Client
	// ServerURL is the Plex server base URL. Required.
	ServerURL string
	// Token authenticates every request.
	Token Token
	// CACertPath, when non-empty, pins the PEM file at that path as the
	// sole TLS trust anchor (verification stays ON). Empty uses the OS
	// trust store.
	CACertPath string
}

// NewClient validates opts.ServerURL and returns a Client.
func NewClient(opts Options) (*Client, error) {
	apiOpts, err := caOptions(opts.CACertPath)
	if err != nil {
		return nil, err
	}
	api, err := plexapi.New(opts.ServerURL, opts.Token, apiOpts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: api, TVClient: opts.TVClient}, nil
}

// ForToken derives a same-server Client for a different (user-scoped)
// token, sharing the transport and connection pool. Plex records
// stream-selection writes against the requesting token's user, so
// per-user writes must go through a per-user client.
func (c *Client) ForToken(token plexapi.Token) *Client {
	return &Client{Client: c.Client.ForToken(token), TVClient: c.TVClient}
}

// caOptions loads the CA-pinning option set for caCertPath. The bounded
// read stays here so the PLEX_CA_CERT_PATH context wraps the error and
// the library stays I/O-free.
func caOptions(caCertPath string) ([]plexapi.Option, error) {
	if caCertPath == "" {
		return nil, nil
	}
	const maxCACertSize = 1 << 20 // 1 MB
	pemBytes, err := atomicfile.ReadBounded(context.Background(), caCertPath, maxCACertSize)
	if err != nil {
		return nil, fmt.Errorf("reading PLEX_CA_CERT_PATH=%q: %w", caCertPath, err)
	}
	return []plexapi.Option{plexapi.WithCACertPEM(pemBytes)}, nil
}
