package plex

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cplieger/plexapi"
)

// fetchMetadata issues GET <path> and decodes the response into the
// {"MediaContainer":{"Metadata":[...]}} envelope with the app's own item
// type T (the internal/streams domain model). The transport and its
// hardening are the library's Get.
func fetchMetadata[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var resp plexapi.MC[struct {
		Metadata []T `json:"Metadata"`
	}]
	if err := c.Get(ctx, path, &resp); err != nil {
		return nil, warnIfOverCap(err, path)
	}
	return resp.MediaContainer.Metadata, nil
}

// fetchDirectory is fetchMetadata for responses whose container field is
// named "Directory" (library sections).
func fetchDirectory[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var resp plexapi.MC[struct {
		Directory []T `json:"Directory"`
	}]
	if err := c.Get(ctx, path, &resp); err != nil {
		return nil, warnIfOverCap(err, path)
	}
	return resp.MediaContainer.Directory, nil
}

// warnIfOverCap emits this app's operator-facing WARN when a read blew the
// library's response cap. The message text is plex-language-sync's own
// Loki-alerting contract (it predates the shared library and dashboards
// grep for it), so the APP owns the string; the library reports the
// condition via the typed error. Returns err unchanged for the caller.
func warnIfOverCap(err error, path string) error {
	var tooLarge *plexapi.ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		slog.Warn("plex API response exceeded read cap; body truncated, likely an unfiltered or oversized response",
			"path", path, "cap_bytes", tooLarge.Limit)
	}
	return err
}
