package plex

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// SharedUserTokens fetches shared user tokens from the plex.tv shared_servers
// endpoint. This calls the plex.tv API (not the local server) and always
// uses full TLS verification (there is no skip option) — plex.tv is a
// public endpoint and the admin token must never be
// forwarded through a skipped-verification transport or a redirect.
func (c *Client) SharedUserTokens(ctx context.Context, machineIdentifier string) ([]SharedServerXML, error) {
	apiURL := "https://plex.tv/api/servers/" + url.PathEscape(machineIdentifier) + "/shared_servers"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("X-Plex-Token", c.token)

	// Use the shared plex.tv client — never skip TLS for public endpoints.
	resp, err := tvClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("plex.tv shared_servers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		drainBody(resp.Body)
		return nil, fmt.Errorf("plex.tv shared_servers: %s", resp.Status)
	}

	body, err := readCappedBody(resp.Body, "endpoint", "plex.tv shared_servers")
	if err != nil {
		if errors.Is(err, errBodyOverCap) {
			return nil, fmt.Errorf("plex.tv shared_servers: response exceeded %d-byte read cap", maxResponseBody)
		}
		return nil, err
	}

	// Some plex.tv responses use an empty body instead of an empty
	// <MediaContainer/>; xml.Unmarshal of a zero-length body returns
	// io.EOF, which would be misreported as a parse failure. Treat an
	// empty body as zero shared servers (mirrors the JSON read path's
	// len(body)==0 guard in client.go).
	if len(body) == 0 {
		return nil, nil
	}

	var result SharedServersXML
	if err := xml.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parsing shared_servers XML: %w", err)
	}
	return result.SharedServer, nil
}
