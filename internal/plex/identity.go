package plex

import (
	"context"
	"fmt"
	"strconv"
)

// ServerIdentity returns the Plex server's identity (friendly name, machine
// ID, version) from GET /. Delegates to the library's Identity.
func (c *Client) ServerIdentity(ctx context.Context) (*ServerIdentity, error) {
	return c.Identity(ctx)
}

// LoggedUser resolves the admin user by looking up the myplex account
// username and matching it against the system accounts list (the library's
// AdminAccount), shaped into the app's User type.
func (c *Client) LoggedUser(ctx context.Context) (*User, error) {
	acct, err := c.AdminAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving admin user: %w", err)
	}
	return &User{ID: strconv.Itoa(acct.ID), Name: acct.Name}, nil
}
