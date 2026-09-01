package plex

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v2"
)

// fetchMetadata decodes GET <path> into T via the library's generic
// FetchMetadata, adding this app's over-cap WARN. Generic methods can't
// satisfy an interface, so this non-generic wrapper is the app's seam
// over the Plex read path.
func (c *Client) fetchMetadata[T any](ctx context.Context, path plexapi.Path) ([]T, error) {
	items, err := c.FetchMetadata[T](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// fetchEpisodeList is fetchMetadata under the library's large-listing
// read cap. Concrete, not generic: streams.Episode is its only
// instantiation.
func (c *Client) fetchEpisodeList(ctx context.Context, path plexapi.ListPath) ([]streams.Episode, error) {
	items, err := c.FetchMetadataList[streams.Episode](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// fetchSections is fetchMetadata for responses whose container field is
// named "Directory" (library sections).
func (c *Client) fetchSections(ctx context.Context, path plexapi.Path) ([]Section, error) {
	items, err := c.FetchDirectory[Section](ctx, path)
	return items, warnIfOverCap(err, string(path))
}

// warnIfOverCap emits this app's operator-facing WARN when a read blew
// the library's response cap. The message text is this app's own
// Loki-alerting contract, so it must not change independently of the
// alert rule. Returns err unchanged.
func warnIfOverCap(err error, path string) error {
	if tooLarge, ok := errors.AsType[*plexapi.ResponseTooLargeError](err); ok {
		slog.Warn("plex API response exceeded read cap; body truncated, likely an unfiltered or oversized response",
			"path", path, "cap_bytes", tooLarge.Limit)
	}
	return err
}
