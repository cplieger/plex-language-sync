// Package ignore holds the cross-subsystem "should I skip this library
// / show / episode?" decision. One policy value constructed in
// main.run() is injected into notifyAdapter, Syncer, and Scheduler,
// replacing three duplicated ignore implementations and two inline
// slices.Contains guards in the deep-scan pass.
//
// Consumers declare their own narrow interface over the subset of
// *Policy they use (one method for the event path, two for the deep
// scan) rather than sharing a wide one, so a fake policy in a test only
// implements what that test's subject actually calls.
package ignore

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// MetadataReader is the one Plex read the label check needs: a show's
// metadata, for its labels. Declared here rather than accepted per call
// because every caller passed the same admin-scoped client — the label
// check is a property of the show, not of the user asking.
type MetadataReader interface {
	ShowMetadata(ctx context.Context, showRatingKey plex.RatingKey) (*plex.Show, error)
}

// Config configures New. Named fields because Libraries and Labels are
// two adjacent []string: a transposition type-checks, and the policy
// would then match library names against label tags and vice versa —
// silently ignoring nothing while reporting itself configured.
type Config struct {
	// Reader fetches show metadata for the label check. Nil disables the
	// label half of ShouldSkipEpisode (the library check still applies).
	Reader MetadataReader
	// Libraries are library section titles to skip entirely.
	Libraries []string
	// Labels are Plex label tags that exclude a show.
	Labels []string
}

// Policy encapsulates the ignore rules applied before touching a
// library, show, or episode. The zero value is valid and never skips
// anything.
type Policy struct {
	reader    MetadataReader
	libraries []string
	labels    []string
}

// New returns a Policy holding defensive copies of cfg's slices, so the
// caller can mutate its own env-var-derived slices afterwards without
// affecting the policy. Nil slices are allowed and produce a policy that
// always reports "do not skip".
func New(cfg Config) *Policy {
	// append([]string(nil), x...) defensively copies x and yields nil for a
	// nil or empty x (appending zero elements to a nil slice returns nil), so
	// the empty case needs no len() guard.
	return &Policy{
		reader:    cfg.Reader,
		libraries: append([]string(nil), cfg.Libraries...),
		labels:    append([]string(nil), cfg.Labels...),
	}
}

// IgnoreLibrary reports whether a library section title is on the
// ignore list. Case-sensitive, matching the pre-extraction behaviour in
// Syncer.shouldIgnoreLibrary and the deep scan's inline
// slices.Contains guards.
func (p *Policy) IgnoreLibrary(title string) bool {
	return slices.Contains(p.libraries, title)
}

// IgnoreShowLabels reports whether any of the show's labels match the
// ignore list. Case-sensitive equality on label.Tag, mirroring the
// pre-extraction hasIgnoreLabel helper.
func (p *Policy) IgnoreShowLabels(labels []streams.Label) bool {
	for _, label := range labels {
		if slices.Contains(p.labels, label.Tag) {
			return true
		}
	}
	return false
}

// ShouldSkipEpisode combines IgnoreLibrary + a ShowMetadata fetch +
// IgnoreShowLabels into a single decision. Returns true if the episode
// should be skipped for any reason.
//
// A nil ref is treated as "no reason to skip" (false) so callers can
// reach this from paths where the episode reference is absent without
// guarding at every call site.
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
func (p *Policy) ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool {
	if ref == nil {
		return false
	}
	if p.IgnoreLibrary(ref.LibraryTitle.Raw()) {
		slog.Debug("library ignored", "library", ref.LibraryTitle)
		return true
	}
	if ref.GrandparentRatingKey == "" || p.reader == nil {
		return false
	}
	show, err := p.reader.ShowMetadata(ctx, plex.RatingKey(ref.GrandparentRatingKey))
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
