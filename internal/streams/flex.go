package streams

import (
	"fmt"

	"github.com/cplieger/jsonx/v2"
)

// FlexInt unmarshals a Plex JSON field that may arrive as a number or a
// quoted numeric string (Plex is inconsistent across endpoints for
// numeric index fields). Decodes both shapes into a plain int; null and
// absent fields decode to 0.
//
// Exported because internal/plex embeds it in HistoryItem.AccountID and
// HistoryItem.ViewedAt.
//
// Wire-origin string fields (Episode.RatingKey, HistoryItem.RatingKey)
// stay typed as string: the Plex wire format for rating keys is a
// string and must be preserved as such. FlexInt only replaces fields
// whose semantic intent is an integer.
type FlexInt int

// UnmarshalJSON accepts a JSON number or a quoted numeric string; null and
// empty-string payloads decode to 0. jsonx.ParseInt64 under StrictAbsentZero
// rejects hex floats, "Inf"/"NaN" and underscore separators, and never
// round-trips through float64. Errors carry a "flexint:" prefix so a log
// reader can tell them apart from plex.RatingKey.Validate's "invalid rating key".
//
//deadset:ignore DS1004,DS1101 -- encoding/json calls it through json.Unmarshaler, which fixes its exported name, when plexapi decodes Plex responses into Episode and HistoryItem.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	*f = 0
	n, err := jsonx.ParseInt64(data, jsonx.StrictAbsentZero())
	if err != nil {
		return fmt.Errorf("flexint: %w", err)
	}
	*f = FlexInt(n)
	return nil
}
