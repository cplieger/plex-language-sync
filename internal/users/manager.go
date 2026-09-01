// Package users owns the per-user state, token storage, and per-user
// Plex client cache.
//
// Inviolate contracts:
//
//   - The on-disk cache schema is untouched; the Manager reads and writes
//     tokens through the tokenStore interface, never by mutating cache.Data
//     directly.
//   - WARN/ERROR slog keys for token refresh ("failed to refresh shared
//     user tokens", "shared user tokens refreshed") are byte-for-byte
//     identical across versions; Loki alerts grep them.
//   - Initial-refresh retry semantics (5 attempts, 5s base, 2x backoff,
//     60s cap, short-circuit on cached users, context-cancel aware) live on
//     a RefreshConfig value so tests can shrink them.
package users

import (
	"sync"

	"github.com/cplieger/plex-language-sync/internal/plex"
)

// ID is the typed user identifier. Plex user IDs are numeric strings but are
// routinely treated as opaque keys; the typed wrapper keeps them from being
// conflated with other string keys (ratingKey, tokens, session keys) inside
// this package while still round-tripping through APIs that expect strings.
//
// Public methods accept plain strings rather than ID so a consumer can
// declare its own lookup interface without importing this package.
type ID string

// String returns the ID as a plain string for APIs that accept strings.
func (i ID) String() string { return string(i) }

// record is the manager's own per-user entry: the typed ID, display name, and
// Plex access token. Unexported because of that token — a struct that can
// carry a secret must not cross a package boundary. Callers get Account
// instead.
type record struct {
	ID   ID
	Name string
	// Token is plex.Token, not a string: it is compared against
	// (*Client).Token() and handed to ForToken.
	Token plex.Token
}

// Account is a user's identity as every other package sees it: an ID and a
// display name, with no token field, so leaking a token through this struct
// is structurally impossible. Anything needing to act as a user goes through
// ClientForUser, which looks the token up internally and never returns it.
type Account struct {
	ID   string
	Name string
}

// tokenStore is the persistence this package needs: read and write the
// shared-user token map. Declared here so a test fake implements two
// methods instead of the cache's full surface.
type tokenStore interface {
	UserTokens() map[string]string
	SetUserTokens(tokens map[string]string)
}

// Manager owns the shared-user map, the per-user HTTP client cache, and
// the admin user identity. All fields are guarded by mu; the manager is
// safe for concurrent use.
type Manager struct {
	cache   tokenStore
	shared  map[ID]record       // keyed by typed userID
	clients map[ID]*plex.Client // cached per-user clients
	admin   record
	mu      sync.Mutex
}

// NewManager returns a Manager with empty shared-user and client maps.
// The Init method (called by the composition root after the admin user
// is resolved) seeds the admin identity.
func NewManager(c tokenStore) *Manager {
	return &Manager{
		cache:   c,
		shared:  make(map[ID]record),
		clients: make(map[ID]*plex.Client),
	}
}

// Init seeds the manager with the admin user. Safe to call multiple
// times; existing shared-user state is preserved so a re-init does not
// clobber in-flight data.
func (m *Manager) Init(admin *plex.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admin = record{ID: ID(admin.ID), Name: admin.Name}
	if m.shared == nil {
		m.shared = make(map[ID]record)
	}
	m.clients = make(map[ID]*plex.Client)
}

// LoadFromCache seeds the shared-user map from cached tokens. Cached entries
// use synthetic display names ("user-{id}") until a successful plex.tv
// refresh supplies the real username. Called at startup so the app can
// operate on per-user tokens when plex.tv is unreachable.
func (m *Manager) LoadFromCache() {
	tokensCopy := m.cache.UserTokens()

	m.mu.Lock()
	defer m.mu.Unlock()
	for uidStr, token := range tokensCopy {
		uid := ID(uidStr)
		// Mirrors the s.AccessToken == "" guard in RefreshTokens so a
		// corrupted-cache phantom user never enters m.shared.
		if uid == m.admin.ID || token == "" {
			continue
		}
		if _, exists := m.shared[uid]; !exists {
			m.shared[uid] = record{ID: uid, Token: plex.Token(token), Name: "user-" + uidStr}
		}
	}
}

// ClientForUser returns a *plex.Client scoped to the given user's token,
// derived from adminClient via ForToken (a pure same-server derivation, no
// disk I/O, cannot fail). Derived clients are cached per user until the
// token rotates.
//
// Returns the admin client when userID matches admin. Returns nil (fail
// closed) when no per-user identity is available — the user is
// unknown/departed, or holds an empty token. The caller must skip the
// operation rather than write under the admin token: a per-user stream PUT
// is per-user-scoped on the server, so writing under the admin token
// corrupts the admin's own selection and still drops the intended user's
// preference.
//
// Silent on the nil path: callers own the "skipping" log and already
// de-spam it (the scheduler history path guards one WARN per unknown user
// per pass; handlePlayEvent WARNs on the naturally rate-limited play path).
//
// userID is a plain string so consumers need not import this package for
// the ID type.
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
	info, ok := m.shared[uid]
	if !ok || info.Token == "" {
		return nil
	}
	c := adminClient.ForToken(info.Token)
	m.clients[uid] = c
	return c
}

// SharedCount returns the number of shared (non-admin) users currently
// known. Used by InitialRefreshWithRetry to detect whether a refresh
// populated any users, independent of whether the plex.tv call succeeded.
func (m *Manager) SharedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.shared)
}

// All returns the admin plus all shared users as Account values. Account has
// no token field, so this slice cannot carry one.
func (m *Manager) All() []Account {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Account, 0, 1+len(m.shared))
	out = append(out, Account{
		ID:   m.admin.ID.String(),
		Name: m.admin.Name,
	})
	for _, u := range m.shared {
		out = append(out, Account{
			ID:   u.ID.String(),
			Name: u.Name,
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
