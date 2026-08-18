// Package plex adapts the shared github.com/cplieger/plexapi/v2 client for
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

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/plexapi/v2"
)

// ErrNotFound is the library's 404 sentinel, re-exported for call sites
// (errors.Is(err, plex.ErrNotFound)).
var ErrNotFound = plexapi.ErrNotFound

// HTTPStatusError is the library's non-200 error, aliased so the startup
// fatal-vs-transient classifier keeps matching with errors.As.
type HTTPStatusError = plexapi.StatusError

// Client is an HTTP client for a single Plex Media Server base URL + auth
// token. Build one with NewClient; derive per-user clients with ForToken.
type Client struct {
	*plexapi.Client
	// TVClient overrides the HTTP client for plex.tv (shared-server) lookups;
	// nil leaves the library's hardened default. Carried on the value rather
	// than in a package global: the global was swapped by an EXPORTED helper,
	// which forced every suite that touched it to run serially for a seam
	// production never uses. Exported because the test-support package builds
	// Client values from outside this package. ForToken propagates it, so a
	// per-user client keeps the same override.
	TVClient *http.Client
}

// Options configures NewClient. The old signature was three adjacent
// strings, and a transposition put the token where the CA path belongs — the
// library then reports the value it could not read, so the token would have
// reached the startup log.
// Field order is govet fieldalignment's, not editorial.
type Options struct {
	// TVClient overrides the HTTP client used for plex.tv (shared-server)
	// lookups. Nil leaves the library's hardened default (30s timeout,
	// refuse-all redirects, OS trust store, no verification-skip). Set only
	// by tests that point those lookups at a local httptest server; it
	// replaces an exported process-global swapper, which cost every suite
	// that touched it its parallelism.
	TVClient *http.Client
	// ServerURL is the Plex server base URL. Required; plexapi.New validates
	// the scheme and warns when it is plain http to a non-local host (the
	// token would transit unencrypted).
	ServerURL string
	// Token authenticates every request.
	Token string
	// CACertPath, when non-empty, pins the PEM file at that path as the sole
	// TLS trust anchor (verification stays ON) — the setup for a self-signed
	// Plex. Empty uses the OS trust store.
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
// token, sharing the underlying transport, trust settings, and connection
// pool — the library's per-user write path. Plex records stream-selection
// writes against the requesting token's user, so per-user writes must go
// through a per-user client. Derivation is pure (no I/O, cannot fail):
// the CA pin and transport were established once at NewClient time.
func (c *Client) ForToken(token string) *Client {
	return &Client{Client: c.Client.ForToken(token), TVClient: c.TVClient}
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
