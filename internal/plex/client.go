package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/httpx/v2"
)

// maxResponseBody caps the number of bytes read from any single Plex JSON
// response. Matches the main package's original limit; a prior bug around
// unfiltered /history responses overflowed this cap (see History for the
// viewedAt>= fix that keeps us inside it).
const maxResponseBody = 10 << 20 // 10 MB

// ErrNotFound is returned when Plex responds with 404 (missing metadata /
// session / etc.). Callers detect it with errors.Is(err, plex.ErrNotFound).
var ErrNotFound = errors.New("not found")

// errBodyOverCap signals a response exceeded maxResponseBody. Callers map
// it to an endpoint-specific error so the message stays specific while the
// read-cap WARN + limit live in one place.
var errBodyOverCap = errors.New("response exceeded read cap")

// readCappedBody reads up to maxResponseBody bytes from body, logging the
// shared over-cap WARN (identified by warnAttrs) and returning
// errBodyOverCap when the response exceeds the cap. Single-sources the
// WARN string (a Loki-alerting contract) and the cap across both read
// paths (local Plex JSON, plex.tv XML).
func readCappedBody(body io.Reader, warnAttrs ...any) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(body, maxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > maxResponseBody {
		slog.Warn("plex API response exceeded read cap; body truncated, likely an unfiltered or oversized response",
			append(warnAttrs, "cap_bytes", maxResponseBody)...)
		return nil, errBodyOverCap
	}
	return b, nil
}

// Client is an HTTP client for a single Plex Media Server base URL + auth
// token. Use NewClient or NewClientForUser.
type Client struct {
	httpClient *http.Client
	baseURL    *url.URL
	token      string
}

// NewClient parses serverURL, validates scheme, and returns a Client
// configured with the given token and TLS behaviour. When caCertPath is
// non-empty, the PEM file at that path is loaded into the TLS RootCAs pool
// so verification stays ON, pinned to that CA — recommended for users with
// self-signed Plex certificates. Empty caCertPath uses the OS trust store
// (works for plex.direct + any publicly-issued cert; works as a no-op for
// plain http:// URLs). Returns an error on invalid URL, unsupported scheme,
// missing cert file, or unparseable PEM; the caller (main.go) is
// responsible for logging and exiting.
func NewClient(serverURL, token, caCertPath string) (*Client, error) {
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PLEX_URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("PLEX_URL must use http or https scheme, got %q", parsed.Scheme)
	}
	hc, err := newHTTPClient(caCertPath)
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: parsed, token: token, httpClient: hc}, nil
}

// NewClientForUser creates a Client using a different token but the same
// server base URL and TLS settings as an existing client. Same caCertPath
// semantics as NewClient.
func NewClientForUser(baseURL *url.URL, token, caCertPath string) (*Client, error) {
	hc, err := newHTTPClient(caCertPath)
	if err != nil {
		return nil, err
	}
	return &Client{baseURL: baseURL, token: token, httpClient: hc}, nil
}

// NewClientFromHTTP builds a Client from an already-parsed base URL and a
// caller-supplied http.Client. Intended for tests that want to point a
// Client at an httptest.Server — production code should use NewClient.
func NewClientFromHTTP(baseURL *url.URL, token string, hc *http.Client) *Client {
	return &Client{baseURL: baseURL, token: token, httpClient: hc}
}

// BaseURL returns the server's base URL. Used by callers that need to derive
// a websocket URL or log the host.
func (c *Client) BaseURL() *url.URL { return c.baseURL }

// Token returns the token the client uses. Exposed for the user-manager
// eviction path, which compares a cached client's token against a freshly
// refreshed user-info entry.
func (c *Client) Token() string { return c.token }

// HTTPClient returns the underlying *http.Client. Exposed so the WebSocket
// listener can dial the notifications endpoint with the same transport
// (matching CA-trust, timeouts, and redirect policy) rather than
// spinning up a second client.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// WarnIfPlaintextURL emits a startup warning when the Plex URL is http:// to
// a non-loopback, non-docker-DNS host. In that case the X-Plex-Token header
// transits the network unencrypted. Trusted on a LAN-only proxy bridge, but
// dangerous when the published image is pointed at a remote Plex server
// without a TLS proxy.
func WarnIfPlaintextURL(u *url.URL) {
	if u == nil || u.Scheme != "http" {
		return
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return
	}
	if ip := net.ParseIP(host); ip != nil {
		// Bare IP literal (v4 or v6). Loopback is safe; anything
		// else (including remote IPv6, which the dot heuristic
		// below would silently treat as a trusted docker name)
		// transits the token in cleartext.
		if ip.IsLoopback() {
			return
		}
	} else if !strings.Contains(host, ".") {
		// Non-IP hostname with no dot: a docker short-name on the
		// trusted proxy bridge. Stay quiet.
		return
	}
	slog.Warn("PLEX_URL is http:// to a non-local host; X-Plex-Token will "+
		"transit unencrypted. Front Plex with a TLS proxy and set "+
		"PLEX_URL=https://... for off-LAN deployments.",
		"host", host)
}

// newHTTPClient returns the HTTP client used for local Plex Media Server
// calls. Refuses to follow redirects to prevent X-Plex-Token exfiltration
// via a hostile 3xx (MITM, DNS poisoning, compromised upstream). Matches
// PLEX-SEC-01.
//
// When caCertPath is non-empty, the PEM file at that path is loaded into a
// custom TLS RootCAs pool — verification stays ON, pinned to that CA. This
// supports self-signed Plex certs without disabling cert checking.
// Empty caCertPath means: use the OS trust store (default Transport, nil).
// For http:// URLs the TLS config is unused either way.
func newHTTPClient(caCertPath string) (*http.Client, error) {
	c := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if caCertPath != "" {
		const maxCACertSize = 1 << 20 // 1 MB
		// The bounded PEM read stays here (so the PLEX_CA_CERT_PATH context
		// wraps the error and httpx stays I/O-free); httpx.CATransport does the
		// pinning: it clones http.DefaultTransport and installs a fresh TLS
		// config trusting ONLY the CA(s) in the PEM (RootCAs pinned, TLS 1.2
		// minimum, verification always on).
		pemBytes, err := atomicfile.ReadBounded(context.Background(), caCertPath, maxCACertSize)
		if err != nil {
			return nil, fmt.Errorf("reading PLEX_CA_CERT_PATH=%q: %w", caCertPath, err)
		}
		transport, err := httpx.CATransport(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("PLEX_CA_CERT_PATH=%q: %w", caCertPath, err)
		}
		c.Transport = transport
	}
	return c, nil
}

// tvClient is a shared HTTP client for plex.tv API calls. Always uses
// full TLS verification (there is no verification-skip option). Refuses to
// follow redirects to prevent cross-origin X-Plex-Token exfiltration via a
// compromised plex.tv redirect or CDN front. The admin token is sent to
// plex.tv for shared_servers lookup and must never be forwarded elsewhere.
var tvClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// SwapTVClient replaces the package-level plex.tv HTTP client with the
// supplied one and returns a function that restores the original. Intended
// for tests that need to point shared-server lookups at a local httptest
// server; production code never calls this.
func SwapTVClient(replacement *http.Client) (restore func()) {
	orig := tvClient
	tvClient = replacement
	return func() { tvClient = orig }
}

func (c *Client) doJSON(ctx context.Context, method, path string, result any) error {
	ref, err := url.Parse(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		c.baseURL.ResolveReference(ref).String(), http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		drainBody(resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		// Drain body to allow connection reuse.
		drainBody(resp.Body)
		return fmt.Errorf("plex API %s %s: %s", method, path, resp.Status)
	}
	if result == nil {
		drainBody(resp.Body)
		return nil
	}
	body, err := readCappedBody(resp.Body, "method", method, "path", path)
	if err != nil {
		if errors.Is(err, errBodyOverCap) {
			return fmt.Errorf("plex API %s %s: response exceeded %d-byte read cap", method, path, maxResponseBody)
		}
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, result)
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	return c.doJSON(ctx, http.MethodGet, path, result)
}

func (c *Client) put(ctx context.Context, path string) error {
	return c.doJSON(ctx, http.MethodPut, path, nil)
}

// drainBody reads and discards up to 4 KB of the response body to enable
// HTTP connection reuse.
func drainBody(body io.ReadCloser) {
	if _, err := io.CopyN(io.Discard, body, 4<<10); err != nil && !errors.Is(err, io.EOF) {
		slog.Debug("failed to drain response body", "error", err)
	}
}
