package cache

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestLoadFromWarnsOnPermissiveMode pins the permissive-mode guard
// (cache.go L86: `info.Mode().Perm()&0o077 != 0`). A world/group-readable
// cache file holds user tokens, so LoadFrom must warn. A CONDITIONALS_NEGATION
// mutation (`!= 0`→`== 0`) inverts the check so the warning fires for safe
// files and stays silent for the dangerous ones.
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

// TestLoadFromQuietOnUserOnlyMode is the complement of the test above: a
// 0600 (user-only) cache file must NOT warn. Together they kill the
// CONDITIONALS_NEGATION mutant in both directions.
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
