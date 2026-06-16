package plex

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

// captureSlog redirects the default slog logger to a buffer for the duration
// of fn and returns everything logged. It restores the previous default
// logger on cleanup. Tests using it must NOT be parallel (they mutate the
// process-global default logger).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// TestWarnIfPlaintextURL_WarnsOnRemoteHTTP pins both comparisons on
// client.go L101 (`u == nil` and `u.Scheme != "http"`). An http:// URL to a
// dotted remote host must emit the plaintext-token warning.
//
//   - `u == nil`→`u != nil` returns early for a non-nil URL → no warning.
//   - `u.Scheme != "http"`→`==` returns early for an http URL → no warning.
func TestWarnIfPlaintextURL_WarnsOnRemoteHTTP(t *testing.T) {
	u, err := url.Parse("http://plex.example.com:32400")
	if err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() { WarnIfPlaintextURL(u) })

	if !strings.Contains(out, "transit unencrypted") {
		t.Errorf("WarnIfPlaintextURL(%q) logged %q, want a warning containing 'transit unencrypted'", u, out)
	}
}

// TestWarnIfPlaintextURL_QuietForDottedLoopback pins the
// `host == "127.0.0.1"` comparison on client.go L105. The dotted loopback IP
// must be suppressed by that explicit check; a CONDITIONALS_NEGATION
// (`==`→`!=`) lets 127.0.0.1 fall through to the FQDN warning (it contains
// dots, so the `!strings.Contains(host, ".")` guard does not catch it).
func TestWarnIfPlaintextURL_QuietForDottedLoopback(t *testing.T) {
	u, err := url.Parse("http://127.0.0.1:32400")
	if err != nil {
		t.Fatal(err)
	}

	out := captureSlog(t, func() { WarnIfPlaintextURL(u) })

	if strings.Contains(out, "transit unencrypted") {
		t.Errorf("WarnIfPlaintextURL(127.0.0.1) logged %q, want no warning (loopback IP)", out)
	}
}

// TestNewHTTPClient_DefaultTimeout pins the request timeout on client.go
// L132 (`30 * time.Second`). An ARITHMETIC_BASE mutation (e.g. `*`→`/`)
// collapses the timeout to 0 (no timeout), which is a silent reliability
// regression.
func TestNewHTTPClient_DefaultTimeout(t *testing.T) {
	c, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}

	if c.Timeout != 30*time.Second {
		t.Errorf("newHTTPClient timeout = %v, want 30s", c.Timeout)
	}
}

// TestDrainBody_LogsOnNonEOFError pins the error check on client.go L232
// (`err != nil && !errors.Is(err, io.EOF)`). A genuine (non-EOF) read error
// must be logged at debug. A CONDITIONALS_NEGATION mutation
// (`err != nil`→`err == nil`) inverts the check so real errors are silently
// dropped (and successful drains would log spuriously).
func TestDrainBody_LogsOnNonEOFError(t *testing.T) {
	body := io.NopCloser(&errReader{err: fmt.Errorf("connection reset")})

	out := captureSlog(t, func() { drainBody(body) })

	if !strings.Contains(out, "failed to drain response body") {
		t.Errorf("drainBody on a non-EOF error logged %q, want a debug line 'failed to drain response body'", out)
	}
}
