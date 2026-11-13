package tracksync

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// captureLogs installs a Debug-level text handler over a buffer as the default
// logger for the test and returns that buffer. Tests using it must NOT be
// parallel (the default logger is process-global).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	capture.SwapDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// observeOK runs Observe for user "1" and fails the test on an error.
func observeOK(t *testing.T, s *Syncer, ep *streams.Episode) {
	t.Helper()
	if err := s.Observe(t.Context(), "1", ep, "play"); err != nil {
		t.Fatalf("Observe = %v, want nil", err)
	}
}
