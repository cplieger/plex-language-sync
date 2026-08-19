package plex

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cplieger/httpx/v5/certtest"
	"github.com/cplieger/plexapi/v2"
)

// newClientForServer builds a Client whose requests to the Plex server go
// through hc — the shape a test needs to aim a client at an httptest.Server.
// It is the in-package twin of
// testsupport/plexclient.NewFromHTTP, duplicated here because this file is
// `package plex`: importing the fixture package (which imports plex) would
// be an import cycle.
//
// Split into two single-purpose helpers rather than one taking `hc, tv
// *http.Client`: that adjacent same-type pair type-checks transposed, and
// swapping it aims the server client at plex.tv and vice versa — the test
// would then pass or fail for the wrong reason. No call site needs both, so
// the pair never has to exist.
func newClientForServer(t *testing.T, baseURL *url.URL, token plexapi.Token, hc *http.Client) *Client {
	t.Helper()
	return newClient(t, baseURL, token, plexapi.WithHTTPClient(hc))
}

// newClientForTV builds a Client whose plex.tv (shared-server) lookups go
// through tv, leaving the server client on the library's hardened default.
func newClientForTV(t *testing.T, baseURL *url.URL, token plexapi.Token, tv *http.Client) *Client {
	t.Helper()
	c := newClient(t, baseURL, token)
	c.TVClient = tv
	return c
}

func newClient(t *testing.T, baseURL *url.URL, token plexapi.Token, opts ...plexapi.Option) *Client {
	t.Helper()
	api, err := plexapi.New(baseURL.String(), token, opts...)
	if err != nil {
		t.Fatalf("newClient(%s): %v", baseURL, err)
	}
	return &Client{Client: api}
}

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
	return newClientForServer(t, u, "test-token", srv.Client())
}

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
	c, err := NewClient(Options{ServerURL: "http://plex:32400", Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if c.BaseURL().Host != "plex:32400" {
		t.Errorf("baseURL.Host = %q, want plex:32400", c.BaseURL().Host)
	}
}

func TestNewClient_InvalidURL(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Options{ServerURL: "://bad", Token: "tok"})
	if err == nil {
		t.Fatal("NewClient() with invalid URL should return error")
	}
}

func TestNewClient_BadScheme(t *testing.T) {
	t.Parallel()
	_, err := NewClient(Options{ServerURL: "ftp://plex:32400", Token: "tok"})
	if err == nil {
		t.Fatal("NewClient() with ftp scheme should return error")
	}
}

// --- Tests: ForToken ---

func TestForToken(t *testing.T) {
	t.Parallel()
	c, err := NewClient(Options{ServerURL: "http://plex:32400", Token: "admin-token"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	u := c.ForToken("user-token")
	if u.Token() != "user-token" {
		t.Errorf("token = %q, want user-token", u.Token())
	}
	if u.BaseURL().String() != c.BaseURL().String() {
		t.Error("ForToken must keep the same server base URL")
	}
	// Pool sharing itself is the library's pinned ForToken contract
	// (plexapi's own white-box test); observable here: the derivation
	// keeps the hardened base transport.
	if u.BaseTransport() == nil {
		t.Error("ForToken derivation lost the hardened base transport")
	}
	if c.Token() != "admin-token" {
		t.Error("ForToken must not mutate the source client's token")
	}
}

// --- Tests: NewClient CA pinning ---

func TestNewClient_CACert(t *testing.T) {
	t.Parallel()
	caPath := certtest.WriteSelfSignedCA(t)
	c, err := NewClient(Options{ServerURL: "https://plex:32400", Token: "test-token", CACertPath: caPath})
	if err != nil {
		t.Fatalf("NewClient with CA path: %v", err)
	}
	if c.BaseTransport() == nil {
		t.Fatal("expected the hardened base transport when caCertPath set")
	}
	// The pinned base transport must survive into ForToken derivations —
	// the per-user write path keeps the same trust anchors.
	if c.ForToken("other").BaseTransport() == nil {
		t.Error("ForToken derivation lost the hardened base transport")
	}
}

// --- Tests: newHTTPClient ---

// --- Tests: newHTTPClient refuses redirects (PLEX-LS-SEC-01) ---

// --- Tests: WarnIfPlaintextURL (PLEX-LS-SEC-02) ---

// --- Tests: drainBody ---

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
	if err := c.Get(t.Context(), "/some/path", &out); err != nil {
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
	err := c.Get(t.Context(), "/missing", &out)
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
	err := c.Get(t.Context(), "/boom", &out)
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
	if err := c.Get(t.Context(), "/", &out); err != nil {
		t.Errorf("get() on empty 200 body = %v, want nil", err)
	}
}

func TestDoJSON_NilResultSkipsUnmarshal(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		// Body is invalid JSON but result is nil so it must not be decoded.
		_, _ = w.Write([]byte("not json"))
	})
	if err := c.Get(t.Context(), "/do-something", nil); err != nil {
		t.Errorf("put() should ignore body when result is nil, got err = %v", err)
	}
}

func TestDoJSON_ResponseExceedingCapErrors(t *testing.T) {
	const capBytes = 10 << 20 // the library's per-response read cap
	oversized := bytes.Repeat([]byte("a"), capBytes+1)
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	})
	var err error
	log := captureSlog(t, func() {
		_, err = c.Episode(t.Context(), "1")
	})
	if err == nil {
		t.Fatal("get() on an over-cap response must return an error, not silently truncate")
	}
	if _, ok := errors.AsType[*plexapi.ResponseTooLargeError](err); !ok {
		t.Errorf("get() error = %v, want *plexapi.ResponseTooLargeError", err)
	}
	// The legacy alert string is the APP's Loki contract, emitted by the
	// adapter's fetch paths on the library's typed over-cap error.
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
	_, err := c.Episode(t.Context(), RatingKey("../etc/passwd"))
	if err == nil {
		t.Fatal("Episode() with non-numeric key should return error")
	}
}

func TestEpisode_NotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	_, err := c.Episode(t.Context(), RatingKey("123"))
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
	ep, err := c.Episode(t.Context(), RatingKey("456"))
	if err != nil {
		t.Fatalf("Episode() error = %v", err)
	}
	if ep.RatingKey != "456" || ep.SeasonNum() != 2 || ep.Num() != 3 {
		t.Errorf("episode = %+v, want rk=456 S02E03", ep)
	}
}

func TestShowEpisodes_InvalidKey(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("handler should not be called for invalid show key")
	})
	_, err := c.ShowEpisodes(t.Context(), RatingKey("abc"))
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
	eps, err := c.ShowEpisodes(t.Context(), RatingKey("42"))
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
	eps, err := c.SeasonEpisodes(t.Context(), RatingKey("10"))
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
	_, err := c.SeasonEpisodes(t.Context(), RatingKey("abc"))
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
	eps, err := c.RecentlyAdded(t.Context(), RatingKey("5"), 1700000000)
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
	_, err := c.RecentlyAdded(t.Context(), RatingKey("abc"), 0)
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
	if _, err := c.RecentlyAdded(t.Context(), RatingKey("5"), 1700000000); err != nil {
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
	got, err := c.ShowSections(t.Context())
	if err != nil {
		t.Fatalf("ShowSections() error = %v", err)
	}
	if len(got) != 1 || got[0].Key != "2" {
		t.Errorf("ShowSections() = %+v, want only the TV show section", got)
	}
}

// --- Tests: ShowMetadata (runtime-types-p1 split) ---

// ShowMetadata returns *Show with the show's labels. Before the split it
// delegated to Episode and returned *Episode; now it's its own library hit
// but the wire behaviour (path, label decoding) is identical.
//
// The payload deliberately carries a field Show does NOT declare
// (ratingKey) — real Plex sends it, and the non-strict decoder must ignore
// it rather than fail.
func TestShowMetadata_DecodesShowResponse(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/metadata/42" {
			t.Errorf("path = %q, want /library/metadata/42", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"42","Label":[{"tag":"PLS_IGNORE"}]}]}}`))
	})
	show, err := c.ShowMetadata(t.Context(), RatingKey("42"))
	if err != nil {
		t.Fatalf("ShowMetadata() error = %v", err)
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
	_, err := c.ShowMetadata(t.Context(), RatingKey("abc"))
	if err == nil {
		t.Fatal("ShowMetadata() with non-numeric key should return error")
	}
}

func TestShowMetadata_NotFound(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	})
	_, err := c.ShowMetadata(t.Context(), RatingKey("42"))
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
	uid, uname, err := c.UserFromSession(t.Context(), "mac-B")
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
	_, _, err := c.UserFromSession(t.Context(), "mac-X")
	if err == nil {
		t.Fatal("UserFromSession() on no match should return error")
	}
}

// --- Tests: Identity (embedded library method) ---

// TestIdentity_HappyPath pins the adapter wiring: the library's Identity
// is reachable through the embedded client (the app's former
// ServerIdentity pass-through was inlined away) and decodes into the
// aliased ServerIdentity shape main consumes.
func TestIdentity_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("path = %q, want /", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"abc","version":"1.40"}}`))
	})
	id, err := c.Identity(t.Context())
	if err != nil {
		t.Fatalf("Identity() error = %v", err)
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
	user, err := c.LoggedUser(t.Context())
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
	_, err := c.LoggedUser(t.Context())
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
	_, err := c.LoggedUser(t.Context())
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
	_, err := c.LoggedUser(t.Context())
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
	items, err := c.History(t.Context(), 1700000000)
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
	items, err := c.History(t.Context(), 1700000000)
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
	if err := c.SetAudioStream(t.Context(), plexapi.StreamSelection{PartID: 100, StreamID: 200}); err != nil {
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
	if err := c.SetSubtitleStream(t.Context(), plexapi.StreamSelection{PartID: 100, StreamID: 200}); err != nil {
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
	if err := c.DisableSubtitles(t.Context(), 100); err != nil {
		t.Fatalf("DisableSubtitles() error = %v", err)
	}
	want := "/library/parts/100?subtitleStreamID=0&allParts=1"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// plexTVRewriteTransport redirects the hardcoded https://plex.tv/... request in
// SharedUserTokens to a local httptest server. It rewrites scheme+host on
// every request; the client carrying it is injected per Client, so these
// tests need no process-global swap and stay parallel-safe.
type plexTVRewriteTransport struct {
	base http.RoundTripper
	host string
}

func (rt plexTVRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = rt.host
	return rt.base.RoundTrip(req)
}

// TestSharedUserTokens exercises the plex.tv shared_servers call: a
// host-rewriting transport points the hardcoded plex.tv URL at a local server,
// injected on the Client itself (Options.TVClient) rather than swapped into a
// package global, so this test and its subtests run parallel.
func TestSharedUserTokens(t *testing.T) {
	t.Parallel()

	newTVClient := func(t *testing.T, h http.HandlerFunc) *Client {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		tv := &http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}
		base, _ := url.Parse("http://plex.local:32400")
		return newClientForTV(t, base, "admin-token", tv)
	}

	t.Run("happy path parses servers and sends auth", func(t *testing.T) {
		t.Parallel()

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
		servers, err := c.SharedUserTokens(t.Context(), "machine/../id")
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
		t.Parallel()

		c := newTVClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		if _, err := c.SharedUserTokens(t.Context(), "abc"); err == nil {
			t.Error("SharedUserTokens() on 502 should return error")
		}
	})

	t.Run("malformed XML returns error", func(t *testing.T) {
		t.Parallel()

		c := newTVClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`<MediaContainer><SharedServer`))
		})
		if _, err := c.SharedUserTokens(t.Context(), "abc"); err == nil {
			t.Error("SharedUserTokens() on malformed XML should return error")
		}
	})
}

// Serial (no t.Parallel): allocates a 10 MB body. The four *Cap* tests in this
// file stay serial so their buffers do not stack up concurrently — the reason is
// memory, not a shared global.
func TestSharedUserTokens_ResponseExceedingCapErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("a"), (10<<20)+1))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tv := &http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}
	base, _ := url.Parse("http://plex.local:32400")
	c := newClientForTV(t, base, "admin-token", tv)

	_, stErr := c.SharedUserTokens(t.Context(), "machine-id")
	if stErr == nil {
		t.Fatal("SharedUserTokens() on an over-cap response must return an error")
	}
	if _, ok := errors.AsType[*plexapi.ResponseTooLargeError](stErr); !ok {
		t.Errorf("SharedUserTokens() error = %v, want *plexapi.ResponseTooLargeError", stErr)
	}
}

func TestSharedUserTokens_EmptyBodyReturnsNoServers(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tv := &http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}
	base, _ := url.Parse("http://plex.local:32400")
	c := newClientForTV(t, base, "admin-token", tv)

	servers, stErr := c.SharedUserTokens(t.Context(), "machine-id")
	if stErr != nil {
		t.Fatalf("SharedUserTokens() on an empty body must not error, got %v", stErr)
	}
	if servers != nil {
		t.Errorf("SharedUserTokens() on an empty body = %+v, want nil (zero shared servers)", servers)
	}
}

// TestDoJSON_AcceptsResponseExactlyAtCap is the boundary companion to
// maxResponseBodyTest mirrors the library's per-response read cap.
const maxResponseBodyTest = 10 << 20

// TestDoJSON_ResponseExceedingCapErrors. The read cap is a strict >
// comparison, so a body of exactly maxResponseBodyTest bytes is the largest
// response that must still be accepted and decoded — a >= regression would
// reject a legitimate at-cap 10 MB response (e.g. a large but valid
// allLeaves listing). The body is valid JSON padded to the exact cap size.
func TestDoJSON_AcceptsResponseExactlyAtCap(t *testing.T) {
	const wrapper = `{"x":""}` // structural overhead around the padding
	body := make([]byte, 0, maxResponseBodyTest)
	body = append(body, `{"x":"`...)
	body = append(body, bytes.Repeat([]byte("a"), maxResponseBodyTest-len(wrapper))...)
	body = append(body, `"}`...)
	if len(body) != maxResponseBodyTest {
		t.Fatalf("test setup: body len = %d, want exactly %d", len(body), maxResponseBodyTest)
	}
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})
	var out struct {
		X string `json:"x"`
	}
	if err := c.Get(t.Context(), "/library/metadata/1", &out); err != nil {
		t.Fatalf("get() on a body exactly at the cap = %v, want nil (exact-cap must be accepted)", err)
	}
	if len(out.X) != maxResponseBodyTest-len(wrapper) {
		t.Errorf("decoded field len = %d, want %d (at-cap body must be fully read and decoded)",
			len(out.X), maxResponseBodyTest-len(wrapper))
	}
}

// xmlCommentPadding returns exactly n bytes of XML comment padding, split so no
// single comment approaches plexapi's per-token bound (16 KiB for the plex.tv
// shared-servers limits). Sizing is derived rather than hardcoded so a change to
// the read cap does not silently reintroduce an over-long comment.
func xmlCommentPadding(t *testing.T, n int) []byte {
	t.Helper()
	const payloadMax = 8 << 10 // comfortably under the 16 KiB token bound
	overhead := len("<!--") + len("-->")
	if n < overhead {
		t.Fatalf("test setup: %d bytes cannot hold an XML comment (%d of overhead)", n, overhead)
	}
	blocks := (n + payloadMax + overhead - 1) / (payloadMax + overhead)
	payload := n - blocks*overhead
	out := make([]byte, 0, n)
	// Spread the payload evenly; the divisor shrinks each pass so integer
	// truncation lands the remainder in the final comments rather than dropping it.
	for remaining := blocks; remaining > 0; remaining-- {
		size := payload / remaining
		payload -= size
		out = append(out, "<!--"...)
		out = append(out, bytes.Repeat([]byte("a"), size)...)
		out = append(out, "-->"...)
	}
	if len(out) != n {
		t.Fatalf("test setup: padding len = %d, want exactly %d", len(out), n)
	}
	return out
}

// TestSharedUserTokens_AcceptsResponseExactlyAtCap is the boundary companion to
// TestSharedUserTokens_ResponseExceedingCapErrors. Like the JSON read path, the
// plex.tv read cap is a strict > comparison, so a body of exactly
// maxResponseBodyTest bytes must be parsed, not rejected.
//
// The padding is split across many comments rather than one 10 MB comment:
// plexapi runs an xmlx preflight over this response before encoding/xml sees it,
// and that preflight bounds each token (16 KiB for the shared-servers limits).
// A single cap-sized comment is valid XML but not a plausible plex.tv response,
// and the preflight rightly refuses it — which would make this test assert the
// preflight rather than the read cap it exists to pin.
//
// Serial (no t.Parallel): allocates a 10 MB body — the reason is memory, not a
// shared global.
func TestSharedUserTokens_AcceptsResponseExactlyAtCap(t *testing.T) {
	const prefix = `<MediaContainer>`
	const suffix = `</MediaContainer>`
	body := make([]byte, 0, maxResponseBodyTest)
	body = append(body, prefix...)
	body = append(body, xmlCommentPadding(t, maxResponseBodyTest-len(prefix)-len(suffix))...)
	body = append(body, suffix...)
	if len(body) != maxResponseBodyTest {
		t.Fatalf("test setup: body len = %d, want exactly %d", len(body), maxResponseBodyTest)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tv := &http.Client{Transport: plexTVRewriteTransport{http.DefaultTransport, u.Host}}
	base, _ := url.Parse("http://plex.local:32400")
	c := newClientForTV(t, base, "admin-token", tv)

	servers, stErr := c.SharedUserTokens(t.Context(), "machine-id")
	if stErr != nil {
		t.Fatalf("SharedUserTokens() on a body exactly at the cap = %v, want nil (exact-cap must be accepted)", stErr)
	}
	if len(servers) != 0 {
		t.Errorf("SharedUserTokens() = %d servers, want 0 (comment-only MediaContainer)", len(servers))
	}
}
