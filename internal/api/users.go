package api

import "plex-language-sync/internal/plex"

// UserInfo is the minimal user record consumers pass across the api
// spine. Mirrors internal/users.Info but uses primitive string IDs so
// the api package stays at the bottom of the import graph — importing
// internal/users here would introduce a cycle because users depends on
// api.Cache.
type UserInfo struct {
	ID    string
	Name  string
	Token string
}

// UserLookup resolves user IDs to per-user Plex clients, display
// names, and the full list of known users. The concrete implementation
// lives in internal/users.
//
// Method signatures use plain strings for user IDs (rather than the
// typed users.ID) to keep this package free of a reverse dependency on
// internal/users. Callers that want typed IDs should go directly
// through internal/users at the consumer site; this interface is the
// shared wire other packages (sync, scheduler) can depend on without
// pulling the full user-manager surface into their import graph.
type UserLookup interface {
	ClientForUser(userID string, adminClient *plex.Client) *plex.Client
	All() []UserInfo
	Name(userID string) string
}
