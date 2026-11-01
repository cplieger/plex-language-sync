package tracksync

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/cplieger/slogx/capture"
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
