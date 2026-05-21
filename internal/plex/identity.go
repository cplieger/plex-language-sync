package plex

import (
	"context"
	"fmt"
	"strconv"
)

// ServerIdentity returns the Plex server's identity (friendly name, machine
// ID, version) from GET /.
func (c *Client) ServerIdentity(ctx context.Context) (*ServerIdentity, error) {
	var resp mc[ServerIdentity]
	if err := c.get(ctx, "/", &resp); err != nil {
		return nil, err
	}
	return &resp.MediaContainer, nil
}

// LoggedUser resolves the admin user by looking up the myplex account
// username and matching it against the system accounts list.
func (c *Client) LoggedUser(ctx context.Context) (*User, error) {
	// Get the admin username from myPlex account.
	var acctResp struct {
		Username string `json:"username"`
	}
	if err := c.get(ctx, "/myplex/account", &acctResp); err != nil {
		return nil, fmt.Errorf("fetching account: %w", err)
	}
	// Match against system accounts via the shared fetchAccounts helper.
	accounts, err := fetchAccounts[Account](ctx, c, "/accounts")
	if err != nil {
		return nil, fmt.Errorf("fetching system accounts: %w", err)
	}
	for _, a := range accounts {
		if a.Name == acctResp.Username {
			return &User{ID: strconv.Itoa(a.ID), Name: a.Name}, nil
		}
	}
	return nil, fmt.Errorf("admin user %q not found in system accounts", acctResp.Username)
}
