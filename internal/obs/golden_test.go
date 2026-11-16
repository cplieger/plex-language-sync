package obs_test

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/obs"
)

const regenerate = "UPDATE_GOLDEN=1 go test ./internal/obs/ -run TestExpositionMatchesGolden"

// TestExpositionMatchesGolden drives the registry to a fixed state (every
// family, the seeded label sets, a value per recorded instrument) and compares
// the /metrics page, minus the Go and process families whose values move
// between scrapes, to a committed file byte for byte. Dashboards and alert
// rules key on these names, labels and types.
func TestExpositionMatchesGolden(t *testing.T) {
	t.Parallel()
	m := obs.New("v1.2.3")
	base := time.Unix(1_800_000_000, 0)

	m.WebSocketConnected(true)
	m.WebSocketReconnect()
	m.WebSocketReconnect()
	m.DeepScanInterval(90*time.Minute + 500*time.Millisecond)
	m.DeepScanSeeded(base)
	m.DeepScanDue(50*time.Hour, base)
	m.DeepScanFinished(false, base.Add(time.Hour))
	m.DeepScanFinished(true, base.Add(2*time.Hour))
	m.ResolutionStalled(obs.StallAllClientsAbsent)
	for range 3 {
		m.PlayEvent(true)
	}
	m.PlayEvent(false)
	m.TrackUpdate(obs.TrackAudio, true)
	m.TrackUpdate(obs.TrackAudio, true)
	m.TrackUpdate(obs.TrackAudio, false)
	for range 4 {
		m.TrackUpdate(obs.TrackSubtitle, true)
	}
	m.CacheSaveFailed()
	m.SharedUsers(5)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}
	got := withoutRuntimeFamilies(t, rec.Result().Body)

	path := filepath.Join("testdata", "metrics.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; write it with %s", err, regenerate)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/metrics differs from %s (regenerate with %s only for a deliberate change)\n--- got\n%s\n--- want\n%s",
			path, regenerate, got, want)
	}
}

// withoutRuntimeFamilies drops every line of the go_ and process_ families,
// comments included, and keeps the rest of the page byte for byte.
func withoutRuntimeFamilies(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out bytes.Buffer
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		name := line
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name = rest
		}
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
