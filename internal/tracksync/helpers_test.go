package tracksync

import (
	"bytes"
	"io"
	"log"
	"log/slog"
	"testing"
)

// captureLogs installs a Debug-level text handler over a buffer as the default
// logger for the test and returns that buffer. Tests using it must NOT be
// parallel (the default logger is process-global).
//
// slog.SetDefault also points the log package at the installed handler and
// zeroes its flags, and skips that redirect for slog's own default handler, so
// restoring slog alone leaves log writing into a dead buffer. slog goes back
// first: reinstalling a non-default prev re-runs the redirect.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, prevWriter, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestCaptureLogs_restoresLogPackage pins all three globals slog.SetDefault
// mutates. A sentinel writer and a non-zero flag set are installed first so
// neither assertion can hold by accident: the incomplete restore leaves the
// log package aimed at the capture buffer with its flags zeroed, which
// silences every later slog call in the package.
func TestCaptureLogs_restoresLogPackage(t *testing.T) {
	prevWriter, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	var sentinel bytes.Buffer
	log.SetOutput(&sentinel)
	log.SetFlags(log.Lshortfile)

	t.Run("capture", func(t *testing.T) {
		buf := captureLogs(t)
		slog.Info("captured")
		if buf.Len() == 0 {
			t.Fatal("captureLogs() captured nothing; the swap itself is broken, so the restore assertions below would be vacuous")
		}
	})

	if got := log.Writer(); got != io.Writer(&sentinel) {
		t.Errorf("after captureLogs cleanup, log.Writer() = %T, want the sentinel *bytes.Buffer", got)
	}
	if got := log.Flags(); got != log.Lshortfile {
		t.Errorf("after captureLogs cleanup, log.Flags() = %d, want %d", got, log.Lshortfile)
	}
}
