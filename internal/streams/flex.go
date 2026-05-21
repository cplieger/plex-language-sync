package streams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
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
// Malformed inputs (non-numeric strings, floating-point numbers,
// objects, arrays) return a parse error. The error phrasing
// deliberately does NOT reuse the "invalid rating key" prefix owned
// by plex.RatingKey.Validate — inviolate item 5 reserves that exact
// string for rating-key validation, and conflating the two would
// muddle Loki alerts keyed on rating-key failures.
func (f *FlexInt) UnmarshalJSON(data []byte) error {
	// Treat null / absent as zero, matching the pre-flex json.Number
	// semantics where an empty Number string produced 0 via the
	// strconv.Atoi fallback in SeasonNum/EpisodeNum.
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*f = 0
		return nil
	}
	// Quoted numeric string: strip the quotes, then Atoi the body.
	// An empty quoted string ("") also decodes to zero to mirror the
	// json.Number.String() == "" → 0 fallback.
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("flexint: decode string: %w", err)
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("flexint: parse %q: %w", s, err)
		}
		*f = FlexInt(n)
		return nil
	}
	// Bare numeric token: decode via json.Number to tolerate whitespace
	// and sign handling uniformly, then Atoi the textual form. Direct
	// Atoi on the raw bytes would miss json's whitespace rules.
	var num json.Number
	if err := json.Unmarshal(data, &num); err != nil {
		return fmt.Errorf("flexint: decode number: %w", err)
	}
	n, err := strconv.Atoi(num.String())
	if err != nil {
		return fmt.Errorf("flexint: parse %s: %w", num.String(), err)
	}
	*f = FlexInt(n)
	return nil
}
