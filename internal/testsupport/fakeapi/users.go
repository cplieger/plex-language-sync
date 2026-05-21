package fakeapi

import (
	"plex-language-sync/internal/api"
	"plex-language-sync/internal/plex"
)

// Users implements api.UserLookup for tests. The zero value returns
// nil/empty from all methods, matching the minimal scheduler fake.
type Users struct {
	Names     map[string]string
	AllResult []api.UserInfo
}

var _ api.UserLookup = (*Users)(nil)

func (u *Users) ClientForUser(_ string, _ *plex.Client) *plex.Client { return nil }

func (u *Users) All() []api.UserInfo { return u.AllResult }

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
