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

// LoggedUser resolves the admin (server owner) user via the library's
// AdminAccount — system account id 1, the same server-local id space that
// sessions and watch history report — shaped into the app's User type.
// plexapi v1.1.2 fixed AdminAccount returning the id-0 placeholder (the
// /myplex/account envelope + email-username made name-matching resolve the
// wrong account, so owner play/history events were skipped).
func (c *Client) LoggedUser(ctx context.Context) (*User, error) {
	acct, err := c.AdminAccount(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving admin user: %w", err)
	}
	return &User{ID: strconv.Itoa(acct.ID), Name: acct.Name}, nil
}
