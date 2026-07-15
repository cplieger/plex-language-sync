// Package plex adapts the shared github.com/cplieger/plexapi client for
// plex-language-sync. The transport — header-borne token, refuse-all
// redirects, same-origin path guard, CA pinning, transparent retry with
// Retry-After honoring, bounded reads, and the plaintext-URL startup
// warning — is the library's. This package owns the app's construction
// shapes (CA path from env, per-user clients), its decode types (the
// stream-selection domain model in internal/streams), and the app-facing
// method vocabulary (ShowEpisodes, LoggedUser, SharedUserTokens, ...).
package plex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/plexapi"
)

// ErrNotFound is the library's 404 sentinel, re-exported for call sites
// (errors.Is(err, plex.ErrNotFound)).
var ErrNotFound = plexapi.ErrNotFound

// HTTPStatusError is the library's non-200 error, aliased so the startup
// fatal-vs-transient classifier keeps matching with errors.As.
type HTTPStatusError = plexapi.StatusError

// Client is an HTTP client for a single Plex Media Server base URL + auth
// token. Use NewClient, NewClientForUser, or NewClientFromHTTP.
type Client struct {
	*plexapi.Client
}

// NewClient parses serverURL, validates the scheme, and returns a Client.
// When caCertPath is non-empty, the PEM file at that path is pinned as the
// sole TLS trust anchor (verification stays ON) — the setup for
// self-signed Plex certificates. Empty caCertPath uses the OS trust store.
// The library warns at construction when the URL is plain http to a
// non-local host (the token would transit unencrypted).
func NewClient(serverURL, token, caCertPath string) (*Client, error) {
	opts, err := caOptions(caCertPath)
	if err != nil {
		return nil, err
	}
	api, err := plexapi.New(serverURL, token, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: api}, nil
}

// NewClientForUser creates a Client using a different (user-scoped) token
// but the same server base URL and TLS settings. Plex records
// stream-selection writes against the requesting token's user, so per-user
// writes must go through a per-user client.
func NewClientForUser(baseURL *url.URL, token, caCertPath string) (*Client, error) {
	opts, err := caOptions(caCertPath)
	if err != nil {
		return nil, err
	}
	api, err := plexapi.New(baseURL.String(), token, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: api}, nil
}

// NewClientFromHTTP builds a Client from an already-parsed base URL and a
// caller-supplied http.Client. Intended for tests that point a Client at an
// httptest.Server — production code uses NewClient. A nil hc gets the
// library's default hardened transport.
func NewClientFromHTTP(baseURL *url.URL, token string, hc *http.Client) *Client {
	var opts []plexapi.Option
	if hc != nil {
		opts = append(opts, plexapi.WithHTTPClient(hc))
	}
	api, err := plexapi.New(baseURL.String(), token, opts...)
	if err != nil {
		// The URL was already parsed by the caller; construction can only
		// fail on a non-http(s) scheme, which is a test-fixture bug.
		panic(fmt.Sprintf("plex.NewClientFromHTTP: %v", err))
	}
	return &Client{Client: api}
}

// caOptions loads the CA-pinning option set for caCertPath. The bounded
// PEM read stays here (so the PLEX_CA_CERT_PATH context wraps the error
// and the library stays I/O-free); pinning itself is the library's.
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
