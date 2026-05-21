// Package users owns the per-user state, token storage, and per-user
// Plex client cache. It is the home of the userManager subsystem that
// previously lived in main.go and the cross-module typed user-id.
//
// Inviolate contracts preserved (see refactor-agent-guide.md):
//
//   - The on-disk cache.json schema is untouched. The Manager reads and
//     writes tokens through the api.Cache interface (backed by
//     internal/cache), never by mutating cache.Data directly.
//   - WARN/ERROR slog keys for token refresh ("failed to refresh shared
//     user tokens", "shared user tokens refreshed") are byte-for-byte
//     identical to the pre-extraction log lines.
//   - Initial-refresh retry semantics (5 attempts, 5s base, 2× backoff,
//     60s cap, short-circuit on cached users, context-cancel aware) are
//     preserved; the tunables live on a RefreshConfig value so tests can
//     shrink them without reaching into package-level globals.
package users

import (
	"net/url"
	"sync"

	"plex-language-sync/internal/api"
	"plex-language-sync/internal/plex"
)

// Compile-time interface satisfaction assertion.
var _ api.UserLookup = (*Manager)(nil)

// ID is the typed user identifier (runtime-types-p2). Plex user IDs are
// numeric strings, but they are routinely treated as opaque keys — the
// typed wrapper keeps them from being conflated with other string keys
// (ratingKey, tokens, session keys) inside this package while still
// round-tripping through APIs that expect strings.
//
// The Manager's public methods accept plain strings (rather than ID)
// so *Manager naturally satisfies api.UserLookup without a wrapper;
// the typed ID remains available for internal map keys and for callers
// that want stricter typing at their own boundaries.
type ID string

// String returns the ID as a plain string for APIs that accept strings
// (HTTP query params, slog values, cache keys).
func (i ID) String() string { return string(i) }

// Info is the per-user record: the typed ID, display name, and Plex
// access token. Tokens are secret; callers must not log Token values.
type Info struct {
	ID    ID
	Name  string
	Token string
}

// Manager owns the shared-user map, the per-user HTTP client cache, and
// the admin user identity. All fields are guarded by mu; the manager is
// safe for concurrent use.
type Manager struct {
	baseURL *url.URL
	cache   api.Cache
	shared  map[ID]Info         // keyed by typed userID
	clients map[ID]*plex.Client // cached per-user clients
	admin   Info
	mu      sync.Mutex
	skipTLS bool
}

// NewManager returns a Manager with empty shared-user and client maps.
// The Init method (called by the composition root after the admin user
// is resolved) seeds admin identity and base URL.
func NewManager(c api.Cache) *Manager {
	return &Manager{
		cache:   c,
		shared:  make(map[ID]Info),
		clients: make(map[ID]*plex.Client),
	}
}

// Init seeds the manager with the admin user and base URL. Safe to call
// multiple times; existing shared-user state is preserved so a re-init
// (e.g., after a token refresh during startup) does not clobber in-flight
// data.
func (m *Manager) Init(admin *plex.User, baseURL *url.URL, skipTLS bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admin = Info{ID: ID(admin.ID), Name: admin.Name}
	m.baseURL = baseURL
	m.skipTLS = skipTLS
	if m.shared == nil {
		m.shared = make(map[ID]Info)
	}
	m.clients = make(map[ID]*plex.Client)
}

// LoadFromCache seeds the shared-user map from cached tokens. The cached
// entries use synthetic display names ("user-{id}") until a successful
// plex.tv refresh supplies the real username. Called at startup so the
// app can operate on per-user tokens when plex.tv is unreachable.
func (m *Manager) LoadFromCache() {
	tokensCopy := m.cache.UserTokens()

	m.mu.Lock()
	defer m.mu.Unlock()
	for uidStr, token := range tokensCopy {
		uid := ID(uidStr)
		if uid == m.admin.ID {
			continue
		}
		if _, exists := m.shared[uid]; !exists {
			m.shared[uid] = Info{ID: uid, Token: token, Name: "user-" + uidStr}
		}
	}
}

// ClientForUser returns a *plex.Client using the given user's token.
// Caches clients to avoid creating new HTTP connection pools on every
// call. Falls back to the admin client when the userID matches admin or
// is unknown (e.g., a historical session from a user who no longer
// shares the server).
//
// userID is accepted as a plain string so *Manager satisfies
// api.UserLookup; convert to ID internally for map keys.
func (m *Manager) ClientForUser(userID string, adminClient *plex.Client) *plex.Client {
	uid := ID(userID)
	m.mu.Lock()
	defer m.mu.Unlock()

	if uid == m.admin.ID {
		return adminClient
	}
	// Return cached client if token hasn't changed.
	if cached, ok := m.clients[uid]; ok {
		if info, exists := m.shared[uid]; exists && cached.Token() == info.Token {
			return cached
		}
	}
	if info, ok := m.shared[uid]; ok && info.Token != "" {
		c := plex.NewClientForUser(m.baseURL, info.Token, m.skipTLS)
		m.clients[uid] = c
		return c
	}
	// Unknown user — fall back to admin.
	return adminClient
}

// SharedCount returns the number of shared (non-admin) users currently
// known. Used by InitialRefreshWithRetry to detect whether a refresh
// attempt populated any users, independent of whether the plex.tv API
// call itself succeeded or silently returned an empty shared-servers
// list.
func (m *Manager) SharedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.shared)
}

// All returns the admin plus all shared users as api.UserInfo values.
// The returned admin entry has an empty Token field; callers that need
// an HTTP client for the admin user should use ClientForUser (which
// falls back to the admin client for the admin ID) rather than
// threading the admin token through this slice. Keeping the admin
// token out narrows the in-memory surface that holds it.
//
// The return type is api.UserInfo (not internal Info) so *Manager
// satisfies api.UserLookup and consumers (sync, scheduler) can depend
// on the api interface without pulling in internal/users.
func (m *Manager) All() []api.UserInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]api.UserInfo, 0, 1+len(m.shared))
	out = append(out, api.UserInfo{
		ID:   m.admin.ID.String(),
		Name: m.admin.Name,
	})
	for _, u := range m.shared {
		out = append(out, api.UserInfo{
			ID:    u.ID.String(),
			Name:  u.Name,
			Token: u.Token,
		})
	}
	return out
}

// Name returns the display name for a userID. Unknown users get an
// "unknown-{id}" placeholder so log lines remain parseable.
func (m *Manager) Name(userID string) string {
	uid := ID(userID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if uid == m.admin.ID {
		return m.admin.Name
	}
	if info, ok := m.shared[uid]; ok {
		return info.Name
	}
	return "unknown-" + userID
}

// Admin returns the admin UserInfo. Primarily for tests that need to
// assert the manager was initialized with the expected admin identity.
func (m *Manager) Admin() api.UserInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return api.UserInfo{ID: m.admin.ID.String(), Name: m.admin.Name}
}
