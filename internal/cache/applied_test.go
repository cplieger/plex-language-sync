package cache

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

func TestAppliedSurvivesSaveLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := streams.Applied{
		Basis:      streams.Basis{From: "bob", Version: 1700000000, Source: streams.SourceHousehold},
		At:         1700000100,
		PartID:     5001,
		AudioID:    9001,
		SubtitleID: 9002,
	}
	orig := New()
	orig.RecordApplied("alice", "1001", want)
	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded := New()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, ok := loaded.AppliedFor("alice", "1001")
	if !ok {
		t.Fatal("AppliedFor(alice, 1001) after save and load = ok=false, want the recorded entry")
	}
	if got != want {
		t.Errorf("AppliedFor(alice, 1001) = %+v, want %+v", got, want)
	}
	if _, ok := loaded.AppliedFor("bob", "1001"); ok {
		t.Error("AppliedFor(bob, 1001) = ok=true, want false: records are per user")
	}
}

func TestEvictOldestApplied(t *testing.T) {
	t.Parallel()
	at := func(ts int64) streams.Applied { return streams.Applied{At: ts} }
	tests := []struct {
		in    map[string]map[string]streams.Applied
		want  map[string]map[string]streams.Applied
		name  string
		limit int
	}{
		{
			name:  "under_limit_untouched",
			in:    map[string]map[string]streams.Applied{"alice": {"1001": at(1), "1002": at(2)}},
			limit: 2,
			want:  map[string]map[string]streams.Applied{"alice": {"1001": at(1), "1002": at(2)}},
		},
		{
			name: "over_limit_drops_oldest_across_users",
			in: map[string]map[string]streams.Applied{
				"alice": {"1001": at(10), "1002": at(30)},
				"bob":   {"1001": at(20), "1003": at(5)},
			},
			limit: 2,
			want: map[string]map[string]streams.Applied{
				"alice": {"1002": at(30)},
				"bob":   {"1001": at(20)},
			},
		},
		{
			name: "user_left_empty_is_removed",
			in: map[string]map[string]streams.Applied{
				"alice": {"1001": at(1)},
				"bob":   {"1001": at(2)},
			},
			limit: 1,
			want:  map[string]map[string]streams.Applied{"bob": {"1001": at(2)}},
		},
		{
			name: "ties_go_by_user_then_key",
			in: map[string]map[string]streams.Applied{
				"alice": {"1001": at(7), "1002": at(7)},
				"bob":   {"1001": at(7)},
			},
			limit: 1,
			want:  map[string]map[string]streams.Applied{"bob": {"1001": at(7)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			evictOldestApplied(tt.in, tt.limit)
			if !maps.EqualFunc(tt.in, tt.want, maps.Equal) {
				t.Errorf("after evictOldestApplied(limit %d) = %v, want %v", tt.limit, tt.in, tt.want)
			}
		})
	}
}

// TestAppliedCapFitsMaxCacheSize fills the applied section past its cap at the
// widest realistic encoding and checks Save's own encoder evicts it to the cap
// and stays under the size the next load accepts.
func TestAppliedCapFitsMaxCacheSize(t *testing.T) {
	t.Parallel()
	const users = 15
	c := New()
	total := maxAppliedEntries + 1000
	for i := range total {
		user := strconv.Itoa(1999999900 + i%users)
		a := streams.Applied{
			Basis:      streams.Basis{From: "1999999999", Version: 1999999999, Source: streams.SourceHousehold},
			At:         int64(1000000000 + i),
			PartID:     1999999999,
			AudioID:    1999999999,
			SubtitleID: 1999999999,
		}
		if i%4 == 0 {
			a.Basis.Version = -9223372036854775807
		}
		c.RecordApplied(user, strconv.Itoa(1000000000+i), a)
	}
	now := time.Now().Unix()
	for i := range maxProcessedEntries {
		c.data.ProcessedEpisodes[KeyPrefixScheduler+strconv.Itoa(1000000000+i)] = now
	}

	_, _, state, err := c.encodeAllForSave()
	if err != nil {
		t.Fatalf("encodeAllForSave() error = %v", err)
	}
	if len(state) >= maxCacheSize {
		t.Errorf("state.json at the applied cap = %d bytes, want under maxCacheSize (%d)", len(state), maxCacheSize)
	}
	kept := 0
	for _, perUser := range c.data.Applied {
		kept += len(perUser)
	}
	if kept != maxAppliedEntries {
		t.Errorf("applied entries after encodeAllForSave = %d, want the cap %d", kept, maxAppliedEntries)
	}
}
