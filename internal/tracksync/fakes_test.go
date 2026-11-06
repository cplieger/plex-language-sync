package tracksync

import (
	"context"

	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/users"
	"github.com/cplieger/plexapi/v3"
)

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

// cancellingClient cancels its context on the first write of kind on
// ("audio" or "subtitle"). With fail set, that write then fails with the
// context's error, as a request the cancellation ended does; otherwise the
// fake accepts it.
type cancellingClient struct {
	*fakeapi.Plex
	cancel context.CancelFunc
	on     string
	fail   bool
}

func (c *cancellingClient) SetAudioStream(ctx context.Context, sel plexapi.StreamSelection) error {
	if err := c.cancelOn(ctx, "audio"); err != nil {
		return err
	}
	return c.Plex.SetAudioStream(ctx, sel)
}

func (c *cancellingClient) SetSubtitleStream(ctx context.Context, sel plexapi.StreamSelection) error {
	if err := c.cancelOn(ctx, "subtitle"); err != nil {
		return err
	}
	return c.Plex.SetSubtitleStream(ctx, sel)
}

func (c *cancellingClient) cancelOn(ctx context.Context, kind string) error {
	if kind != c.on {
		return nil
	}
	c.cancel()
	if c.fail {
		return ctx.Err()
	}
	return nil
}

var _ PlexReadWriter = (*cancellingClient)(nil)
