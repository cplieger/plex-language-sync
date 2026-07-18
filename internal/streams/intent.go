package streams

// Intent is the app's own durable record of a user's last deliberately
// observed track selection for a show — captured on the event plane
// (a resolved play session), applied on the reconcile plane (scheduler
// history replay) and when seeding new/updated episodes.
//
// The point of recording intents is that "the user's choice" is an
// event-time fact: Plex's metadata reads expose only an ambient current
// selection whose per-user attribution is not reliable after the fact
// (see the scheduler package doc). An intent captures the choice at the
// only moment it is attributable, so no later code path has to fabricate
// attribution by joining a historical identity with a current read.
//
// JSON tags are part of the on-disk profiles.json schema (inviolate
// contract item 7) — do not change without a migration.
type Intent struct {
	// Subtitle is the observed subtitle selection; nil means the user
	// chose "no subtitles" for this audio, and the policy "no subtitle
	// means no subtitle" applies when the intent is re-applied.
	// (Field ordered first for fieldalignment; JSON schema is keyed by
	// name, not order.)
	Subtitle *IntentStream `json:"subtitle"`
	// Audio is the observed audio selection. Every intent has one: an
	// episode with no selected audio never records an intent.
	Audio IntentStream `json:"audio"`
	// ObservedAt is the unix timestamp of the observation (app clock at
	// the resolved play event).
	ObservedAt int64 `json:"observed_at"`
}

// IntentStream is the persisted projection of a Stream: exactly the
// fields the matchers and scorers consume when the stream is used as a
// REFERENCE (MatchAudio / MatchSubtitle / ScoreAudio / ScoreSubtitle /
// SubtitleCriteria / ShouldSkipSubtitleForCommentary / TitleForMatch).
// Per-episode identity fields (ID, Selected, StreamType) are
// deliberately absent — they are meaningless outside the episode the
// stream was observed on.
type IntentStream struct {
	LanguageCode         string `json:"languageCode"`
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
// Intent. audio must be non-nil (callers gate on a selected audio
// stream before recording); subtitle may be nil ("no subtitles").
func NewIntent(audio, subtitle *Stream, observedAt int64) *Intent {
	return &Intent{
		Audio:      *intentStreamFrom(audio),
		Subtitle:   intentStreamFrom(subtitle),
		ObservedAt: observedAt,
	}
}

// intentStreamFrom projects the matcher-relevant fields of s. nil→nil.
func intentStreamFrom(s *Stream) *IntentStream {
	if s == nil {
		return nil
	}
	return &IntentStream{
		LanguageCode:         s.LanguageCode,
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
func (i *Intent) RefStreams() (audio, subtitle *Stream) {
	return i.Audio.stream(), i.Subtitle.stream()
}

// stream converts the projection back into a Stream carrying only the
// reference-relevant fields. nil→nil.
func (is *IntentStream) stream() *Stream {
	if is == nil {
		return nil
	}
	return &Stream{
		LanguageCode:         is.LanguageCode,
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
// only indirection). Used by the cache to keep exclusive ownership of
// stored values.
func (i *Intent) Clone() Intent {
	out := *i
	if i.Subtitle != nil {
		sub := *i.Subtitle
		out.Subtitle = &sub
	}
	return out
}
