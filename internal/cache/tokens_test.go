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
