package fakeapi

import (
	"sync"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/api"
)

// RunCacheContract exercises the api.Cache contract against any
// implementation. Both cache.Cache and fakeapi.Cache must pass.
func RunCacheContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("SetGet_roundtrip", func(t *testing.T) {
		c.LearnLanguageProfile("u1", "eng", "fra")
		got, ok := c.SubtitleLangForAudio("u1", "eng")
		if !ok || got != "fra" {
			t.Errorf("SubtitleLangForAudio = (%q, %v), want (fra, true)", got, ok)
		}
	})

	t.Run("processed_roundtrip", func(t *testing.T) {
		if c.WasRecentlyProcessed("contract-key") {
			t.Error("fresh key should not be recently processed")
		}
		c.MarkProcessed("contract-key")
		if !c.WasRecentlyProcessed("contract-key") {
			t.Error("after MarkProcessed, WasRecentlyProcessed should be true")
		}
	})

	t.Run("tokens_roundtrip", func(t *testing.T) {
		tokens := map[string]string{"a": "1", "b": "2"}
		c.SetUserTokens(tokens)
		got := c.UserTokens()
		if got["a"] != "1" || got["b"] != "2" {
			t.Errorf("UserTokens = %v, want map[a:1 b:2]", got)
		}
	})

	t.Run("concurrent_writers", func(_ *testing.T) {
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				key := "conc-" + string(rune('A'+i%26))
				c.MarkProcessed(key)
				c.WasRecentlyProcessed(key)
			})
		}
		wg.Wait()
	})
}
