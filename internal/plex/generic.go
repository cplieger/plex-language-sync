package plex

import "context"

// fetchMetadata issues GET <path> and decodes the response into
// mc[struct{Metadata []T}]. Used by library-lookup methods whose JSON shape
// is {"MediaContainer":{"Metadata":[...]}}. Runtime-types-p4: the eight
// original collection-fetch getters all share this envelope; collapsing the
// ~8 LOC boilerplate onto a single generic helper removes ~50 LOC of
// near-identical copy-paste while preserving exact request semantics.
func fetchMetadata[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var resp mc[struct {
		Metadata []T `json:"Metadata"`
	}]
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// fetchDirectory is the same as fetchMetadata but for responses whose
// container field is named "Directory" (library sections, plugin lists).
func fetchDirectory[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var resp mc[struct {
		Directory []T `json:"Directory"`
	}]
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Directory, nil
}

// fetchAccounts is the Account-container sibling of fetchMetadata; the
// /accounts endpoint wraps its list in a field literally named "Account".
func fetchAccounts[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var resp mc[struct {
		Account []T `json:"Account"`
	}]
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Account, nil
}
