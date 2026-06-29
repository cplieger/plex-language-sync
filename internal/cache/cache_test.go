package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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

// --- LoadFrom: missing / empty / corrupt files ---

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

// --- SaveTo / LoadFrom round-trip + on-disk guarantees ---

func TestCacheSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	orig := New()
	orig.data.ProcessedEpisodes = map[string]int64{
		"play:1:100": time.Now().Unix(),
	}
	orig.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng", "eng": ""},
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
	// An empty subtitle string ("no subtitles" for that audio language) must
	// survive the disk round-trip rather than being dropped on save or load.
	if got, ok := loaded.data.LanguageProfiles["1"]["eng"]; !ok || got != "" {
		t.Errorf("LanguageProfiles[1][eng] = %q (present=%v), want empty string present", got, ok)
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
	// Parent is a regular file, so MkdirAll fails with ENOTDIR even as root;
	// SaveTo (via atomicfile.WriteFile with WithMkdirMode, which auto-creates
	// the dir) must error.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := New()
	err := c.SaveTo(filepath.Join(f, "subdir", "cache.json"))
	if err == nil {
		t.Fatal("SaveTo() under a file should return error, got nil")
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

// --- Concurrent stress: the mutex must protect every Cache surface ---

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

func TestCacheContract(t *testing.T) {
	t.Parallel()
	fakeapi.RunCacheContract(t, New())
}

// --- LoadFrom permissive-mode warning ---

// captureSlog redirects the default slog logger to a buffer for the duration
// of fn and returns everything logged. Tests using it must NOT be parallel
// (they mutate the process-global default logger).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// TestLoadFromWarnsOnPermissiveMode verifies that a world/group-readable cache
// file triggers a warning: the file holds user tokens, so a non-0600 mode is a
// disclosure risk that must be surfaced.
func TestLoadFromWarnsOnPermissiveMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; force the group/other-readable bits.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() {
		var c Cache
		if err := c.LoadFrom(path); err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
	})

	if !strings.Contains(out, "permissive mode") {
		t.Errorf("LoadFrom on a 0644 file logged %q, want a 'permissive mode' warning", out)
	}
}

// TestLoadFromQuietOnUserOnlyMode is the complement of the test above: a 0600
// (user-only) cache file must NOT warn. The pair pins the mode check in both
// directions so it cannot be satisfied by a check that fires on safe files.
func TestLoadFromQuietOnUserOnlyMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() {
		var c Cache
		if err := c.LoadFrom(path); err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
	})

	if strings.Contains(out, "permissive mode") {
		t.Errorf("LoadFrom on a 0600 file logged %q, want no 'permissive mode' warning", out)
	}
}

// --- SaveTo schema contract: empty UserTokens stays JSON null ---

// TestCacheSaveToWithKeyAndNoTokensWritesNull pins the on-disk schema contract
// for the encryption path: when an encryption key is set but there are no user
// tokens, SaveTo must NOT allocate a token map, so the nil UserTokens map
// serializes to JSON null (not {}). The package doc declares the persisted JSON
// schema an inviolate read-forward/write-back contract, so asserting the exact
// serialized form is legitimate here.
func TestCacheSaveToWithKeyAndNoTokensWritesNull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	// Zero-value Cache: every map (including UserTokens) is nil. Setting an
	// encryption key makes the key guard true, so the outcome hinges on the
	// "are there any tokens?" check skipping allocation for the empty map.
	key, err := DeriveKey("admin-token")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	var c Cache
	c.SetEncryptionKey(key)

	if err := c.SaveTo(path); err != nil {
		t.Fatalf("SaveTo() error = %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got := string(fields["user_tokens"]); got != "null" {
		t.Errorf("SaveTo(encKey set, nil UserTokens): user_tokens = %s, want null "+
			"(allocating an empty map would serialize to {} and break the schema)", got)
	}
}

func TestCacheSaveToEncryptFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")

	c := New()
	c.SetEncryptionKey([]byte("too-short"))
	c.SetUserTokens(map[string]string{"u1": "tok"})

	err := c.SaveTo(path)
	if err == nil {
		t.Fatal("SaveTo() with an invalid encryption key = nil, want error")
	}
	if !strings.Contains(err.Error(), "encrypt token for user") {
		t.Errorf("SaveTo() error = %q, want substring %q", err.Error(), "encrypt token for user")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("SaveTo() wrote a file despite encrypt failure (stat err = %v), want no file", statErr)
	}
}
