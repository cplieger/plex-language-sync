package plex

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v2"
)

// fetchMetadata decodes GET <path> through the library's exported
// Metadata-envelope kernel into the app's own item type T (the
// internal/streams domain model), attaching this app's over-cap WARN
// contract. Transport, hardening, and the envelope decode are the
// library's (plexapi's (*Client).FetchMetadata generic method, promoted
// through the embed); the
// builder-typed path carries the endpoint's read-cap class, so a listing
// endpoint cannot land here.
//
// This wrapper is also the app's seam over a surface that cannot have one:
// the library's decoders are generic methods (Go 1.27), and a generic method
// can never satisfy an interface, so anything abstracting over the Plex read
// path abstracts over these non-generic wrappers instead.
func (c *Client) fetchMetadata[T any](ctx context.Context, path plexapi.Path) ([]T, error) {
	items, err := c.FetchMetadata[T](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// fetchEpisodeList is fetchMetadata under the library's large-listing read
// cap, for the ListPath builders (full section listings, recently-added
// windows). Concrete rather than generic: it has exactly one instantiation
// (streams.Episode), and a type parameter with one argument is a parameter
// that documents flexibility nothing uses. The library's FetchMetadataList
// stays generic — genuinely so, across the fleet.
func (c *Client) fetchEpisodeList(ctx context.Context, path plexapi.ListPath) ([]streams.Episode, error) {
	items, err := c.FetchMetadataList[streams.Episode](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// fetchSections is fetchMetadata for responses whose container field is
// named "Directory" (library sections). Concrete for the same reason as
// fetchEpisodeList: one instantiation, Section.
func (c *Client) fetchSections(ctx context.Context, path plexapi.Path) ([]Section, error) {
	items, err := c.FetchDirectory[Section](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// warnIfOverCap emits this app's operator-facing WARN when a read blew the
// library's response cap. The message text is plex-language-sync's own
// Loki-alerting contract (it predates the shared library and dashboards
// grep for it), so the APP owns the string; the library reports the
// condition via the typed error. Returns err unchanged for the caller.
func warnIfOverCap(err error, path string) error {
	if tooLarge, ok := errors.AsType[*plexapi.ResponseTooLargeError](err); ok {
		slog.Warn("plex API response exceeded read cap; body truncated, likely an unfiltered or oversized response",
			"path", path, "cap_bytes", tooLarge.Limit)
	}
	return err
}
