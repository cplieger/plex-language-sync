package fakeapi

import "testing"

func TestCacheContract(t *testing.T) {
	t.Parallel()
	RunCacheContract(t, NewCache())
}
