package plex

import (
	"context"

	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi"
)

// ShowSections returns the TV-show library sections.
func (c *Client) ShowSections(ctx context.Context) ([]Section, error) {
	sections, err := fetchDirectory[Section](ctx, c, plexapi.SectionsPath())
	if err != nil {
		return nil, err
	}
	shows := sections[:0]
	for _, s := range sections {
		if s.Type == SectionTypeShow {
			shows = append(shows, s)
		}
	}
	return shows, nil
}

// Episode fetches episode (or any library item) metadata by rating key.
// Returns ErrNotFound when the item is missing. /library/metadata/{key} is
// type-agnostic; ShowMetadata wraps this variant for show-level lookups.
func (c *Client) Episode(ctx context.Context, rk RatingKey) (*streams.Episode, error) {
	path, err := plexapi.MetadataPath(rk)
	if err != nil {
		return nil, err
	}
	eps, err := fetchMetadata[streams.Episode](ctx, c, path)
	if err != nil {
		return nil, err
	}
	if len(eps) == 0 {
		return nil, ErrNotFound
	}
	return &eps[0], nil
}

// ShowEpisodes returns every episode in a show (allLeaves).
func (c *Client) ShowEpisodes(ctx context.Context, rk RatingKey) ([]streams.Episode, error) {
	path, err := plexapi.AllLeavesPath(rk)
	if err != nil {
		return nil, err
	}
	return fetchMetadata[streams.Episode](ctx, c, path)
}

// SeasonEpisodes returns the episodes of a single season (children).
func (c *Client) SeasonEpisodes(ctx context.Context, rk RatingKey) ([]streams.Episode, error) {
	path, err := plexapi.ChildrenPath(rk)
	if err != nil {
		return nil, err
	}
	return fetchMetadata[streams.Episode](ctx, c, path)
}

// ShowMetadata fetches the show-level metadata (labels, library) for a show.
// /library/metadata/{key} returns whatever type the key points to, so this
// delegates to the same endpoint as Episode but decodes into *Show.
// Split off from Episode: a show response does not
// have Media/Part/Stream, so typing it as *Show instead of *Episode keeps
// the field set honest for callers (e.g. shouldIgnoreShow reads only
// LibraryTitle + Label).
func (c *Client) ShowMetadata(ctx context.Context, rk RatingKey) (*Show, error) {
	path, err := plexapi.MetadataPath(rk)
	if err != nil {
		return nil, err
	}
	shows, err := fetchMetadata[Show](ctx, c, path)
	if err != nil {
		return nil, err
	}
	if len(shows) == 0 {
		return nil, ErrNotFound
	}
	return &shows[0], nil
}

// RecentlyAdded fetches recently added episodes from a library section,
// filtered server-side by addedAt >= sinceUnix. The path — including the
// literal single-char `addedAt>=` operator — is the library's
// RecentlyAddedPath builder; this app owns only the Episode decode shape.
// The builder returns a ListPath, so the read rides the large-listing cap
// like the library's own typed method (this call site once sat under the
// 10 MB general cap while plexapi's RecentlyAdded used 40 MB — the
// cap-class drift the typed builders now close at compile time).
func (c *Client) RecentlyAdded(ctx context.Context, sectionKey RatingKey, sinceUnix int64) ([]streams.Episode, error) {
	path, err := plexapi.RecentlyAddedPath(sectionKey, MetadataTypeEpisode, sinceUnix)
	if err != nil {
		return nil, err
	}
	return fetchMetadataList[streams.Episode](ctx, c, path)
}
