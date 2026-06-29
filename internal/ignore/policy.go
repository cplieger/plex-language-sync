// Package ignore holds the cross-subsystem "should I skip this library
// / show / episode?" decision. One policy value constructed in
// main.run() is injected into notifyAdapter, Syncer, and Scheduler via
// their Config, replacing three duplicated ignore implementations and
// two inline slices.Contains guards in the scheduler.
//
// Consumers depend on api.IgnoreChecker (declared in internal/api)
// rather than on *Policy directly, so tests can substitute a fake
// policy without importing this package.
package ignore

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Compile-time interface satisfaction assertion.
var _ api.IgnoreChecker = (*Policy)(nil)

// Policy encapsulates the ignore rules applied before touching a
// library, show, or episode. Zero value is valid: an empty Policy
// never skips anything.
//
// Construct with NewPolicy in the composition root; the Libraries and
// Labels fields hold defensive copies of the configured slices so the
// caller can mutate its own env-var-derived slices without affecting
// the policy.
type Policy struct {
	Libraries []string
	Labels    []string
}

// NewPolicy returns a Policy with defensive copies of the supplied
// slices. Nil inputs are allowed and produce an empty policy (which
// always reports "do not skip").
func NewPolicy(libraries, labels []string) *Policy {
	// append([]string(nil), x...) defensively copies x and yields nil for a
	// nil or empty x (appending zero elements to a nil slice returns nil), so
	// the empty case needs no len() guard.
	return &Policy{
		Libraries: append([]string(nil), libraries...),
		Labels:    append([]string(nil), labels...),
	}
}

// IgnoreLibrary reports whether a library section title is on the
// ignore list. Case-sensitive to match the pre-extraction behaviour in
// Syncer.shouldIgnoreLibrary and the scheduler's inline
// slices.Contains guards.
func (p *Policy) IgnoreLibrary(title string) bool {
	return slices.Contains(p.Libraries, title)
}

// IgnoreShowLabels reports whether any of the show's labels match the
// ignore list. Case-sensitive equality on label.Tag, mirroring the
// pre-extraction hasIgnoreLabel helper in sync/tracks.go.
func (p *Policy) IgnoreShowLabels(labels []streams.Label) bool {
	for _, label := range labels {
		if slices.Contains(p.Labels, label.Tag) {
			return true
		}
	}
	return false
}

// ShouldSkipEpisode combines IgnoreLibrary + a ShowMetadata fetch +
// IgnoreShowLabels into a single decision. Returns true if the episode
// should be skipped for any reason.
//
// A nil ref is treated as "no reason to skip" (false) so the scheduler
// can call this from paths where the episode reference is absent
// without guarding at every call site.
//
// On ShowMetadata fetch failure the method returns false (do not skip)
// to match the pre-extraction behaviour in Syncer.shouldIgnoreShow:
// conservatism here trades a single episode processed against a
// transient Plex blip for never silently dropping work on a real
// error.
//
// DEBUG log keys ("library ignored", "show ignored") are preserved
// verbatim from the three pre-extraction emit sites so any Loki
// query grepping on those strings keeps firing.
func (p *Policy) ShouldSkipEpisode(ctx context.Context, reader api.PlexReader, ref *streams.Episode) bool {
	if ref == nil {
		return false
	}
	if p.IgnoreLibrary(ref.LibraryTitle) {
		slog.Debug("library ignored", "library", ref.LibraryTitle)
		return true
	}
	if ref.GrandparentRatingKey == "" || reader == nil {
		return false
	}
	show, err := reader.ShowMetadata(ctx, plex.RatingKey(ref.GrandparentRatingKey))
	if err != nil {
		slog.Debug("ignore: show metadata fetch failed, not skipping",
			"show", ref.GrandparentTitle, "error", err)
		return false
	}
	if p.IgnoreShowLabels(show.Label) {
		slog.Debug("show ignored", "show", ref.GrandparentTitle)
		return true
	}
	return false
}
