package plex

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
)

// SharedUserTokens fetches shared user tokens from the plex.tv shared_servers
// endpoint. This calls the plex.tv API (not the local server) and always
// uses TLS verification regardless of the client's SKIP_TLS_VERIFICATION
// setting — plex.tv is a public endpoint and the admin token must never be
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxResponseBody {
		slog.Warn("plex API response exceeded read cap; body truncated, likely an unfiltered or oversized response",
			"endpoint", "plex.tv shared_servers", "cap_bytes", maxResponseBody)
		return nil, fmt.Errorf("plex.tv shared_servers: response exceeded %d-byte read cap", maxResponseBody)
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
