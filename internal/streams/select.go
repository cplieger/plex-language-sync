package streams

// firstPart returns the first media part of an episode, or nil when the
// episode has no media or parts. It centralizes the Media[0].Part[0]
// navigation every selection helper below depends on.
func firstPart(ep *Episode) *Part {
	if ep == nil || len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil
	}
	return &ep.Media[0].Part[0]
}

// streamsByType returns the streams from the first part of the first
// media that satisfy keep. Returns nil when the episode has no
// media/parts.
func streamsByType(ep *Episode, keep func(*Stream) bool) []*Stream {
	p := firstPart(ep)
	if p == nil {
		return nil
	}
	var out []*Stream
	for i := range p.Stream {
		s := &p.Stream[i]
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// Selected returns the currently-selected audio and subtitle streams
// from the first part of the first media of an episode. Either return
// may be nil if no stream of that type is marked selected (or if the
// episode has no media/parts at all).
func Selected(ep *Episode) (audio, subtitle *Stream) {
	p := firstPart(ep)
	if p == nil {
		return nil, nil
	}
	for i := range p.Stream {
		s := &p.Stream[i]
		if s.IsAudio() && s.Selected {
			audio = s
		}
		if s.IsSubtitle() && s.Selected {
			subtitle = s
		}
	}
	return audio, subtitle
}

// Audio returns all audio streams from the first part of the first
// media. Returns nil when the episode has no parts.
func Audio(ep *Episode) []*Stream {
	return streamsByType(ep, (*Stream).IsAudio)
}

// Subtitle returns all subtitle streams from the first part of the
// first media. Returns nil when the episode has no parts.
func Subtitle(ep *Episode) []*Stream {
	return streamsByType(ep, (*Stream).IsSubtitle)
}

// FirstPartID returns the part ID of the first media part, or 0 when
// the episode has no media/parts.
func FirstPartID(ep *Episode) int {
	p := firstPart(ep)
	if p == nil {
		return 0
	}
	return p.ID
}
