package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"pgregory.net/rapid"
)

// --- Tests: LanguageProfile per-user ---

func TestCacheLanguageProfilePerUser(t *testing.T) {
	c := New()

	// User 1 prefers English subs for Japanese audio.
	c.LearnLanguageProfile("1", "jpn", "eng")
	// User 2 prefers no subs for Japanese audio.
	c.LearnLanguageProfile("2", "jpn", "")

	lang, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok || lang != "eng" {
		t.Errorf("user 1 jpn: got %q, %v; want eng, true", lang, ok)
	}

	lang, ok = c.SubtitleLangForAudio("2", "jpn")
	if !ok || lang != "" {
		t.Errorf("user 2 jpn: got %q, %v; want empty, true", lang, ok)
	}

	// Unknown user returns false.
	_, ok = c.SubtitleLangForAudio("999", "jpn")
	if ok {
		t.Error("expected false for unknown user")
	}
}

// --- Tests: WasRecentlyProcessed ---

func TestCacheWasRecentlyProcessed(t *testing.T) {
	c := New()

	if c.WasRecentlyProcessed("ep1") {
		t.Error("expected false for unknown key")
	}

	c.MarkProcessed("ep1")
	if !c.WasRecentlyProcessed("ep1") {
		t.Error("expected true after marking")
	}
}

// --- Tests: pruneOldEntries (exercised via SaveTo/MarkProcessed + time seed) ---

func TestCachePruneOldEntries(t *testing.T) {
	var c Cache
	c.data.ProcessedEpisodes = map[string]int64{
		"old": time.Now().Add(-48 * time.Hour).Unix(),
		"new": time.Now().Unix(),
	}
	c.pruneOldEntriesLocked()
	if _, ok := c.data.ProcessedEpisodes["old"]; ok {
		t.Error("old entry should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["new"]; !ok {
		t.Error("new entry should be kept")
	}
}

// --- Tests: LearnLanguageProfile ignores empty audio ---

func TestCacheLearnLanguageProfileIgnoresEmptyAudio(t *testing.T) {
	c := New()
	c.LearnLanguageProfile("1", "", "eng")
	if len(c.data.LanguageProfiles) != 0 {
		t.Error("should not learn profile with empty audio lang")
	}
}

// --- Tests: MarkProcessed auto-prune at >10000 ---

func TestCacheMarkProcessedAutoprune(t *testing.T) {
	c := New()
	// Fill with >10000 old entries to trigger inline prune.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 10001 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.MarkProcessed("fresh")
	// After prune, old entries should be gone.
	if len(c.data.ProcessedEpisodes) > 2 {
		t.Errorf("expected pruned map, got %d entries", len(c.data.ProcessedEpisodes))
	}
}

// --- Tests: MarkProcessed boundary at exactly 10000 ---

func TestCacheMarkProcessedBoundary10000(t *testing.T) {
	c := New()
	// Fill with exactly 9999 old entries. After inserting "fresh", total
	// = 10000. The threshold is > 10000 (not >=), so prune should NOT
	// fire at exactly 10000.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 9999 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.MarkProcessed("fresh")
	// 9999 old + 1 fresh = 10000 entries. 10000 > 10000 is false → no prune.
	if len(c.data.ProcessedEpisodes) != 10000 {
		t.Errorf("MarkProcessed at exactly 10000 entries should NOT prune, got %d entries",
			len(c.data.ProcessedEpisodes))
	}
}

// --- Tests: SubtitleLangForAudio edge cases ---

func TestCacheGetSubtitleLangForAudioNilProfiles(t *testing.T) {
	var c Cache
	// Don't initialize LanguageProfiles — test nil map path.
	lang, ok := c.SubtitleLangForAudio("1", "eng")
	if ok || lang != "" {
		t.Errorf("expected empty/false for nil profiles, got %q, %v", lang, ok)
	}
}

// --- Tests: LearnLanguageProfile idempotent ---

func TestCacheLearnLanguageProfileIdempotent(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	c.LearnLanguageProfile("1", "jpn", "eng") // same value — should not log again

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("expected eng, got %q", lang)
	}
}

// --- Tests: MarkProcessed nil map initialization ---

func TestCacheMarkProcessedNilMap(t *testing.T) {
	var c Cache
	// Don't initialize ProcessedEpisodes — test nil map path.
	c.MarkProcessed("test-key")
	if !c.WasRecentlyProcessed("test-key") {
		t.Error("expected true after MarkProcessed on nil map")
	}
}

// --- Tests: LearnLanguageProfile update existing ---

func TestCacheLearnLanguageProfileUpdate(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	if c.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Fatal("initial profile not set")
	}

	c.LearnLanguageProfile("1", "jpn", "fre")
	if c.data.LanguageProfiles["1"]["jpn"] != "fre" {
		t.Errorf("profile should update to fre, got %q", c.data.LanguageProfiles["1"]["jpn"])
	}
}

// --- Tests: load/save round-trip (JSON layer) ---

func TestCacheLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Build cache data via the public surface and helper seeding.
	original := &Cache{}
	original.data.ProcessedEpisodes = map[string]int64{
		"play:1:100": time.Now().Unix(),
		"play:2:200": time.Now().Unix(),
	}
	original.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng", "eng": ""},
		"2": {"kor": "eng"},
	}
	original.data.UserTokens = map[string]string{
		"2": "friend-token",
		"3": "other-token",
	}
	original.data.LastSchedulerRun = time.Now().Unix()

	// Write via JSON (simulating save to custom path).
	data, err := json.MarshalIndent(&original.data, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Load into a new cache by reading the file directly.
	loaded := New()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &loaded.data); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify round-trip.
	if len(loaded.data.ProcessedEpisodes) != 2 {
		t.Errorf("ProcessedEpisodes: got %d, want 2", len(loaded.data.ProcessedEpisodes))
	}
	if loaded.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng",
			loaded.data.LanguageProfiles["1"]["jpn"])
	}
	if loaded.data.LanguageProfiles["1"]["eng"] != "" {
		t.Errorf("LanguageProfiles[1][eng] = %q, want empty",
			loaded.data.LanguageProfiles["1"]["eng"])
	}
	if loaded.data.UserTokens["2"] != "friend-token" {
		t.Errorf("UserTokens[2] = %q, want friend-token", loaded.data.UserTokens["2"])
	}
	if loaded.data.LastSchedulerRun != original.data.LastSchedulerRun {
		t.Errorf("LastSchedulerRun = %d, want %d",
			loaded.data.LastSchedulerRun, original.data.LastSchedulerRun)
	}
}

func TestCacheLoadFromNonExistentFileIsNil(t *testing.T) {
	dir := t.TempDir()
	var c Cache
	// Missing file on a real temp dir — LoadFrom returns nil and
	// initializes the maps.
	err := c.LoadFrom(filepath.Join(dir, "never-created.json"))
	if err != nil {
		t.Errorf("LoadFrom() on non-existent file = %v, want nil", err)
	}
	if c.data.ProcessedEpisodes == nil {
		t.Error("ProcessedEpisodes should be initialized even on missing file")
	}
	if c.data.LanguageProfiles == nil {
		t.Error("LanguageProfiles should be initialized even on missing file")
	}
	if c.data.UserTokens == nil {
		t.Error("UserTokens should be initialized even on missing file")
	}
}

// --- PBT: JSON round-trip preserves LastSchedulerRun + map lengths ---

func TestCacheDataJSONRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nEntries := rapid.IntRange(0, 5).Draw(t, "n_entries")
		processed := make(map[string]int64, nEntries)
		for i := range nEntries {
			key := rapid.StringMatching(`[a-z:0-9]{1,20}`).Draw(t, fmt.Sprintf("key_%d", i))
			processed[key] = int64(rapid.IntRange(0, 2000000000).Draw(t, fmt.Sprintf("ts_%d", i)))
		}

		original := Data{
			ProcessedEpisodes: processed,
			LanguageProfiles:  make(map[string]map[string]string),
			UserTokens:        make(map[string]string),
			LastSchedulerRun:  int64(rapid.IntRange(0, 2000000000).Draw(t, "last_run")),
		}

		data, err := json.Marshal(&original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		var decoded Data
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if len(decoded.ProcessedEpisodes) != len(original.ProcessedEpisodes) {
			t.Errorf("ProcessedEpisodes length: got %d, want %d",
				len(decoded.ProcessedEpisodes), len(original.ProcessedEpisodes))
		}
		if decoded.LastSchedulerRun != original.LastSchedulerRun {
			t.Errorf("LastSchedulerRun: got %d, want %d",
				decoded.LastSchedulerRun, original.LastSchedulerRun)
		}
	})
}

// --- Tests: LearnLanguageProfile multiple languages ---

func TestCacheLearnLanguageProfileMultipleLanguages(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	c.LearnLanguageProfile("1", "kor", "eng")
	c.LearnLanguageProfile("1", "eng", "")

	if lang, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || lang != "eng" {
		t.Errorf("jpn profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.SubtitleLangForAudio("1", "kor"); !ok || lang != "eng" {
		t.Errorf("kor profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.SubtitleLangForAudio("1", "eng"); !ok || lang != "" {
		t.Errorf("eng profile: got %q, %v (want empty string, true)", lang, ok)
	}
	if _, ok := c.SubtitleLangForAudio("1", "fre"); ok {
		t.Error("fre profile should not exist")
	}
}

func TestCacheLearnLanguageProfileNilMaps(t *testing.T) {
	t.Parallel()
	var c Cache
	// Don't initialize LanguageProfiles — test nil map initialization path.
	c.LearnLanguageProfile("1", "jpn", "eng")

	lang, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("expected profile to exist after learn")
	}
	if lang != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) = %q, want eng", lang)
	}
}

func TestCacheLearnLanguageProfileNoChange(t *testing.T) {
	t.Parallel()
	c := New()
	c.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng"},
	}
	// Call with same value — should be a no-op (no log, no change).
	c.LearnLanguageProfile("1", "jpn", "eng")

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng", lang)
	}
}

// --- Boundary tests to kill lived mutants ---

func TestCacheWasRecentlyProcessedBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	// Entry exactly at the 5-minute boundary should NOT be recent.
	c.data.ProcessedEpisodes["old"] = time.Now().Add(-5 * time.Minute).Unix()
	if c.WasRecentlyProcessed("old") {
		t.Error("WasRecentlyProcessed(old) = true, want false for entry exactly 5 min ago")
	}

	// Entry 4m59s ago should still be recent.
	c.data.ProcessedEpisodes["recent"] = time.Now().Add(-4*time.Minute - 59*time.Second).Unix()
	if !c.WasRecentlyProcessed("recent") {
		t.Error("WasRecentlyProcessed(recent) = false, want true for entry 4m59s ago")
	}

	// Entry just now should be recent.
	c.data.ProcessedEpisodes["now"] = time.Now().Unix()
	if !c.WasRecentlyProcessed("now") {
		t.Error("WasRecentlyProcessed(now) = false, want true for entry just now")
	}

	// Entry 6 minutes ago should not be recent.
	c.data.ProcessedEpisodes["stale"] = time.Now().Add(-6 * time.Minute).Unix()
	if c.WasRecentlyProcessed("stale") {
		t.Error("WasRecentlyProcessed(stale) = true, want false for entry 6 min ago")
	}
}

func TestCacheMarkProcessedPruneBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	// Fill exactly 10000 entries — should NOT trigger prune.
	for i := range 10000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("key-%d", i)] = time.Now().Unix()
	}
	c.MarkProcessed("trigger")
	// After adding one more (10001 total), prune should have run.
	// Since all entries are recent, none should be pruned.
	if len(c.data.ProcessedEpisodes) != 10001 {
		t.Errorf("after MarkProcessed with 10001 entries, got %d entries, want 10001",
			len(c.data.ProcessedEpisodes))
	}

	// Now add old entries to make prune effective.
	oldTS := time.Now().Add(-25 * time.Hour).Unix()
	for i := range 5000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("old-%d", i)] = oldTS
	}
	// Total is now 15001. MarkProcessed triggers prune (>10000).
	c.MarkProcessed("trigger2")
	// Old entries should be pruned.
	if len(c.data.ProcessedEpisodes) > 10002 {
		t.Errorf("after prune, got %d entries, want ≤10002 (old entries removed)",
			len(c.data.ProcessedEpisodes))
	}
}

func TestCachePruneOldEntriesBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	now := time.Now()
	// Entry exactly 24h ago — should NOT be pruned (cutoff is -24h, ts <
	// cutoff means strictly older).
	c.data.ProcessedEpisodes["exact-24h"] = now.Add(-24 * time.Hour).Unix()
	// Entry 23h59m ago — should NOT be pruned.
	c.data.ProcessedEpisodes["23h59m"] = now.Add(-23*time.Hour - 59*time.Minute).Unix()
	// Entry 25h ago — should be pruned.
	c.data.ProcessedEpisodes["25h"] = now.Add(-25 * time.Hour).Unix()
	// Entry just now — should NOT be pruned.
	c.data.ProcessedEpisodes["now"] = now.Unix()

	c.pruneOldEntriesLocked()

	if _, ok := c.data.ProcessedEpisodes["exact-24h"]; !ok {
		t.Error("entry at exactly 24h should NOT be pruned (boundary: ts == cutoff)")
	}
	if _, ok := c.data.ProcessedEpisodes["23h59m"]; !ok {
		t.Error("entry at 23h59m should NOT be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["25h"]; ok {
		t.Error("entry at 25h should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["now"]; !ok {
		t.Error("entry at now should NOT be pruned")
	}
}

// --- SaveTo / LoadFrom (u1c1-001 + PLEX-LS-SEC-03) ---

func TestCacheSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	orig := New()
	orig.data.ProcessedEpisodes = map[string]int64{
		"play:1:100": time.Now().Unix(),
	}
	orig.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng"},
	}
	orig.data.UserTokens = map[string]string{"2": "t2"}
	orig.data.LastSchedulerRun = 1700000000

	if err := orig.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}
	loaded := &Cache{}
	if err := loaded.LoadFrom(path); err != nil {
		t.Fatalf("LoadFrom() error = %v", err)
	}
	if loaded.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng",
			loaded.data.LanguageProfiles["1"]["jpn"])
	}
	if loaded.data.UserTokens["2"] != "t2" {
		t.Errorf("UserTokens[2] = %q, want t2", loaded.data.UserTokens["2"])
	}
	if loaded.data.LastSchedulerRun != 1700000000 {
		t.Errorf("LastSchedulerRun = %d, want 1700000000", loaded.data.LastSchedulerRun)
	}
}

func TestCacheSaveToLeavesNoTempFileOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	c := New()

	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory contains %d entries, want 1 (cache.json only); found %v",
			len(entries), entries)
	}
	if entries[0].Name() != "cache.json" {
		t.Errorf("remaining file = %q, want cache.json", entries[0].Name())
	}
}

func TestCacheSaveToEnforces0600Permissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	c := New()
	c.data.UserTokens = map[string]string{"7": "secret-token"}

	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The token cache must be user-only. Reject any group/other bits.
	if got := info.Mode().Perm() & 0o077; got != 0 {
		t.Errorf("cache perm = %v (group/other bits set), want 0600",
			info.Mode().Perm())
	}
}

func TestCacheSaveToRejectsBadDir(t *testing.T) {
	t.Parallel()
	c := New()
	err := c.SaveTo("/nonexistent/path-never-created/cache.json")
	if err == nil {
		t.Fatal("SaveTo() on bad dir should return error, got nil")
	}
}

func TestCacheLoadFromEmptyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Cache{}
	err := c.LoadFrom(path)
	if err == nil {
		t.Fatal("LoadFrom() on empty file should return unmarshal error, got nil")
	}
}

func TestCacheLoadFromCorruptJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Cache{}
	err := c.LoadFrom(path)
	if err == nil {
		t.Fatal("LoadFrom() on corrupt JSON should return error, got nil")
	}
}

func TestCacheLoadFromMissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := &Cache{}
	if err := c.LoadFrom(filepath.Join(dir, "never-created.json")); err != nil {
		t.Errorf("LoadFrom() on missing file = %v, want nil", err)
	}
	if c.data.ProcessedEpisodes == nil {
		t.Error("ProcessedEpisodes should be initialized on missing file")
	}
	if c.data.LanguageProfiles == nil {
		t.Error("LanguageProfiles should be initialized on missing file")
	}
	if c.data.UserTokens == nil {
		t.Error("UserTokens should be initialized on missing file")
	}
}

// --- Concurrent stress (u1c1-006) ---

func TestCache_ConcurrentLearnAndRead(t *testing.T) {
	t.Parallel()
	c := New()

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N * 3)
	for i := range N {
		userID := strconv.Itoa(i % 5)
		go func() {
			defer wg.Done()
			c.LearnLanguageProfile(userID, "jpn", "eng")
		}()
		go func() {
			defer wg.Done()
			_, _ = c.SubtitleLangForAudio(userID, "jpn")
		}()
		go func() {
			defer wg.Done()
			c.MarkProcessed("play:" + userID + ":abc")
			_ = c.WasRecentlyProcessed("play:" + userID + ":abc")
		}()
	}
	wg.Wait()

	for i := range 5 {
		lang, ok := c.SubtitleLangForAudio(strconv.Itoa(i), "jpn")
		if !ok || lang != "eng" {
			t.Errorf("user %d: got (%q,%v), want (eng,true)", i, lang, ok)
		}
	}
}

func TestCache_ConcurrentLearnAndSetUserTokens(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetUserTokens(map[string]string{
		"2": "token-2",
		"3": "token-3",
	})

	const N = 30
	var wg sync.WaitGroup
	wg.Add(N * 2)
	for range N {
		go func() {
			defer wg.Done()
			_ = c.UserTokens()
		}()
		go func() {
			defer wg.Done()
			c.LearnLanguageProfile("2", "jpn", "eng")
		}()
	}
	wg.Wait()
}

// --- PBT: learnLanguageProfile last-write-wins + empty-audio no-op ---

func TestLearnLanguageProfile_LastWriteWinsPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := New()

		nWrites := rapid.IntRange(1, 20).Draw(t, "n_writes")
		expect := make(map[string]string)
		for i := range nWrites {
			user := rapid.SampledFrom([]string{"1", "2", "3"}).Draw(t, fmt.Sprintf("u_%d", i))
			audio := rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra"}).Draw(t, fmt.Sprintf("a_%d", i))
			sub := rapid.SampledFrom([]string{"", "eng", "jpn", "kor", "fra"}).Draw(t, fmt.Sprintf("s_%d", i))
			c.LearnLanguageProfile(user, audio, sub)
			expect[user+"|"+audio] = sub
		}
		for k, want := range expect {
			parts := strings.SplitN(k, "|", 2)
			user, audio := parts[0], parts[1]
			got, ok := c.SubtitleLangForAudio(user, audio)
			if !ok {
				t.Errorf("SubtitleLangForAudio(%q,%q): not found, want %q", user, audio, want)
				continue
			}
			if got != want {
				t.Errorf("SubtitleLangForAudio(%q,%q) = %q, want %q (last-write-wins)", user, audio, got, want)
			}
		}
	})
}

func TestLearnLanguageProfile_EmptyAudioIsNoOpPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := New()

		user := rapid.SampledFrom([]string{"1", "2"}).Draw(t, "user")
		sub := rapid.String().Draw(t, "sub")
		c.LearnLanguageProfile(user, "", sub)

		if profiles, ok := c.data.LanguageProfiles[user]; ok {
			if _, hasEmpty := profiles[""]; hasEmpty {
				t.Errorf("LearnLanguageProfile with empty audio created a %q entry", "")
			}
		}
	})
}

// --- UserTokens defensive-copy contract ---

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

// --- LastSchedulerRun conversion boundary ---

func TestLastSchedulerRunZeroReturnsZeroTime(t *testing.T) {
	t.Parallel()
	c := New()
	if got := c.LastSchedulerRun(); !got.IsZero() {
		t.Errorf("LastSchedulerRun() on fresh cache = %v, want zero time", got)
	}
}

func TestSetLastSchedulerRunRoundTrips(t *testing.T) {
	t.Parallel()
	c := New()
	want := time.Unix(1700000000, 0)
	c.SetLastSchedulerRun(want)
	if got := c.LastSchedulerRun(); !got.Equal(want) {
		t.Errorf("LastSchedulerRun() = %v, want %v", got, want)
	}
}

func TestSetLastSchedulerRunZeroClears(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetLastSchedulerRun(time.Unix(1700000000, 0))
	c.SetLastSchedulerRun(time.Time{})
	if got := c.LastSchedulerRun(); !got.IsZero() {
		t.Errorf("LastSchedulerRun() after zero set = %v, want zero", got)
	}
}

func TestCacheContract(t *testing.T) {
	t.Parallel()
	fakeapi.RunCacheContract(t, New())
}
