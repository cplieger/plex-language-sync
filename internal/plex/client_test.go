package plex

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/httpx/v2/certtest"
)

// newTestClient builds a Client pointed at an httptest server running the
// given handler. The server is torn down when the test ends. Shared across
// all client tests in this package.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		baseURL:    u,
		token:      "test-token",
		httpClient: srv.Client(),
	}
}

// errReader is a reader that always returns an error. Used for drainBody
// error-path tests.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// captureSlog redirects the default slog logger to a buffer for the duration
// of fn and returns everything logged. It restores the previous default
// logger on cleanup. Tests using it must NOT be parallel (they mutate the
// process-global default logger).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// --- Tests: NewClient ---

func TestNewClient_HappyPath(t *testing.T) {
	t.Parallel()
	c, err := NewClient("http://plex:32400", "tok", "")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if c.baseURL.Host != "plex:32400" {
		t.Errorf("baseURL.Host = %q, want plex:32400", c.baseURL.Host)
	}
}

func TestNewClient_InvalidURL(t *testing.T) {
	t.Parallel()
	_, err := NewClient("://bad", "tok", "")
	if err == nil {
		t.Fatal("NewClient() with invalid URL should return error")
	}
}

func TestNewClient_BadScheme(t *testing.T) {
	t.Parallel()
	_, err := NewClient("ftp://plex:32400", "tok", "")
	if err == nil {
		t.Fatal("NewClient() with ftp scheme should return error")
	}
}

// --- Tests: NewClientForUser ---

func TestNewClientForUser(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	c, err := NewClientForUser(parsed, "test-token", "")
	if err != nil {
		t.Fatalf("NewClientForUser: %v", err)
	}
	if c.token != "test-token" {
		t.Errorf("token = %q, want test-token", c.token)
	}
	if c.baseURL != parsed {
		t.Error("baseURL should match")
	}
}

// --- Tests: newHTTPClient ---

func TestNewClientForUserCACert(t *testing.T) {
	parsed, _ := url.Parse("https://plex:32400")
	caPath := certtest.WriteSelfSignedCA(t)
	c, err := NewClientForUser(parsed, "test-token", caPath)
	if err != nil {
		t.Fatalf("NewClientForUser: %v", err)
	}
	if c.httpClient.Transport == nil {
		t.Fatal("expected custom transport when caCertPath set")
	}
}

// --- Tests: newHTTPClient ---

func TestNewHTTPClientNoCA(t *testing.T) {
	c, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	if c.Transport != nil {
		t.Error("expected nil transport when caCertPath is empty (use OS trust store)")
	}
}

func TestNewHTTPClientWithCA(t *testing.T) {
	caPath := certtest.WriteSelfSignedCA(t)
	c, err := newHTTPClient(caPath)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	if c.Transport == nil {
		t.Fatal("expected non-nil transport when caCertPath is set")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected TLSClientConfig.RootCAs to be populated")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must remain false; CA-pool path is the SECURE path")
	}
}

func TestNewHTTPClient_missing_file_errors(t *testing.T) {
	_, err := newHTTPClient("/nonexistent/ca.pem")
	if err == nil {
		t.Fatal("newHTTPClient should error when caCertPath points to a missing file")
	}
}

func TestNewHTTPClient_invalid_pem_errors(t *testing.T) {
	bogus := filepath.Join(t.TempDir(), "bogus.pem")
	if err := os.WriteFile(bogus, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := newHTTPClient(bogus)
	if err == nil {
		t.Fatal("newHTTPClient should error when caCertPath has no PEM-encoded certs")
	}
}

// TestNewHTTPClient_DefaultTimeout pins the 30s request timeout: a zeroed
// timeout would silently drop the bound on every Plex call, a reliability
// regression that no other test would catch.
func TestNewHTTPClient_DefaultTimeout(t *testing.T) {
	t.Parallel()
	c, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	if c.Timeout != 30*time.Second {
		t.Errorf("newHTTPClient timeout = %v, want 30s", c.Timeout)
	}
}

// --- Tests: newHTTPClient refuses redirects (PLEX-LS-SEC-01) ---

func TestNewHTTPClient_RefusesRedirects(t *testing.T) {
	t.Parallel()
	// Chain: src redirects to dst; an attacker-controlled dst would receive
	// the X-Plex-Token header if the client followed the 302.
	dstHits := 0
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dstHits++
		if r.Header.Get("X-Plex-Token") != "" {
			t.Error("X-Plex-Token forwarded to redirect target — token exfiltration risk")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dst.URL, http.StatusFound)
	}))
	defer src.Close()

	c, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, src.URL, http.NoBody)
	req.Header.Set("X-Plex-Token", "secret")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 (redirect not followed)", resp.StatusCode)
	}
	if dstHits != 0 {
		t.Errorf("destination received %d hits; redirect was followed", dstHits)
	}
}

// --- Tests: WarnIfPlaintextURL (PLEX-LS-SEC-02) ---

// TestWarnIfPlaintextURL asserts the plaintext-token warning fires for
// http:// to a remote (dotted) host and stays silent for every safe shape:
// loopback, localhost, Docker-DNS short names (no dots), and any https URL.
// captureSlog inspects the emitted log instead of only checking the call
// does not panic, so each branch's observable behaviour is pinned.
func TestWarnIfPlaintextURL(t *testing.T) {
	const warning = "transit unencrypted"
	tests := []struct {
		name     string
		rawURL   string
		wantWarn bool
	}{
		{"http to remote FQDN warns", "http://plex.example.com:32400", true},
		{"http to remote IP warns", "http://203.0.113.100:32400", true},
		{"http to remote IPv6 literal warns", "http://[2001:db8::1]:32400", true},
		{"http to multi-label host warns", "http://my.plex.server:32400", true},
		{"http to loopback IP is quiet", "http://127.0.0.1:32400", false},
		{"http to IPv6 loopback is quiet", "http://[::1]:32400", false},
		{"http to localhost is quiet", "http://localhost:32400", false},
		{"http to Docker DNS short name is quiet", "http://plex:32400", false},
		{"https to FQDN is quiet", "https://plex.example.com", false},
		{"https to IP is quiet", "https://203.0.113.5", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tt.rawURL, err)
			}
			out := captureSlog(t, func() { WarnIfPlaintextURL(u) })
			if gotWarn := strings.Contains(out, warning); gotWarn != tt.wantWarn {
				t.Errorf("WarnIfPlaintextURL(%q) warned=%v, want %v (log: %q)",
					tt.rawURL, gotWarn, tt.wantWarn, out)
			}
		})
	}

	// A nil URL must be a no-op: no panic, no warning.
	if out := captureSlog(t, func() { WarnIfPlaintextURL(nil) }); strings.Contains(out, warning) {
		t.Errorf("WarnIfPlaintextURL(nil) warned, want silence (log: %q)", out)
	}
}

// --- Tests: drainBody ---

// TestDrainBody pins drainBody's logging contract: successful drains (small,
// empty, and over-4KB bodies, where io.CopyN stops at the 4KB cap or hits a
// suppressed io.EOF) emit nothing, while a genuine non-EOF read error is
// surfaced at debug. captureSlog asserts on the emitted log instead of only
// checking the call does not panic.
func TestDrainBody(t *testing.T) {
	const debugLine = "failed to drain response body"
	tests := []struct {
		name    string
		body    io.ReadCloser
		wantLog bool
	}{
		{"small body drains without logging", io.NopCloser(strings.NewReader("hello world")), false},
		{"empty body drains without logging", io.NopCloser(strings.NewReader("")), false},
		{"over-4KB body drains without logging", io.NopCloser(strings.NewReader(strings.Repeat("x", 8192))), false},
		{"non-EOF read error is logged at debug", io.NopCloser(&errReader{err: errors.New("connection reset")}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := captureSlog(t, func() { drainBody(tt.body) })
			if gotLog := strings.Contains(out, debugLine); gotLog != tt.wantLog {
				t.Errorf("drainBody logged=%v, want %v (log: %q)", gotLog, tt.wantLog, out)
			}
		})
	}
}

// --- Tests: doJSON / get / put ---

func TestDoJSON_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "test-token" {
			t.Errorf("X-Plex-Token = %q, want test-token", r.Header.Get("X-Plex-Token"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q, want application/json", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.get(context.Background(), "/some/path", &out); err != nil {
		t.Fatalf("get() error = %v", err)
	}
	if !out.OK {
		t.Error("get() did not decode body")
	}
}

func TestDoJSON_Returns404AsErrNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	})
	var out struct{}
	err := c.get(context.Background(), "/missing", &out)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("get() error = %v, want ErrNotFound", err)
	}
}

func TestDoJSON_NonOKStatusReturnsError(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	var out struct{}
	err := c.get(context.Background(), "/boom", &out)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("get() error = %v, want non-nil non-ErrNotFound", err)
	}
}

func TestDoJSON_EmptyBodyOK(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	var out map[string]string
	if err := c.get(context.Background(), "/", &out); err != nil {
		t.Errorf("get() on empty 200 body = %v, want nil", err)
	}
}

func TestDoJSON_NilResultSkipsUnmarshal(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Body is invalid JSON but result is nil so it must not be decoded.
		_, _ = w.Write([]byte("not json"))
	})
	if err := c.put(context.Background(), "/do-something"); err != nil {
		t.Errorf("put() should ignore body when result is nil, got err = %v", err)
	}
}

func TestDoJSON_ResponseExceedingCapErrors(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), maxResponseBody+1)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	})
	var out map[string]any
	var err error
	log := captureSlog(t, func() {
		err = c.get(context.Background(), "/library/metadata/1", &out)
	})
	if err == nil {
		t.Fatal("get() on an over-cap response must return an error, not silently truncate")
	}
	if !strings.Contains(err.Error(), "read cap") {
		t.Errorf("get() error = %v, want it to mention the read cap", err)
	}
	if !strings.Contains(log, "plex API response exceeded read cap") {
		t.Errorf("missing operator-facing WARN on cap hit; log: %q", log)
	}
}

// --- Tests: Episode / ShowEpisodes / SeasonEpisodes ---

func TestEpisode_InvalidRatingKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("handler should not be called for invalid rating key, got %s", r.URL.Path)
	})
	_, err := c.Episode(context.Background(), RatingKey("../etc/passwd"))
	if err == nil {
		t.Fatal("Episode() with non-numeric key should return error")
	}
}

func TestEpisode_NotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	_, err := c.Episode(context.Background(), RatingKey("123"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Episode() on empty Metadata = %v, want ErrNotFound", err)
	}
}

func TestEpisode_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/456" {
			t.Errorf("path = %q, want /library/metadata/456", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"ratingKey":"456","grandparentTitle":"Show","parentIndex":"2","index":"3","type":"episode"}` +
			`]}}`))
	})
	ep, err := c.Episode(context.Background(), RatingKey("456"))
	if err != nil {
		t.Fatalf("Episode() error = %v", err)
	}
	if ep.RatingKey != "456" || ep.SeasonNum() != 2 || ep.EpisodeNum() != 3 {
		t.Errorf("episode = %+v, want rk=456 S02E03", ep)
	}
}

func TestShowEpisodes_InvalidKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("handler should not be called for invalid show key")
	})
	_, err := c.ShowEpisodes(context.Background(), RatingKey("abc"))
	if err == nil {
		t.Fatal("ShowEpisodes() with non-numeric key should return error")
	}
}

func TestShowEpisodes_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/42/allLeaves" {
			t.Errorf("path = %q, want /library/metadata/42/allLeaves", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"ratingKey":"1","parentIndex":"1","index":"1"},` +
			`{"ratingKey":"2","parentIndex":"1","index":"2"}` +
			`]}}`))
	})
	eps, err := c.ShowEpisodes(context.Background(), RatingKey("42"))
	if err != nil {
		t.Fatalf("ShowEpisodes() error = %v", err)
	}
	if len(eps) != 2 {
		t.Errorf("len(eps) = %d, want 2", len(eps))
	}
}

func TestSeasonEpisodes_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/10/children" {
			t.Errorf("path = %q, want /library/metadata/10/children", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"ratingKey":"101","parentIndex":"2","index":"1"},` +
			`{"ratingKey":"102","parentIndex":"2","index":"2"}` +
			`]}}`))
	})
	eps, err := c.SeasonEpisodes(context.Background(), RatingKey("10"))
	if err != nil {
		t.Fatalf("SeasonEpisodes() error = %v", err)
	}
	if len(eps) != 2 {
		t.Errorf("len(eps) = %d, want 2", len(eps))
	}
}

func TestSeasonEpisodes_InvalidKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called for invalid season key")
	})
	_, err := c.SeasonEpisodes(context.Background(), RatingKey("abc"))
	if err == nil {
		t.Fatal("SeasonEpisodes() with non-numeric key should return error")
	}
}

func TestRecentlyAdded_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections/5/all" {
			t.Errorf("path = %q, want /library/sections/5/all", r.URL.Path)
		}
		q := r.URL.RawQuery
		if !strings.Contains(q, "type=4") {
			t.Errorf("query %q missing type=4", q)
		}
		if !strings.Contains(q, "addedAt") {
			t.Errorf("query %q missing addedAt filter", q)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"ratingKey":"200","parentIndex":"1","index":"1","type":"episode"}` +
			`]}}`))
	})
	eps, err := c.RecentlyAdded(context.Background(), RatingKey("5"), 1700000000)
	if err != nil {
		t.Fatalf("RecentlyAdded() error = %v", err)
	}
	if len(eps) != 1 || eps[0].RatingKey != "200" {
		t.Errorf("RecentlyAdded() = %+v, want 1 episode with key 200", eps)
	}
}

func TestRecentlyAdded_InvalidSectionKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called for invalid section key")
	})
	_, err := c.RecentlyAdded(context.Background(), RatingKey("abc"), 0)
	if err == nil {
		t.Fatal("RecentlyAdded() with non-numeric key should return error")
	}
}

// TestRecentlyAdded_FilterUsesSingleGTEOperator pins the addedAt>= filter to a
// single literal > operator. Plex silently ignores a doubled >> and returns the
// full unfiltered library, which overflowed the 10 MB read cap and caused a
// 14-day daily-failure outage on the sibling viewedAt>= path. The happy-path
// test only checks Contains(q, "addedAt"), which a >>= regression still passes.
func TestRecentlyAdded_FilterUsesSingleGTEOperator(t *testing.T) {
	t.Parallel()
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	if _, err := c.RecentlyAdded(context.Background(), RatingKey("5"), 1700000000); err != nil {
		t.Fatalf("RecentlyAdded() error = %v", err)
	}
	if !strings.Contains(gotQuery, "addedAt>=1700000000") {
		t.Errorf("query %q must contain single-operator addedAt>=1700000000", gotQuery)
	}
	if strings.Contains(gotQuery, "addedAt>>") {
		t.Errorf("query %q contains doubled >> operator; Plex silently ignores it and returns the full library", gotQuery)
	}
}

// --- Tests: ShowSections ---

func TestShowSections_FiltersNonShow(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[` +
			`{"key":"1","title":"Movies","type":"movie"},` +
			`{"key":"2","title":"TV","type":"show"},` +
			`{"key":"3","title":"Music","type":"artist"}` +
			`]}}`))
	})
	got, err := c.ShowSections(context.Background())
	if err != nil {
		t.Fatalf("ShowSections() error = %v", err)
	}
	if len(got) != 1 || got[0].Key != "2" {
		t.Errorf("ShowSections() = %+v, want only the TV show section", got)
	}
}

// --- Tests: ShowMetadata (runtime-types-p1 split) ---

// ShowMetadata returns *Show with Label + LibraryTitle. Before the split it
// delegated to Episode and returned *Episode; now it's its own library hit
// but the wire behaviour (path, label decoding) is identical.
func TestShowMetadata_DecodesShowResponse(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/42" {
			t.Errorf("path = %q, want /library/metadata/42", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"42","Label":[{"tag":"PLS_IGNORE"}]}]}}`))
	})
	show, err := c.ShowMetadata(context.Background(), RatingKey("42"))
	if err != nil {
		t.Fatalf("ShowMetadata() error = %v", err)
	}
	if show.RatingKey != "42" {
		t.Errorf("show.RatingKey = %q, want 42", show.RatingKey)
	}
	if len(show.Label) != 1 || show.Label[0].Tag != "PLS_IGNORE" {
		t.Errorf("show.Label = %+v, want [{Tag:PLS_IGNORE}]", show.Label)
	}
}

func TestShowMetadata_InvalidKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called for invalid key")
	})
	_, err := c.ShowMetadata(context.Background(), RatingKey("abc"))
	if err == nil {
		t.Fatal("ShowMetadata() with non-numeric key should return error")
	}
}

func TestShowMetadata_NotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	_, err := c.ShowMetadata(context.Background(), RatingKey("42"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ShowMetadata() on empty Metadata = %v, want ErrNotFound", err)
	}
}

// --- Tests: UserFromSession ---

func TestUserFromSession_Match(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"User":{"id":"7","title":"alice"},"Player":{"machineIdentifier":"mac-A"}},` +
			`{"User":{"id":"9","title":"bob"},"Player":{"machineIdentifier":"mac-B"}}` +
			`]}}`))
	})
	uid, uname, err := c.UserFromSession(context.Background(), "mac-B")
	if err != nil {
		t.Fatalf("UserFromSession() error = %v", err)
	}
	if uid != "9" || uname != "bob" {
		t.Errorf("got (%q,%q), want (9,bob)", uid, uname)
	}
}

func TestUserFromSession_NoMatch(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	_, _, err := c.UserFromSession(context.Background(), "mac-X")
	if err == nil {
		t.Fatal("UserFromSession() on no match should return error")
	}
}

// --- Tests: ServerIdentity ---

func TestServerIdentity_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("path = %q, want /", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"abc","version":"1.40"}}`))
	})
	id, err := c.ServerIdentity(context.Background())
	if err != nil {
		t.Fatalf("ServerIdentity() error = %v", err)
	}
	if id.FriendlyName != "Plex" || id.MachineIdentifier != "abc" || id.Version != "1.40" {
		t.Errorf("identity = %+v", id)
	}
}

// --- Tests: LoggedUser ---

func TestLoggedUser_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/myplex/account":
			_, _ = w.Write([]byte(`{"username":"admin"}`))
		case "/accounts":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Account":[` +
				`{"name":"guest","id":2},` +
				`{"name":"admin","id":1}` +
				`]}}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	})
	user, err := c.LoggedUser(context.Background())
	if err != nil {
		t.Fatalf("LoggedUser() error = %v", err)
	}
	if user.ID != "1" || user.Name != "admin" {
		t.Errorf("LoggedUser() = %+v, want ID=1 Name=admin", user)
	}
}

func TestLoggedUser_AdminNotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/myplex/account":
			_, _ = w.Write([]byte(`{"username":"missing-user"}`))
		case "/accounts":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Account":[` +
				`{"name":"other","id":99}` +
				`]}}`))
		}
	})
	_, err := c.LoggedUser(context.Background())
	if err == nil {
		t.Fatal("LoggedUser() should fail when admin not in system accounts")
	}
}

func TestLoggedUser_AccountFetchError(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/myplex/account" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})
	_, err := c.LoggedUser(context.Background())
	if err == nil {
		t.Fatal("LoggedUser() should fail on account fetch error")
	}
}

func TestLoggedUser_SystemAccountsFetchError(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/myplex/account":
			_, _ = w.Write([]byte(`{"username":"admin"}`))
		case "/accounts":
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	_, err := c.LoggedUser(context.Background())
	if err == nil {
		t.Fatal("LoggedUser() should fail on system accounts fetch error")
	}
}

// --- Tests: History (viewedAt>= query contract) ---

func TestHistory_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status/sessions/history/all" {
			t.Errorf("path = %q, want /status/sessions/history/all", r.URL.Path)
		}
		// The query must use viewedAt>= (single >) not viewedAt>>= (double >).
		// A prior bug used double > which Plex silently ignores, returning
		// the full unfiltered history and overflowing the 10 MB read cap.
		q := r.URL.RawQuery
		if !strings.Contains(q, "viewedAt>=1700000000") {
			t.Errorf("query %q missing correct viewedAt>= filter", q)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"ratingKey":"300","type":"episode","accountID":"1","librarySectionID":"2","librarySectionTitle":"TV"}` +
			`]}}`))
	})
	items, err := c.History(context.Background(), 1700000000)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(items) != 1 || items[0].RatingKey != "300" {
		t.Errorf("History() = %+v, want 1 item with key 300", items)
	}
}

func TestHistory_EmptyResult(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	items, err := c.History(context.Background(), 1700000000)
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(items) != 0 {
		t.Errorf("History() = %+v, want empty", items)
	}
}

// --- Tests: SetAudioStream / SetSubtitleStream / DisableSubtitles ---

func TestSetAudioStream_PUTPath(t *testing.T) {
	t.Parallel()
	var gotPath, gotMethod string
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotMethod = r.Method
	})
	if err := c.SetAudioStream(context.Background(), 100, 200); err != nil {
		t.Fatalf("SetAudioStream() error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	want := "/library/parts/100?audioStreamID=200&allParts=1"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// TestSetSubtitleStream_PUTPath mirrors TestSetAudioStream_PUTPath for the
// subtitle setter, which had no direct test (0% coverage). It pins the PUT
// method and the subtitleStreamID + allParts=1 query contract.
func TestSetSubtitleStream_PUTPath(t *testing.T) {
	t.Parallel()
	var gotPath, gotMethod string
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotMethod = r.Method
	})
	if err := c.SetSubtitleStream(context.Background(), 100, 200); err != nil {
		t.Fatalf("SetSubtitleStream() error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	want := "/library/parts/100?subtitleStreamID=200&allParts=1"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestDisableSubtitles_UsesStreamID0(t *testing.T) {
	t.Parallel()
	var gotPath string
	c := newTestClient(t, func(_ http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
	})
	if err := c.DisableSubtitles(context.Background(), 100); err != nil {
		t.Fatalf("DisableSubtitles() error = %v", err)
	}
	want := "/library/parts/100?subtitleStreamID=0&allParts=1"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// --- Tests: SharedServersXML parsing (plex.tv response shape) ---

func TestParseSharedServersXML(t *testing.T) {
	input := `<MediaContainer>
  <SharedServer id="12345" username="friend1" userID="67890" accessToken="abc123"/>
  <SharedServer id="12346" username="friend2" userID="67891" accessToken="def456"/>
</MediaContainer>`

	var result SharedServersXML
	if err := xml.Unmarshal([]byte(input), &result); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v", err)
	}
	if len(result.SharedServer) != 2 {
		t.Fatalf("expected 2 shared servers, got %d", len(result.SharedServer))
	}

	s := result.SharedServer[0]
	if s.UserID != "67890" || s.Username != "friend1" || s.AccessToken != "abc123" {
		t.Errorf("first server: got userID=%q username=%q token=%q",
			s.UserID, s.Username, s.AccessToken)
	}

	s = result.SharedServer[1]
	if s.UserID != "67891" || s.Username != "friend2" || s.AccessToken != "def456" {
		t.Errorf("second server: got userID=%q username=%q token=%q",
			s.UserID, s.Username, s.AccessToken)
	}
}

func TestParseSharedServersXMLEmpty(t *testing.T) {
	input := `<MediaContainer></MediaContainer>`
	var result SharedServersXML
	if err := xml.Unmarshal([]byte(input), &result); err != nil {
		t.Fatalf("xml.Unmarshal failed: %v", err)
	}
	if len(result.SharedServer) != 0 {
		t.Errorf("expected 0 shared servers, got %d", len(result.SharedServer))
	}
}

// plexTVRewriteTransport redirects the hardcoded https://plex.tv/... request in
// SharedUserTokens to a local httptest server, the documented purpose of
// SwapTVClient. It rewrites scheme+host on every request.
type plexTVRewriteTransport struct {
	base http.RoundTripper
	host string
}

func (rt plexTVRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = rt.host
	return rt.base.RoundTrip(req)
}

// TestSharedUserTokens exercises the plex.tv shared_servers call via SwapTVClient:
// a host-rewriting transport points the hardcoded plex.tv URL at a local server.
// It must not be parallel (SwapTVClient mutates the process-global tvClient).
func TestSharedUserTokens(t *testing.T) {
	newTVClient := func(t *testing.T, h http.HandlerFunc) *Client {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(SwapTVClient(&http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}))
		return &Client{token: "admin-token"}
	}

	t.Run("happy path parses servers and sends auth", func(t *testing.T) {
		var gotToken, gotAccept, gotReqURI string
		c := newTVClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotToken = r.Header.Get("X-Plex-Token")
			gotAccept = r.Header.Get("Accept")
			gotReqURI = r.RequestURI
			_, _ = w.Write([]byte(`<MediaContainer>` +
				`<SharedServer userID="1" username="alice" accessToken="tok-a"/>` +
				`<SharedServer userID="2" username="bob" accessToken="tok-b"/>` +
				`</MediaContainer>`))
		})
		servers, err := c.SharedUserTokens(context.Background(), "machine/../id")
		if err != nil {
			t.Fatalf("SharedUserTokens() error = %v", err)
		}
		if len(servers) != 2 || servers[0].AccessToken != "tok-a" || servers[1].UserID != "2" {
			t.Errorf("servers = %+v, want 2 parsed entries", servers)
		}
		if gotToken != "admin-token" {
			t.Errorf("X-Plex-Token = %q, want admin-token", gotToken)
		}
		if gotAccept != "application/xml" {
			t.Errorf("Accept = %q, want application/xml", gotAccept)
		}
		if !strings.Contains(gotReqURI, "machine%2F..%2Fid") {
			t.Errorf("RequestURI %q must contain PathEscaped machineIdentifier machine%%2F..%%2Fid", gotReqURI)
		}
	})

	t.Run("non-200 status returns error", func(t *testing.T) {
		c := newTVClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		if _, err := c.SharedUserTokens(context.Background(), "abc"); err == nil {
			t.Error("SharedUserTokens() on 502 should return error")
		}
	})

	t.Run("malformed XML returns error", func(t *testing.T) {
		c := newTVClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<MediaContainer><SharedServer`))
		})
		if _, err := c.SharedUserTokens(context.Background(), "abc"); err == nil {
			t.Error("SharedUserTokens() on malformed XML should return error")
		}
	})
}

func TestSharedUserTokens_ResponseExceedingCapErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), maxResponseBody+1))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SwapTVClient(&http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}))
	c := &Client{token: "admin-token"}

	_, stErr := c.SharedUserTokens(context.Background(), "machine-id")
	if stErr == nil {
		t.Fatal("SharedUserTokens() on an over-cap response must return an error")
	}
	if !strings.Contains(stErr.Error(), "read cap") {
		t.Errorf("SharedUserTokens() error = %v, want it to mention the read cap", stErr)
	}
}

func TestSharedUserTokens_EmptyBodyReturnsNoServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SwapTVClient(&http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}))
	c := &Client{token: "admin-token"}

	servers, stErr := c.SharedUserTokens(context.Background(), "machine-id")
	if stErr != nil {
		t.Fatalf("SharedUserTokens() on an empty body must not error, got %v", stErr)
	}
	if servers != nil {
		t.Errorf("SharedUserTokens() on an empty body = %+v, want nil (zero shared servers)", servers)
	}
}

// TestDoJSON_AcceptsResponseExactlyAtCap is the boundary companion to
// TestDoJSON_ResponseExceedingCapErrors. The read cap is a strict >
// comparison, so a body of exactly maxResponseBody bytes is the largest
// response that must still be accepted and decoded — a >= regression would
// reject a legitimate at-cap 10 MB response (e.g. a large but valid
// allLeaves listing). The body is valid JSON padded to the exact cap size.
func TestDoJSON_AcceptsResponseExactlyAtCap(t *testing.T) {
	const wrapper = `{"x":""}` // structural overhead around the padding
	body := make([]byte, 0, maxResponseBody)
	body = append(body, `{"x":"`...)
	body = append(body, bytes.Repeat([]byte("a"), maxResponseBody-len(wrapper))...)
	body = append(body, `"}`...)
	if len(body) != maxResponseBody {
		t.Fatalf("test setup: body len = %d, want exactly %d", len(body), maxResponseBody)
	}
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	var out struct {
		X string `json:"x"`
	}
	if err := c.get(context.Background(), "/library/metadata/1", &out); err != nil {
		t.Fatalf("get() on a body exactly at the cap = %v, want nil (exact-cap must be accepted)", err)
	}
	if len(out.X) != maxResponseBody-len(wrapper) {
		t.Errorf("decoded field len = %d, want %d (at-cap body must be fully read and decoded)",
			len(out.X), maxResponseBody-len(wrapper))
	}
}

// TestSharedUserTokens_AcceptsResponseExactlyAtCap is the boundary companion to
// TestSharedUserTokens_ResponseExceedingCapErrors. Like the JSON read path, the
// plex.tv read cap is a strict > comparison, so a body of exactly
// maxResponseBody bytes must be parsed, not rejected. The body is valid XML (a
// MediaContainer wrapping a comment) padded to the exact cap size.
func TestSharedUserTokens_AcceptsResponseExactlyAtCap(t *testing.T) {
	const prefix = `<MediaContainer><!--`
	const suffix = `--></MediaContainer>`
	body := make([]byte, 0, maxResponseBody)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte("a"), maxResponseBody-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	if len(body) != maxResponseBody {
		t.Fatalf("test setup: body len = %d, want exactly %d", len(body), maxResponseBody)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SwapTVClient(&http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}))
	c := &Client{token: "admin-token"}

	servers, stErr := c.SharedUserTokens(context.Background(), "machine-id")
	if stErr != nil {
		t.Fatalf("SharedUserTokens() on a body exactly at the cap = %v, want nil (exact-cap must be accepted)", stErr)
	}
	if len(servers) != 0 {
		t.Errorf("SharedUserTokens() = %d servers, want 0 (comment-only MediaContainer)", len(servers))
	}
}
