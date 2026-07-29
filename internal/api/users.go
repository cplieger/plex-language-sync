package api

// UserInfo is the minimal user record consumers pass across the api
// spine. Mirrors internal/users.Info but uses primitive string IDs so
// the api package stays at the bottom of the import graph — importing
// internal/users here would introduce a cycle because users depends on
// api.Cache.
//
// Deliberately carries no token field: a per-user Plex client comes from
// api.UserClientFunc (backed by users.Manager.ClientForUser, which looks
// the token up internally), so nothing across the spine needs one. With
// no field to populate, leaking a token through this struct is
// structurally impossible rather than merely discouraged.
type UserInfo struct {
	ID   string
	Name string
}

// UserLookup resolves user IDs to display names and the full list of
// known users. The concrete implementation lives in internal/users.
// (Per-user Plex clients are obtained through api.UserClientFunc, not
// this interface.)
//
// Method signatures use plain strings for user IDs (rather than the
// typed users.ID) to keep this package free of a reverse dependency on
// internal/users. Callers that want typed IDs should go directly
// through internal/users at the consumer site; this interface is the
// shared wire other packages (sync, scheduler) can depend on without
// pulling the full user-manager surface into their import graph.
type UserLookup interface {
	All() []UserInfo
	Name(userID string) string
}
