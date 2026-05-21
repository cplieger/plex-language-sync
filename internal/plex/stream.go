package plex

import (
	"context"
	"fmt"
)

// SetAudioStream selects the audio stream for a media part.
func (c *Client) SetAudioStream(ctx context.Context, partID, streamID int) error {
	path := fmt.Sprintf("/library/parts/%d?audioStreamID=%d&allParts=1", partID, streamID)
	return c.put(ctx, path)
}

// SetSubtitleStream selects the subtitle stream for a media part.
func (c *Client) SetSubtitleStream(ctx context.Context, partID, streamID int) error {
	path := fmt.Sprintf("/library/parts/%d?subtitleStreamID=%d&allParts=1", partID, streamID)
	return c.put(ctx, path)
}

// DisableSubtitles turns subtitles off for a media part.
func (c *Client) DisableSubtitles(ctx context.Context, partID int) error {
	path := fmt.Sprintf("/library/parts/%d?subtitleStreamID=0&allParts=1", partID)
	return c.put(ctx, path)
}
