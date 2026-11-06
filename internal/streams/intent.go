package streams

// Intent is the app's durable record of a user's last observed track
// selection for a show, captured on the event plane at play time: a later
// library read cannot tell the user's pick from a write this app made
// afterwards or from Plex's default.
//
// JSON tags are the profiles.json schema. Fields are additive; an intent
// without "anchor" loads with a zero anchor.
type Intent struct {
	// Subtitle nil means the user chose "no subtitles" for this audio.
	Subtitle *intentStream `json:"subtitle"`
	// Audio is never absent: an episode with no selected audio records no intent.
	Audio      intentStream `json:"audio"`
	Anchor     Anchor       `json:"anchor,omitzero"`
	ObservedAt int64        `json:"observed_at"`
}

// Anchor is the episode and the stream ids the choice was observed on.
type Anchor struct {
	RatingKey  string `json:"ratingKey"`
	Season     int    `json:"season"`
	Index      int    `json:"index"`
	AudioID    int    `json:"audioID"`
	SubtitleID int    `json:"subtitleID,omitempty"` // 0 = no subtitle
}

// IsZero reports an anchor absent from the record, as on an intent
// written before the field existed.
func (a Anchor) IsZero() bool { return a.RatingKey == "" }

// AnchorOf anchors sel, the selection observed on ep. A nil sel.Subtitle
// records subtitle id 0, "no subtitle".
func AnchorOf(ep *Episode, sel Pair) Anchor {
	a := Anchor{RatingKey: ep.RatingKey, Season: ep.SeasonNum(), Index: ep.Num()}
	if sel.Audio != nil {
		a.AudioID = int(sel.Audio.ID)
	}
	if sel.Subtitle != nil {
		a.SubtitleID = int(sel.Subtitle.ID)
	}
	return a
}

// intentStream is the persisted projection of a Stream: exactly the
// fields the matchers and scorers consume when the stream is used as a
// reference. Per-episode identity fields (ID, Selected, StreamType) are
// deliberately absent — they are meaningless outside the episode the
// stream was observed on.
type intentStream struct {
	LanguageCode string `json:"languageCode"`
	// LanguageTag is Plex's BCP 47 tag. Additive and omitempty so an intent
	// written before this field existed still loads, falling back to the
	// coarser LanguageCode.
	LanguageTag          string `json:"languageTag,omitempty"`
	Title                string `json:"title,omitempty"`
	DisplayTitle         string `json:"displayTitle,omitempty"`
	ExtendedDisplayTitle string `json:"extendedDisplayTitle,omitempty"`
	Codec                string `json:"codec,omitempty"`
	AudioChannelLayout   string `json:"audioChannelLayout,omitempty"`
	Channels             int    `json:"channels,omitempty"`
	Forced               bool   `json:"forced,omitempty"`
	HearingImpaired      bool   `json:"hearingImpaired,omitempty"`
	VisualImpaired       bool   `json:"visualImpaired,omitempty"`
}

// NewIntent projects an observed (audio, subtitle) selection into an
// Intent. ref.Audio must be non-nil; ref.Subtitle may be nil ("no subtitles").
func NewIntent(ref Pair, anchor Anchor, observedAt int64) *Intent {
	return &Intent{
		Audio:      *intentStreamFrom(ref.Audio),
		Subtitle:   intentStreamFrom(ref.Subtitle),
		Anchor:     anchor,
		ObservedAt: observedAt,
	}
}

// intentStreamFrom projects the matcher-relevant fields of s.
func intentStreamFrom(s *Stream) *intentStream {
	if s == nil {
		return nil
	}
	return &intentStream{
		LanguageCode:         s.LanguageCode,
		LanguageTag:          s.LanguageTag,
		Title:                s.Title,
		DisplayTitle:         s.DisplayTitle,
		ExtendedDisplayTitle: s.ExtendedDisplayTitle,
		Codec:                s.Codec,
		AudioChannelLayout:   s.AudioChannelLayout,
		Channels:             s.Channels,
		Forced:               s.Forced,
		HearingImpaired:      s.HearingImpaired,
		VisualImpaired:       s.VisualImpaired,
	}
}

// RefStreams reconstructs reference *Stream values for the matchers
// from the persisted projection. The audio return is always non-nil;
// the subtitle return is nil when the intent recorded "no subtitles".
func (i *Intent) RefStreams() Pair {
	return Pair{Audio: i.Audio.stream(), Subtitle: i.Subtitle.stream()}
}

// stream converts the projection back into a Stream carrying only the
// reference-relevant fields.
func (is *intentStream) stream() *Stream {
	if is == nil {
		return nil
	}
	return &Stream{
		LanguageCode:         is.LanguageCode,
		LanguageTag:          is.LanguageTag,
		Title:                is.Title,
		DisplayTitle:         is.DisplayTitle,
		ExtendedDisplayTitle: is.ExtendedDisplayTitle,
		Codec:                is.Codec,
		AudioChannelLayout:   is.AudioChannelLayout,
		Channels:             is.Channels,
		Forced:               is.Forced,
		HearingImpaired:      is.HearingImpaired,
		VisualImpaired:       is.VisualImpaired,
	}
}

// Clone returns a deep copy of the intent (the Subtitle pointer is the
// only indirection), so the cache keeps exclusive ownership of stored values.
func (i *Intent) Clone() Intent {
	out := *i
	if i.Subtitle != nil {
		sub := *i.Subtitle
		out.Subtitle = &sub
	}
	return out
}
