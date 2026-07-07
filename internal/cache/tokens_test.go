package cache

import "testing"

func TestUserTokensReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetUserTokens(map[string]string{"1": "t1", "2": "t2"})

	got := c.UserTokens()
	got["1"] = "mutated"
	got["999"] = "added"

	internal := c.UserTokens()
	if internal["1"] != "t1" {
		t.Errorf("UserTokens returned non-defensive map: internal[1] = %q after external mutation",
			internal["1"])
	}
	if _, ok := internal["999"]; ok {
		t.Error("UserTokens returned non-defensive map: external insert leaked into cache")
	}
}

func TestSetUserTokensCopiesInput(t *testing.T) {
	t.Parallel()
	c := New()
	src := map[string]string{"1": "t1"}
	c.SetUserTokens(src)

	// Mutate the caller's map after set.
	src["1"] = "mutated-by-caller"
	src["999"] = "injected"

	got := c.UserTokens()
	if got["1"] != "t1" {
		t.Errorf("SetUserTokens did not copy input: got[1] = %q after caller mutation", got["1"])
	}
	if _, ok := got["999"]; ok {
		t.Error("SetUserTokens did not copy input: caller insert leaked into cache")
	}
}

func TestSetUserTokensReplacesWholesale(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetUserTokens(map[string]string{"a": "t-a", "b": "t-b"})

	// A second SetUserTokens replaces the map wholesale: a key absent from
	// the new map must be evicted, not merged. This eviction is what lets
	// users.RefreshTokens stop using a revoked user's token.
	c.SetUserTokens(map[string]string{"a": "t-a2"})

	got := c.UserTokens()
	if got["a"] != "t-a2" {
		t.Errorf("UserTokens()[a] = %q, want t-a2 (updated by wholesale replace)", got["a"])
	}
	if _, ok := got["b"]; ok {
		t.Error("UserTokens() retained key b after a wholesale replace that omitted it; want it evicted")
	}
}

func TestSetUserTokensNilClears(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetUserTokens(map[string]string{"a": "t-a", "b": "t-b"})

	c.SetUserTokens(nil)

	if got := c.UserTokens(); len(got) != 0 {
		t.Errorf("UserTokens() after SetUserTokens(nil) = %v (len %d), want empty", got, len(got))
	}
}
