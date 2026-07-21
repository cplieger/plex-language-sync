package cache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"pgregory.net/rapid"
)

// splitFiles lists the three split-layout file names, used by tests that
// assert per-file properties.
var splitFiles = []string{profilesFile, tokensFile, stateFile}

// --- Load: fresh dir / missing files ---

func TestCacheLoadFreshDirIsNil(t *testing.T) {
	t.Parallel()
	var c Cache
	// Empty temp dir — Load returns nil and initializes the maps.
	if err := c.Load(t.TempDir()); err != nil {
		t.Errorf("Load() on fresh dir = %v, want nil", err)
	}
	if c.data.ProcessedEpisodes == nil {
		t.Error("ProcessedEpisodes should be initialized even on a fresh dir")
	}
	if c.data.LanguageProfiles == nil {
		t.Error("LanguageProfiles should be initialized even on a fresh dir")
	}
	if c.data.UserTokens == nil {
		t.Error("UserTokens should be initialized even on a fresh dir")
	}
}

// --- Save / Load round-trip + on-disk guarantees ---

func TestCacheSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	orig := New()
	orig.data.ProcessedEpisodes = map[string]int64{
		"play:1:100": time.Now().Unix(),
	}
	orig.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng", "eng": ""},
	}
	orig.data.UserTokens = map[string]string{"2": "t2"}
	orig.data.LastSchedulerRun = 1700000000

	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, name := range splitFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Save() did not write %s: %v", name, err)
		}
	}
	loaded := &Cache{}
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
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

func TestCacheSaveLeavesNoTempFileOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := New()

	if err := c.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(splitFiles) {
		t.Errorf("directory contains %d entries, want %d (the split files only); found %v",
			len(entries), len(splitFiles), entries)
	}
	for _, e := range entries {
		switch e.Name() {
		case profilesFile, tokensFile, stateFile:
		default:
			t.Errorf("unexpected file %q left behind by Save", e.Name())
		}
	}
}

func TestCacheSaveEnforces0600Permissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := New()
	c.data.UserTokens = map[string]string{"7": "secret-token"}

	if err := c.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, name := range splitFiles {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		// Every cache file is user-only (tokens live in one of them and the
		// uniform mode keeps the contract simple). Reject group/other bits.
		if got := info.Mode().Perm() & 0o077; got != 0 {
			t.Errorf("%s perm = %v (group/other bits set), want 0600",
				name, info.Mode().Perm())
		}
	}
}

func TestCacheSaveRejectsBadDir(t *testing.T) {
	t.Parallel()
	// Parent is a regular file, so MkdirAll fails with ENOTDIR even as root;
	// Save (via atomicfile.WriteFile with WithMkdirMode, which auto-creates
	// the dir) must error.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	c := New()
	if err := c.Save(filepath.Join(f, "subdir")); err == nil {
		t.Fatal("Save() under a file should return error, got nil")
	}
}

// TestCacheSaveRejectsOverCapSection pins the write-side mirror of the
// loader's maxCacheSize bound: a section whose encoded payload exceeds the
// cap fails Save loudly (atomicfile.ErrFileTooLarge through the per-file
// joined error) and leaves the previously saved file intact, instead of
// persisting a file the next boot's bounded read would refuse — silently
// resetting learned profiles.
func TestCacheSaveRejectsOverCapSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	orig := New()
	orig.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
	if err := orig.Save(dir); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	over := New()
	// One value larger than the cap pushes the encoded profiles.json over
	// maxCacheSize; tokens.json and state.json stay small and still write.
	over.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": strings.Repeat("a", maxCacheSize+1)},
	}
	err := over.Save(dir)
	if err == nil {
		t.Fatal("Save() with an over-cap section = nil, want error")
	}
	if !errors.Is(err, atomicfile.ErrFileTooLarge) {
		t.Errorf("Save() error = %v, want errors.Is(..., atomicfile.ErrFileTooLarge)", err)
	}
	if !strings.Contains(err.Error(), profilesFile) {
		t.Errorf("Save() error = %q, want it to name %s", err.Error(), profilesFile)
	}

	// The previous profiles.json is untouched: atomicfile rejects over-cap
	// content before the temp file ever replaces the target.
	loaded := &Cache{}
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() after failed Save error = %v", err)
	}
	if got := loaded.data.LanguageProfiles["1"]["jpn"]; got != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] after failed Save = %q, want eng (previous file intact)", got)
	}
}

// --- Per-section corruption isolation (the split layout's purpose) ---

// TestCacheLoadCorruptSectionIsolation pins the reason the cache is split
// across three files: corrupting ONE file resets ONLY its own section while
// the other two load intact, and Load surfaces the failure as a non-nil
// error. A regression that couples the sections again (one decode error
// resetting everything) fails the intact-section assertions.
func TestCacheLoadCorruptSectionIsolation(t *testing.T) {
	t.Parallel()
	seed := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		orig := New()
		orig.data.ProcessedEpisodes = map[string]int64{"streams:1:100:1:2": time.Now().Unix()}
		orig.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
		orig.data.UserTokens = map[string]string{"2": "t2"}
		orig.data.LastSchedulerRun = 1700000000
		if err := orig.Save(dir); err != nil {
			t.Fatalf("seed Save() error = %v", err)
		}
		return dir
	}

	cases := []struct {
		name    string
		corrupt string
		payload string
	}{
		{"corrupt profiles resets only profiles", profilesFile, "{not json"},
		{"corrupt tokens resets only tokens", tokensFile, "{not json"},
		{"empty state resets only state", stateFile, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := seed(t)
			if err := os.WriteFile(filepath.Join(dir, tc.corrupt), []byte(tc.payload), 0o600); err != nil {
				t.Fatal(err)
			}

			loaded := New()
			if err := loaded.Load(dir); err == nil {
				t.Error("Load() with a corrupt section = nil, want the section's error surfaced")
			}

			gotProfiles := loaded.data.LanguageProfiles["1"]["jpn"] == "eng"
			gotTokens := loaded.data.UserTokens["2"] == "t2"
			gotState := loaded.data.LastSchedulerRun == 1700000000 &&
				len(loaded.data.ProcessedEpisodes) == 1

			if want := tc.corrupt != profilesFile; gotProfiles != want {
				t.Errorf("profiles survived = %v, want %v", gotProfiles, want)
			}
			if want := tc.corrupt != tokensFile; gotTokens != want {
				t.Errorf("tokens survived = %v, want %v", gotTokens, want)
			}
			if want := tc.corrupt != stateFile; gotState != want {
				t.Errorf("state survived = %v, want %v", gotState, want)
			}
		})
	}
}

// --- Legacy cache.json migration ---

// legacyWrite writes a pre-split union cache.json into dir.
func legacyWrite(t *testing.T, dir string, d Data) {
	t.Helper()
	raw, err := json.MarshalIndent(&d, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyCacheFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCacheLoadMigratesLegacyCacheJSON pins the migration contract: a
// pre-split cache.json seeds every section, the three split files are
// written eagerly during Load, the legacy file is removed, and a second
// Load from the migrated layout yields the same state.
func TestCacheLoadMigratesLegacyCacheJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyWrite(t, dir, Data{
		ProcessedEpisodes: map[string]int64{"timeline:9": time.Now().Unix()},
		LanguageProfiles:  map[string]map[string]string{"1": {"jpn": "eng"}},
		UserTokens:        map[string]string{"2": "plain-tok"},
		LastSchedulerRun:  1700000000,
	})

	key, err := DeriveKey("admin-token")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	c := New()
	c.SetEncryptionKey(key)
	if err := c.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if c.data.LanguageProfiles["1"]["jpn"] != "eng" || c.data.UserTokens["2"] != "plain-tok" ||
		c.data.LastSchedulerRun != 1700000000 || len(c.data.ProcessedEpisodes) != 1 {
		t.Errorf("migrated state incomplete: %+v", c.data)
	}
	for _, name := range splitFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("migration did not write %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, legacyCacheFile)); !os.IsNotExist(err) {
		t.Errorf("legacy cache.json still present after migration (stat err = %v)", err)
	}
	// The eager migration save must encrypt the legacy plaintext token.
	raw, err := os.ReadFile(filepath.Join(dir, tokensFile))
	if err != nil {
		t.Fatal(err)
	}
	var td tokensData
	if err := json.Unmarshal(raw, &td); err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(td.UserTokens["2"]) {
		t.Errorf("migrated on-disk token = %q, want encrypted", td.UserTokens["2"])
	}

	// Idempotence: a second Load from the migrated layout sees the same state.
	again := New()
	again.SetEncryptionKey(key)
	if err := again.Load(dir); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if again.data.UserTokens["2"] != "plain-tok" || again.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Errorf("post-migration reload state = %+v, want original values", again.data)
	}
}

// TestCacheLoadSplitWinsOverStaleLegacy pins per-section precedence: when
// the split layout is complete, a lingering legacy cache.json (leftover
// from an interrupted migration cleanup) is ignored for every section and
// removed.
func TestCacheLoadSplitWinsOverStaleLegacy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	split := New()
	split.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
	split.data.UserTokens = map[string]string{"2": "new-tok"}
	split.data.LastSchedulerRun = 2000000000
	if err := split.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	legacyWrite(t, dir, Data{
		LanguageProfiles: map[string]map[string]string{"1": {"jpn": "STALE"}},
		UserTokens:       map[string]string{"2": "STALE"},
		LastSchedulerRun: 1,
	})

	loaded := New()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Errorf("profiles = %q, want the split file's value to win over stale legacy",
			loaded.data.LanguageProfiles["1"]["jpn"])
	}
	if loaded.data.UserTokens["2"] != "new-tok" {
		t.Errorf("tokens = %q, want the split file's value to win over stale legacy",
			loaded.data.UserTokens["2"])
	}
	if loaded.data.LastSchedulerRun != 2000000000 {
		t.Errorf("LastSchedulerRun = %d, want the split file's value", loaded.data.LastSchedulerRun)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyCacheFile)); !os.IsNotExist(err) {
		t.Errorf("stale legacy cache.json not removed (stat err = %v)", err)
	}
}

// TestCacheLoadLegacyFillsMissingSection pins the half-migrated recovery
// path: a section whose split file exists loads from it, while sections
// with no split file yet inherit the legacy values; Load then completes
// the migration.
func TestCacheLoadLegacyFillsMissingSection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacyWrite(t, dir, Data{
		LanguageProfiles: map[string]map[string]string{"1": {"jpn": "LEGACY"}},
		UserTokens:       map[string]string{"2": "legacy-tok"},
		LastSchedulerRun: 1700000000,
	})
	pd, err := json.Marshal(&profilesData{
		LanguageProfiles: map[string]map[string]string{"1": {"jpn": "SPLIT"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, profilesFile), pd, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded := New()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.data.LanguageProfiles["1"]["jpn"] != "SPLIT" {
		t.Errorf("profiles = %q, want the existing split file to win", loaded.data.LanguageProfiles["1"]["jpn"])
	}
	if loaded.data.UserTokens["2"] != "legacy-tok" {
		t.Errorf("tokens = %q, want the legacy value for the missing section", loaded.data.UserTokens["2"])
	}
	if loaded.data.LastSchedulerRun != 1700000000 {
		t.Errorf("LastSchedulerRun = %d, want the legacy value", loaded.data.LastSchedulerRun)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyCacheFile)); !os.IsNotExist(err) {
		t.Errorf("legacy cache.json not removed after completing migration (stat err = %v)", err)
	}
}

// TestCacheLoadMigrationSaveFailureKeepsLegacy pins the migration failure
// path: when the eager migration save cannot write (read-only dir), the
// legacy file is retained as the durable source and the in-memory state is
// still fully loaded.
func TestCacheLoadMigrationSaveFailureKeepsLegacy(t *testing.T) {
	dir := t.TempDir()
	legacyWrite(t, dir, Data{
		UserTokens: map[string]string{"2": "legacy-tok"},
	})
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	c := New()
	out := captureSlog(t, func() {
		if err := c.Load(dir); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	})

	if c.data.UserTokens["2"] != "legacy-tok" {
		t.Errorf("tokens = %q, want legacy value loaded despite failed migration save", c.data.UserTokens["2"])
	}
	if !strings.Contains(out, "migration save failed") {
		t.Errorf("Load logged %q, want a 'migration save failed' warning", out)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyCacheFile)); err != nil {
		t.Errorf("legacy cache.json must be retained when migration cannot write: %v", err)
	}
	for _, name := range splitFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("split file %s unexpectedly present in read-only dir (stat err = %v)", name, err)
		}
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

// --- Load permissive-mode warning ---

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

// TestLoadWarnsOnPermissiveTokensMode verifies that a world/group-readable
// tokens file triggers a warning: it holds user tokens, so a non-0600 mode
// is a disclosure risk that must be surfaced.
func TestLoadWarnsOnPermissiveTokensMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, tokensFile)
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; force the group/other-readable bits.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() {
		var c Cache
		if err := c.Load(dir); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	if !strings.Contains(out, "permissive mode") {
		t.Errorf("Load on a 0644 tokens file logged %q, want a 'permissive mode' warning", out)
	}
}

// TestLoadQuietOnUserOnlyMode is the complement of the test above: a 0600
// (user-only) tokens file must NOT warn — and neither must a permissive
// PROFILES file, which holds no secrets. The pair pins the mode check in
// both directions AND to the secret-bearing file only.
func TestLoadQuietOnUserOnlyMode(t *testing.T) {
	dir := t.TempDir()
	tokensPath := filepath.Join(dir, tokensFile)
	if err := os.WriteFile(tokensPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokensPath, 0o600); err != nil {
		t.Fatal(err)
	}
	// A permissive no-secrets file must stay quiet.
	profilesPath := filepath.Join(dir, profilesFile)
	if err := os.WriteFile(profilesPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(profilesPath, 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() {
		var c Cache
		if err := c.Load(dir); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	if strings.Contains(out, "permissive mode") {
		t.Errorf("Load logged %q, want no 'permissive mode' warning (tokens are 0600; profiles hold no secrets)", out)
	}
}

// --- Save schema contract: empty UserTokens stays JSON null ---

// TestCacheSaveWithKeyAndNoTokensWritesNull pins the on-disk schema contract
// for the encryption path: when an encryption key is set but there are no user
// tokens, Save must NOT allocate a token map, so the nil UserTokens map
// serializes to JSON null (not {}) in tokens.json. The package doc declares
// the persisted JSON schema an inviolate read-forward/write-back contract, so
// asserting the exact serialized form is legitimate here.
func TestCacheSaveWithKeyAndNoTokensWritesNull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Zero-value Cache: every map (including UserTokens) is nil. Setting an
	// encryption key makes the key guard true, so the outcome hinges on the
	// "are there any tokens?" check skipping allocation for the empty map.
	key, err := DeriveKey("admin-token")
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	var c Cache
	c.SetEncryptionKey(key)

	if err := c.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, tokensFile))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	if got := string(fields["user_tokens"]); got != "null" {
		t.Errorf("Save(encKey set, nil UserTokens): user_tokens = %s, want null "+
			"(allocating an empty map would serialize to {} and break the schema)", got)
	}
}

func TestCacheSaveEncryptFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	c := New()
	c.SetEncryptionKey([]byte("too-short"))
	c.SetUserTokens(map[string]string{"u1": "tok"})

	err := c.Save(dir)
	if err == nil {
		t.Fatal("Save() with an invalid encryption key = nil, want error")
	}
	if !strings.Contains(err.Error(), "encrypt token for user") {
		t.Errorf("Save() error = %q, want substring %q", err.Error(), "encrypt token for user")
	}
	// Encoding happens for all three files before any write, so an encrypt
	// failure must leave NO file behind — not even the secret-free ones.
	for _, name := range splitFiles {
		if _, statErr := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			t.Errorf("Save() wrote %s despite encrypt failure (stat err = %v), want no file", name, statErr)
		}
	}
}

// TestCacheSavePrunesStaleProcessedEntries pins that Save -> encodeAllForSave
// prunes >24h processed-episode entries before persisting, so the on-disk map
// cannot grow unbounded across restarts.
func TestCacheSavePrunesStaleProcessedEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	c := New()
	c.data.ProcessedEpisodes["stale"] = time.Now().Add(-25 * time.Hour).Unix()
	c.data.ProcessedEpisodes["fresh"] = time.Now().Unix()

	if err := c.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded := New()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, ok := loaded.data.ProcessedEpisodes["stale"]; ok {
		t.Error("Save persisted a >24h stale processed entry; encodeAllForSave must prune before writing")
	}
	if _, ok := loaded.data.ProcessedEpisodes["fresh"]; !ok {
		t.Error("Save dropped a fresh (<24h) processed entry; only stale entries should be pruned")
	}
}

// TestCacheLoadStatErrorPropagates pins that Load propagates a stat error
// that is NOT os.ErrNotExist rather than swallowing it as a missing-file
// fresh start. A regular file used as a path component makes os.Stat fail
// with ENOTDIR (not ErrNotExist) for every cache file in the dir.
func TestCacheLoadStatErrorPropagates(t *testing.T) {
	t.Parallel()
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	var c Cache
	err := c.Load(filepath.Join(f, "subdir"))
	if err == nil {
		t.Fatal("Load() with a non-ErrNotExist stat error = nil, want the error propagated")
	}
	if c.data.ProcessedEpisodes == nil {
		t.Error("Load() left ProcessedEpisodes nil on the stat-error return; maps must be reset first")
	}
}

// --- Intent ledger persistence (profiles.json) ---

// TestCacheIntentsPersistInProfilesFile pins the intent ledger's on-disk
// home: intents round-trip through Save/Load, live in profiles.json (the
// irreplaceable-state file), preserve the nil-subtitle marker, and reset
// together with profiles when that one file corrupts — without costing
// tokens or state.
func TestCacheIntentsPersistInProfilesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	orig := New()
	orig.RecordIntent("1", "42", streams.NewIntent(
		&streams.Stream{LanguageCode: "jpn", Codec: "eac3"},
		nil, // "no subtitles" must survive the round-trip
		1700000000,
	))
	orig.SetUserTokens(map[string]string{"2": "t2"})
	if err := orig.Save(dir); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// The ledger lands in profiles.json, not the other files.
	raw, err := os.ReadFile(filepath.Join(dir, profilesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"intents"`) {
		t.Error("profiles.json does not contain the intents section")
	}

	loaded := New()
	if err := loaded.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	intent, ok := loaded.IntentFor("1", "42")
	if !ok {
		t.Fatal("IntentFor after disk round-trip = ok=false, want the recorded intent")
	}
	if intent.Audio.LanguageCode != "jpn" || intent.Audio.Codec != "eac3" {
		t.Errorf("intent audio after round-trip = %+v, want jpn/eac3", intent.Audio)
	}
	if intent.Subtitle != nil {
		t.Errorf("intent subtitle after round-trip = %+v, want nil (no-subtitles marker)", intent.Subtitle)
	}
	if intent.ObservedAt != 1700000000 {
		t.Errorf("ObservedAt after round-trip = %d, want 1700000000", intent.ObservedAt)
	}

	// Corrupting profiles.json resets intents (same retention class as
	// profiles) while tokens survive.
	if err := os.WriteFile(filepath.Join(dir, profilesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := New()
	if err := reloaded.Load(dir); err == nil {
		t.Error("Load() with corrupt profiles file = nil, want error surfaced")
	}
	if _, ok := reloaded.IntentFor("1", "42"); ok {
		t.Error("intent survived a corrupt profiles.json; intents must reset with their section")
	}
	if reloaded.data.UserTokens["2"] != "t2" {
		t.Error("tokens lost to a corrupt profiles.json; sections must stay isolated")
	}
}
