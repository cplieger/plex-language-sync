package tracksync

import "github.com/cplieger/plex-language-sync/internal/users"

// fakeUsers is the userLookup fake. It lives in this package's test files
// rather than in internal/testsupport because this is its only consumer, and a
// shared fixture package could not hold it anyway: internal/users' own tests
// import the shared fakes, so a users fake living there would close an import
// cycle back through internal/users.
//
// The zero value returns nil/empty from both methods.
type fakeUsers struct {
	Names     map[string]string
	AllResult []users.Account
}

// All returns the AllResult slice configured on the fake.
func (u *fakeUsers) All() []users.Account { return u.AllResult }

// Name returns the display name for userID from the Names map, falls back to
// "user-<userID>" when AllResult is non-nil, or empty string otherwise.
func (u *fakeUsers) Name(userID string) string {
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

var _ userLookup = (*fakeUsers)(nil)
