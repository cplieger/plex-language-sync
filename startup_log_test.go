package main

import (
	"testing"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// Not parallel: it swaps the process-global default logger.
func TestLogConnected_sanitizesPlexValues(t *testing.T) {
	rec := capture.Default(t)

	logConnected(
		&plex.ServerIdentity{FriendlyName: hostile, MachineIdentifier: hostile, Version: hostile},
		&plex.User{ID: hostile, Name: hostile},
	)

	want := textsafe.SanitizeSingleLine(hostile)
	for _, c := range []struct{ msg, key string }{
		{"connected to plex server", "name"},
		{"connected to plex server", "id"},
		{"connected to plex server", "version"},
		{"authenticated as admin user", "name"},
		{"authenticated as admin user", "id"},
	} {
		if !rec.HasAttr(c.msg, c.key, want) {
			t.Errorf("%q %s was not logged as %q; a value read from Plex reached the log raw: %v", c.msg, c.key, want, rec.Records())
		}
	}
}
