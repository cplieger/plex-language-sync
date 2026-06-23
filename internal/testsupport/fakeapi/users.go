package fakeapi

import (
	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// Users implements api.UserLookup for tests. The zero value returns
// nil/empty from all methods, matching the minimal scheduler fake.
type Users struct {
	Names     map[string]string
	AllResult []api.UserInfo
}

var _ api.UserLookup = (*Users)(nil)

// ClientForUser always returns nil; the fake does not manage per-user clients.
func (u *Users) ClientForUser(_ string, _ *plex.Client) *plex.Client { return nil }

// All returns the AllResult slice configured on the fake.
func (u *Users) All() []api.UserInfo { return u.AllResult }

// Name returns the display name for userID from the Names map, falls back to
// "user-<userID>" when AllResult is non-nil, or empty string otherwise.
func (u *Users) Name(userID string) string {
	if u.Names != nil {
		if n, ok := u.Names[userID]; ok {
			return n
		}
	}
	if u.AllResult != nil {
		return "user-" + userID
	}
	return ""
}
