package streams

import (
	"fmt"
	"strings"
)

// None is the sentinel Desc returns for a nil stream pointer. It lands
// in log attributes like `"audio"="none"` so downstream queries can
// distinguish "no selection" from an absent field.
const None = "none"

// TitleForMatch returns the most-specific non-empty title field on the
// stream, preferring ExtendedDisplayTitle > DisplayTitle > Title. Used
// both for scoring (same title wins) and human-readable descriptions.
func (s *Stream) TitleForMatch() string {
	if s.ExtendedDisplayTitle != "" {
		return s.ExtendedDisplayTitle
	}
	if s.DisplayTitle != "" {
		return s.DisplayTitle
	}
	return s.Title
}

// Desc returns a human-readable description of the stream for log
// output: the best title if any, otherwise "stream-<id>", otherwise
// None for a nil stream.
func Desc(s *Stream) string {
	if s == nil {
		return None
	}
	if t := s.TitleForMatch(); t != "" {
		return t
	}
	return fmt.Sprintf("stream-%d", s.ID)
}

// ID returns s.ID or 0 when s is nil. Used to build stable dedup keys
// from a (possibly absent) current audio/subtitle selection.
func ID(s *Stream) int {
	if s == nil {
		return 0
	}
	return s.ID
}

// descriptiveTerms is the set of keywords that mark an audio track as
// commentary/descriptive. Hoisted to package level for clarity and reuse.
var descriptiveTerms = []string{
	"commentary", "description", "descriptive",
	"narration", "narrative", "described",
}

// ContainsDescriptive reports whether the lowercased title string
// mentions any "commentary" / "descriptive" keyword. Used to filter
// atypical audio tracks out of the default matching path.
func ContainsDescriptive(title string) bool {
	for _, term := range descriptiveTerms {
		if strings.Contains(title, term) {
			return true
		}
	}
	return false
}
