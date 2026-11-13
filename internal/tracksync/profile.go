package tracksync

import (
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// profilePlan is userID's learned profile for ep's default audio language,
// or nil when ep has no audio or the user has no profile for its language.
// The admin read is only ep's inventory: the plan reads the user's own
// selection before it writes.
func (s *Syncer) profilePlan(userID string, ep *streams.Episode) (*plan, seedSource) {
	audio := streams.DefaultAudio(ep)
	if audio == nil || audio.LanguageCode == "" {
		return nil, seedSource{}
	}
	subLang, ok := s.cache.SubtitleLangForAudio(userID, audio.LanguageCode)
	if !ok {
		return nil, seedSource{}
	}
	return &plan{
		pick:  pickForProfile(subLang, s.cfg.SubtitleFloor),
		basis: streams.Basis{Version: profileVersion(streams.ProfileKey(audio.LanguageCode), subLang), Source: streams.SourceProfile},
	}, seedSource{
		audio: audio.LanguageCode, subtitle: subLang, tier: streams.SourceProfile,
	}
}
