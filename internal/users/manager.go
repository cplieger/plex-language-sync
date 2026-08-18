// Package users owns the per-user state, token storage, and per-user
// Plex client cache. It is the home of the userManager subsystem that
// previously lived in main.go and the cross-module typed user-id.
//
// Inviolate contracts preserved (see refactor-agent-guide.md):
//
//   - The on-disk cache schema is untouched. The Manager reads and
//     writes tokens through the tokenStore interface (backed by
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
	"sync"

	"github.com/cplieger/plex-language-sync/internal/plex"
)

// ID is the typed user identifier (runtime-types-p2). Plex user IDs are
// numeric strings, but they are routinely treated as opaque keys — the
// typed wrapper keeps them from being conflated with other string keys
// (ratingKey, tokens, session keys) inside this package while still
// round-tripping through APIs that expect strings.
//
// The Manager's public methods accept plain strings (rather than ID) so a
// consumer can declare its own lookup interface without importing this
// package for the ID type; the typed ID remains available for internal map
// keys and for callers that want stricter typing at their own boundaries.
type ID string

// String returns the ID as a plain string for APIs that accept strings
// (HTTP query params, slog values, cache keys).
func (i ID) String() string { return string(i) }

// record is the manager's own per-user entry: the typed ID, display name, and
// Plex access token. Unexported precisely BECAUSE of that token — a struct that
// can carry a secret must not be handed across a package boundary, and nothing
// outside this package has ever needed one. Callers get Account instead.
type record struct {
	ID    ID
	Name  string
	Token string
}

// Account is a user's identity as every other package sees it: an ID and a
// display name, and deliberately NO token field. With no field to populate,
// leaking a token through this struct is structurally impossible rather than
// merely discouraged. Anything needing to act as a user goes through
// ClientForUser, which looks the token up internally and never returns it.
type Account struct {
	ID   string
	Name string
}

// tokenStore is the persistence this package needs: read and write the
// shared-user token map. Two methods, against the eleven of the cache type it
// is satisfied by — the other nine belong to the profile ledger and the
// deep-scan watermark, which are no business of user management. Declared here
// so a test fake implements two methods instead of eleven.
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
// times; existing shared-user state is preserved so a re-init (e.g.,
// after a token refresh during startup) does not clobber in-flight data.
func (m *Manager) Init(admin *plex.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.admin = record{ID: ID(admin.ID), Name: admin.Name}
	if m.shared == nil {
		m.shared = make(map[ID]record)
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
		// Skip empty tokens: mirror the s.AccessToken == "" guard in
		// RefreshTokens so a corrupted-cache phantom user never enters
		// m.shared and never triggers an admin-fallback write.
		if uid == m.admin.ID || token == "" {
			continue
		}
		if _, exists := m.shared[uid]; !exists {
			m.shared[uid] = record{ID: uid, Token: token, Name: "user-" + uidStr}
		}
	}
}

// ClientForUser returns a *plex.Client scoped to the given user's token,
// derived from adminClient via ForToken — a pure same-server derivation
// that shares the admin client's transport, trust settings, and
// connection pool (no disk I/O, cannot fail). Derived clients are cached
// per user until the token rotates.
//
// Returns the admin client only when the userID matches admin. Returns
// nil (fail CLOSED) when no per-user identity is available: the user is
// unknown/departed (absent from the shared-user map, or holding an empty
// token). The caller must skip the operation rather than write under the
// admin token — a per-user stream PUT is per-user-scoped on the server,
// so executing it under the admin token corrupts the ADMIN's own stream
// selection and still does not apply the intended user's preference.
// Reachable in steady state (a play event for a user no longer sharing)
// and via the fan-out race where a concurrent RefreshTokens prunes the
// user between the users.All() snapshot and this call; in the race the
// user is re-processed on the next pass once the refresh completes.
//
// Intentionally SILENT on the nil path: the callers own the "skipping"
// log and already de-spam it (the scheduler history path guards a single
// WARN per unknown user per pass via its unknownUsers set;
// handlePlayEvent WARNs on the naturally rate-limited play path). A
// per-call WARN here would re-introduce the exact Loki spam that de-spam
// was built to prevent.
//
// userID is accepted as a plain string so consumers need not import this
// package for the ID type; convert to ID internally for map keys.
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
// attempt populated any users, independent of whether the plex.tv API
// call itself succeeded or silently returned an empty shared-servers
// list.
func (m *Manager) SharedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.shared)
}

// All returns the admin plus all shared users as Account values. Account has
// no token field, so this slice cannot carry one. Callers that need an HTTP
// client for any user must use ClientForUser (which falls back to the admin
// client for the admin ID and looks up a shared user's token internally).
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
