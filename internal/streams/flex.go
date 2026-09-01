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

// UnmarshalJSON accepts either a JSON number or a quoted numeric string.
// Null and empty-string payloads decode to 0 without error.
//
// Delegates to jsonx.ParseInt64 under StrictAbsentZero, which hardens
// the string path beyond strconv (rejects hex floats, "Inf"/"NaN",
// underscore separators; no float64 round-trip for large integers).
//
// The "flexint:" error prefix must not be confused with
// plex.RatingKey.Validate's "invalid rating key" prefix — Loki alerts
// key on rating-key failures by that exact string.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	*f = 0
	n, err := jsonx.ParseInt64(data, jsonx.StrictAbsentZero())
	if err != nil {
		return fmt.Errorf("flexint: %w", err)
	}
	*f = FlexInt(n)
	return nil
}
