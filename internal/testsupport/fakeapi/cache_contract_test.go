package fakeapi_test

import (
	"testing"

	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// TestCacheContract runs the REAL cache's contract against this fake. It is the
// test that stops the fake drifting: every other test in the repo trusts the
// fake to behave like the store it stands in for.
func TestCacheContract(t *testing.T) {
	t.Parallel()
	cache.RunContract(t, fakeapi.NewCache())
}
