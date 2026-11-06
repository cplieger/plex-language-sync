package cache

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

// v2Profiles is the decode target for a profiles.json fixture: each intent
// kept as its raw bytes, so a test can compare what the cache re-encodes.
type v2Profiles struct {
	LanguageProfiles map[string]map[string]string          `json:"language_profiles"`
	Intents          map[string]map[string]json.RawMessage `json:"intents"`
}

func readV2Profiles(t *testing.T, raw []byte) v2Profiles {
	t.Helper()
	var p v2Profiles
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode profiles fixture: %v", err)
	}
	return p
}

// assertIntentsMatch checks that c returns every fixture intent, with a zero
// anchor, and that it re-encodes to the fixture's own bytes.
func assertIntentsMatch(t *testing.T, c *Cache, want v2Profiles) {
	t.Helper()
	for user, byKey := range want.Intents {
		for key, raw := range byKey {
			got, ok := c.IntentFor(user, key)
			if !ok {
				t.Errorf("IntentFor(%s, %s) = ok=false, want the file's intent", user, key)
				continue
			}
			if !got.Anchor.IsZero() {
				t.Errorf("IntentFor(%s, %s) anchor = %+v, want zero for an intent written without one", user, key, got.Anchor)
			}
			enc, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal intent: %v", err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatalf("compact fixture intent: %v", err)
			}
			if !bytes.Equal(enc, compact.Bytes()) {
				t.Errorf("IntentFor(%s, %s) re-encodes to\n%s\nwant\n%s", user, key, enc, compact.Bytes())
			}
		}
	}
}

// TestLoadProfilesV2 pins that a profiles.json in the shape every released
// v2 build writes loads with no loss: every show-keyed intent and profile,
// before and after a save by this build.
func TestLoadProfilesV2(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "v2-profiles.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := readV2Profiles(t, raw)
	if users, shows := len(want.Intents), len(want.Intents["alice"])+len(want.Intents["bob"])+len(want.Intents["carol"]); users != 3 || shows != 6 {
		t.Fatalf("fixture holds %d users and %d intents, want 3 and 6", users, shows)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, profilesFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c := New()
	if err := c.Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	assertIntentsMatch(t, c, want)
	if !maps.EqualFunc(c.data.LanguageProfiles, want.LanguageProfiles, maps.Equal) {
		t.Errorf("LanguageProfiles = %v, want %v", c.data.LanguageProfiles, want.LanguageProfiles)
	}

	saved := t.TempDir()
	if err := c.Save(saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	reloaded := New()
	if err := reloaded.Load(saved); err != nil {
		t.Fatalf("Load() of the saved files error = %v", err)
	}
	assertIntentsMatch(t, reloaded, want)

	onDisk, err := os.ReadFile(filepath.Join(saved, profilesFile))
	if err != nil {
		t.Fatal(err)
	}
	written := readV2Profiles(t, onDisk)
	for user, byKey := range want.Intents {
		for key, raw := range byKey {
			var a, b bytes.Buffer
			if err := json.Compact(&a, raw); err != nil {
				t.Fatal(err)
			}
			if err := json.Compact(&b, written.Intents[user][key]); err != nil {
				t.Fatalf("saved intent %s/%s: %v", user, key, err)
			}
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Errorf("saved profiles.json intent %s/%s =\n%s\nwant\n%s", user, key, b.Bytes(), a.Bytes())
			}
		}
	}
}
