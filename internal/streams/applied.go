package streams

// Source names which decision a write applied. The values are persisted
// in state.json and must not be renumbered. Zero names no decision: a part
// a write left half-applied, which no plan's basis equals.
type Source uint8

// The decisions a write can apply.
const (
	SourceIntent    Source = iota + 1 // the user's own recorded choice
	SourceHousehold                   // another user's recorded choice, seeding only
	SourceProfile                     // the user's learned language profile, seeding only
)

// Basis names what a write applied; two writes with equal bases wrote
// the same decision.
type Basis struct {
	From    string `json:"from,omitempty"` // SourceHousehold: the user whose intent seeded
	Version int64  `json:"v"`
	Source  Source `json:"src"`
}

// Applied is what the app last left on one episode for one user, and on
// what basis. A record whose PartID differs from the episode's current
// part describes a previous file and says nothing about the current one.
type Applied struct {
	Basis      Basis `json:"basis"`
	At         int64 `json:"at"` // unix seconds the record was written
	PartID     int   `json:"part"`
	AudioID    int   `json:"audio"`
	SubtitleID int   `json:"subtitle,omitempty"` // 0 = no subtitle, unless SubtitleUnknown
	// SubtitleUnknown marks a part whose subtitle was not read back after
	// an accepted audio write, where Plex may have picked a new one;
	// SubtitleID is then 0 and names nothing.
	SubtitleUnknown bool `json:"subtitleUnknown,omitempty"`
}
