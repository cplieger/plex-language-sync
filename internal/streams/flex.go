package streams

import (
	"fmt"

	"github.com/cplieger/jsonx"
)

// FlexInt unmarshals a Plex JSON field that may arrive as a number OR
// a quoted numeric string. Plex's JSON responses are famously
// inconsistent on numeric index fields (episode index, parent index,
// library-section ID, account ID): some endpoints return `"14"`,
// others return `14`. json.Number accommodates both but forces every
// reader to call .String() and re-parse, which every call site used
// to do.
//
// FlexInt is the ergonomic replacement: it decodes both shapes into a
// plain int so callers can use the value directly (arithmetic, log
// formatting, comparisons) without a trailing strconv.Atoi step. Null
// and absent JSON fields decode to the zero value, matching the
// previous json.Number behaviour where an absent field produced the
// empty-string "" that strconv.Atoi rejected and callers treated as 0.
//
// Exported (capital F) because non-streams packages (internal/plex for
// Show.LibrarySectionID, Season.Index, HistoryItem.AccountID and
// HistoryItem.LibrarySectionID) now embed FlexInt in their struct
// definitions. Keeping it unexported would force those packages to
// redeclare a mirror type or reach through a getter, both of which
// defeat the "one primitive, one place" design.
//
// Wire-origin string fields (streams.Episode.RatingKey,
// plex.Section.Key, plex.HistoryItem.RatingKey, plex.Show.RatingKey,
// plex.Season.RatingKey) deliberately stay typed as string — the Plex
// JSON wire format for rating keys is a string and inviolate item 9
// requires that representation be preserved on the wire. FlexInt only
// replaces json.Number fields whose semantic intent was always "an
// integer".
type FlexInt int

// UnmarshalJSON accepts either a JSON number (`14`) or a quoted
// numeric string (`"14"`) and decodes it into the underlying int.
// Null and empty-string payloads decode to 0 without error to match
// the previous json.Number-backed behaviour where an absent or null
// field produced the zero-value int through strconv.Atoi("").
//
// The decode is a thin shim over jsonx.ParseInt64 under the
// StrictAbsentZero policy, which was built to reproduce exactly this
// type's pinned behaviour: bare or quoted decimal integers anywhere in
// int64, null/absent/"" tolerated as 0, everything else — float forms,
// non-numeric strings, objects, arrays — a parse error. The library
// additionally hardens the string path: forms strconv would loosely
// accept elsewhere (hex floats, "Inf"/"NaN", underscore separators)
// stay rejected, and large integers never round-trip through float64.
//
// Malformed inputs return a parse error under the "flexint:" prefix.
// The error phrasing deliberately does NOT reuse the "invalid rating
// key" prefix owned by plex.RatingKey.Validate — inviolate item 5
// reserves that exact string for rating-key validation, and conflating
// the two would muddle Loki alerts keyed on rating-key failures.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	*f = 0
	n, err := jsonx.ParseInt64(data, jsonx.StrictAbsentZero())
	if err != nil {
		return fmt.Errorf("flexint: %w", err)
	}
	*f = FlexInt(n)
	return nil
}
