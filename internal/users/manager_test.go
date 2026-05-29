package users

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"plex-language-sync/internal/api"
	"plex-language-sync/internal/plex"
	"plex-language-sync/internal/testsupport/fakeapi"
)

// Verify *Manager satisfies api.UserLookup at compile time.
var _ api.UserLookup = (*Manager)(nil)

func TestID_StringRoundTrip(t *testing.T) {
	id := ID("42")
	if id.String() != "42" {
		t.Errorf("ID(%q).String() = %q, want %q", "42", id.String(), "42")
	}
	if ID("") != "" {
		t.Error("empty ID should compare equal to empty string")
	}
}

func TestManager_InitSeedsAdminAndBaseURL(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	m := NewManager(fakeapi.NewCache())
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	admin := m.Admin()
	if admin.ID != "1" || admin.Name != "admin" {
		t.Errorf("admin = %+v, want ID=1 Name=admin", admin)
	}
}

func TestManager_InitPreservesExistingShared(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	m := NewManager(fakeapi.NewCache())
	fc := m.cache.(*fakeapi.Cache)
	fc.SetUserTokens(map[string]string{"2": "pre-token"})
	m.LoadFromCache()

	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	// After Init, shared user "2" should still be resolvable because
	// Init only resets the clients cache, not the shared map.
	if got := m.Name("2"); got != "user-2" {
		t.Errorf("Name(2) after Init = %q, want user-2", got)
	}
}

func TestManager_LoadFromCacheSeedsTokens(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{
		"2": "friend-token",
		"3": "other-token",
	})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	if m.SharedCount() != 2 {
		t.Errorf("SharedCount = %d, want 2", m.SharedCount())
	}
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", nil)
	if m.ClientForUser("2", adminClient).Token() != "friend-token" {
		t.Errorf("ClientForUser(2) token mismatch")
	}
}

func TestManager_LoadFromCacheSkipsAdmin(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{
		"1": "admin-token-from-cache",
		"2": "friend-token",
	})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	// SharedCount should be 1 — the admin entry must be ignored.
	if m.SharedCount() != 1 {
		t.Errorf("SharedCount = %d, want 1 (admin should be skipped)", m.SharedCount())
	}
}

func TestManager_ClientForUser(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"2": "friend-token"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", nil)

	// Admin user returns the admin client.
	if got := m.ClientForUser("1", adminClient); got != adminClient {
		t.Error("expected admin client for admin userID")
	}
	// Known shared user returns a new client with their token.
	if got := m.ClientForUser("2", adminClient); got.Token() != "friend-token" {
		t.Errorf("expected friend-token, got %q", got.Token())
	}
	// Unknown user falls back to admin.
	if got := m.ClientForUser("999", adminClient); got != adminClient {
		t.Error("expected admin client for unknown userID")
	}
}

func TestManager_AllReturnsAdminAndShared(t *testing.T) {
	parsed, _ := url.Parse("http://plex:32400")
	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"2": "t-bob", "3": "t-carol"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	all := m.All()
	if len(all) != 3 {
		t.Fatalf("All() len = %d, want 3", len(all))
	}
	// Admin must be the first entry and have an empty token surface.
	if all[0].ID != "1" {
		t.Errorf("All()[0].ID = %q, want 1", all[0].ID)
	}
	if all[0].Token != "" {
		t.Error("admin entry must not expose a token via All()")
	}
}

func TestManager_NameUnknownReturnsPlaceholder(t *testing.T) {
	m := NewManager(fakeapi.NewCache())
	m.Init(&plex.User{ID: "1", Name: "admin"}, &url.URL{}, "")
	if got := m.Name("999"); got != "unknown-999" {
		t.Errorf("Name(unknown) = %q, want unknown-999", got)
	}
}

func TestManager_ConcurrentClientForUserAndTokenUpdate(t *testing.T) {
	t.Parallel()
	parsed, _ := url.Parse("http://plex:32400")
	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"2": "t-v1"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", nil)

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for range N {
		go func() {
			defer wg.Done()
			// Simulate a concurrent cache-token rotation; the
			// production path runs via RefreshTokens, but writing
			// directly exercises the same lock discipline.
			fc.SetUserTokens(map[string]string{"2": "t-rotating"})
		}()
		go func() {
			defer wg.Done()
			_ = m.ClientForUser("2", adminClient)
			_ = m.Name("2")
			_ = m.All()
		}()
	}
	wg.Wait()
}

// --- RefreshTokens + retry loop tests ---

// roundTripFunc adapts a function to http.RoundTripper for redirecting
// plex.tv API calls to a local httptest server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// swapPlexTVClient redirects the package-level plex.tv client to the
// given test server. Tests using this helper must NOT use t.Parallel()
// because they mutate a package-level var in internal/plex.
func swapPlexTVClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	replacement := &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = srv.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
	restore := plex.SwapTVClient(replacement)
	t.Cleanup(restore)
}

func TestRefreshTokens_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "admin-token" {
			t.Errorf("X-Plex-Token = %q, want admin-token", r.Header.Get("X-Plex-Token"))
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer>` +
			`<SharedServer id="1" username="friend1" userID="100" accessToken="token-100"/>` +
			`<SharedServer id="2" username="friend2" userID="200" accessToken="token-200"/>` +
			`</MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	fc := fakeapi.NewCache()
	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	m.RefreshTokens(context.Background(), adminClient, "machine-id-123")

	if m.SharedCount() != 2 {
		t.Fatalf("SharedCount = %d, want 2", m.SharedCount())
	}
	if got := fc.UserTokens()["100"]; got != "token-100" {
		t.Errorf("cache token 100 = %q, want token-100", got)
	}
	if got := fc.UserTokens()["200"]; got != "token-200" {
		t.Errorf("cache token 200 = %q, want token-200", got)
	}
	if got := m.ClientForUser("100", adminClient); got.Token() != "token-100" {
		t.Errorf("ClientForUser(100) token = %q, want token-100", got.Token())
	}
}

func TestRefreshTokens_EvictsRevokedUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer>` +
			`<SharedServer id="1" username="friend1" userID="100" accessToken="new-token-100"/>` +
			`</MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"100": "old-token-100", "200": "token-200"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	// Pre-populate the per-user client cache so we can assert eviction.
	_ = m.ClientForUser("100", adminClient)
	_ = m.ClientForUser("200", adminClient)

	m.RefreshTokens(context.Background(), adminClient, "machine-id-123")

	if m.SharedCount() != 1 {
		t.Errorf("SharedCount = %d, want 1 (user 200 revoked)", m.SharedCount())
	}
	// User 100 should now return a client with the rotated token.
	if got := m.ClientForUser("100", adminClient); got.Token() != "new-token-100" {
		t.Errorf("ClientForUser(100) token = %q, want new-token-100", got.Token())
	}
	// User 200 should fall back to admin (no longer shared).
	if got := m.ClientForUser("200", adminClient); got != adminClient {
		t.Error("ClientForUser(200) should fall back to admin after revocation")
	}

	tokens := fc.UserTokens()
	if len(tokens) != 1 {
		t.Errorf("cache should have 1 token, got %d", len(tokens))
	}
	if tokens["100"] != "new-token-100" {
		t.Errorf("cache token 100 = %q, want new-token-100", tokens["100"])
	}
}

func TestRefreshTokens_APIFailureKeepsExistingState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"100": "existing-token"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	m.RefreshTokens(context.Background(), adminClient, "machine-id-123")

	if m.SharedCount() != 1 {
		t.Errorf("SharedCount = %d, want 1 (state preserved on plex.tv failure)", m.SharedCount())
	}
	if got := fc.UserTokens()["100"]; got != "existing-token" {
		t.Errorf("cache should be unchanged after failure, got %q", got)
	}
}

func TestRefreshTokens_SkipsEmptyUserIDOrToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer>` +
			`<SharedServer id="1" username="valid" userID="100" accessToken="token-100"/>` +
			`<SharedServer id="2" username="no-token" userID="200" accessToken=""/>` +
			`<SharedServer id="3" username="no-id" userID="" accessToken="token-300"/>` +
			`</MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	fc := fakeapi.NewCache()
	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	m.RefreshTokens(context.Background(), adminClient, "machine-id-123")

	if m.SharedCount() != 1 {
		t.Errorf("SharedCount = %d, want 1 (blanks filtered)", m.SharedCount())
	}
}

// --- InitialRefreshWithRetry ---

func testRefreshConfig(maxAttempts int, base, max time.Duration) RefreshConfig {
	return RefreshConfig{MaxAttempts: maxAttempts, BaseDelay: base, MaxDelay: max}
}

func TestInitialRefreshWithRetry_cached_users_short_circuit(t *testing.T) {
	// plex.tv always returns 500 — the refresh itself fails. Cached
	// tokens should still let the loop exit after a single attempt.
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	fc := fakeapi.NewCache()
	fc.SetUserTokens(map[string]string{"100": "cached-token"})

	m := NewManager(fc)
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	m.LoadFromCache()

	cfg := testRefreshConfig(5, 10*time.Millisecond, 20*time.Millisecond)
	start := time.Now()
	m.InitialRefreshWithRetry(context.Background(), adminClient, "mid", cfg)
	elapsed := time.Since(start)

	if attempts != 1 {
		t.Errorf("got %d plex.tv attempts, want 1 (cached users should short-circuit)", attempts)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("elapsed = %v, want <50ms (no retry wait expected)", elapsed)
	}
}

func TestInitialRefreshWithRetry_success_on_second_attempt(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer>` +
			`<SharedServer id="1" username="friend1" userID="100" accessToken="token-100"/>` +
			`</MediaContainer>`))
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	m := NewManager(fakeapi.NewCache())
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	cfg := testRefreshConfig(5, 5*time.Millisecond, 20*time.Millisecond)
	m.InitialRefreshWithRetry(context.Background(), adminClient, "mid", cfg)

	if attempts != 2 {
		t.Errorf("got %d plex.tv attempts, want 2 (retry after first 500)", attempts)
	}
	if m.SharedCount() != 1 {
		t.Errorf("SharedCount = %d, want 1 (second attempt populates)", m.SharedCount())
	}
}

func TestInitialRefreshWithRetry_gives_up_after_max_attempts(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	m := NewManager(fakeapi.NewCache())
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	cfg := testRefreshConfig(3, 5*time.Millisecond, 10*time.Millisecond)
	m.InitialRefreshWithRetry(context.Background(), adminClient, "mid", cfg)

	if attempts != 3 {
		t.Errorf("got %d plex.tv attempts, want 3 (exhaust max)", attempts)
	}
	if m.SharedCount() != 0 {
		t.Errorf("SharedCount = %d, want 0", m.SharedCount())
	}
}

func TestInitialRefreshWithRetry_context_cancellation(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapPlexTVClient(t, srv)

	parsed, _ := url.Parse("http://plex:32400")
	adminClient := plex.NewClientFromHTTP(parsed, "admin-token", &http.Client{})

	m := NewManager(fakeapi.NewCache())
	m.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")

	// Long delays so the test must rely on context cancellation to exit.
	cfg := testRefreshConfig(10, 5*time.Second, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	m.InitialRefreshWithRetry(ctx, adminClient, "mid", cfg)
	elapsed := time.Since(start)

	// The core invariant: the retry loop aborts promptly when the
	// context is cancelled, not waiting out the 5s backoff. Elapsed
	// well below the shortest backoff proves the cancel path works.
	if elapsed > time.Second {
		t.Errorf("elapsed = %v, want <1s (should abort on ctx cancel, not wait out backoff)", elapsed)
	}
	// We intentionally do NOT assert attempts >= 1. Under heavy
	// goroutine scheduling (e.g. `go test -race`) the cancel goroutine
	// can fire before the first HTTP round-trip lands. When that
	// happens the retry loop correctly exits at its ctx.Err() pre-check
	// without making any attempt, which is the desired behaviour. The
	// elapsed assertion above already proves cancellation aborts the
	// loop before the backoff sleep would have fired. `attempts` is
	// captured here so failure diagnostics can show it.
	_ = attempts
}

func TestDefaultRefreshConfig(t *testing.T) {
	// Pin the production tunables so an accidental change surfaces in CI.
	cfg := DefaultRefreshConfig()
	if cfg.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", cfg.MaxAttempts)
	}
	if cfg.BaseDelay != 5*time.Second {
		t.Errorf("BaseDelay = %v, want 5s", cfg.BaseDelay)
	}
	if cfg.MaxDelay != 60*time.Second {
		t.Errorf("MaxDelay = %v, want 60s", cfg.MaxDelay)
	}
}

func TestPeriodicRefreshInterval(t *testing.T) {
	if got := PeriodicRefreshInterval(); got != 12*time.Hour {
		t.Errorf("PeriodicRefreshInterval = %v, want 12h", got)
	}
}
