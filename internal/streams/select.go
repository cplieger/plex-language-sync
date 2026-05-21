package streams

// Selected returns the currently-selected audio and subtitle streams
// from the first part of the first media of an episode. Either return
// may be nil if no stream of that type is marked selected (or if the
// episode has no media/parts at all).
func Selected(ep *Episode) (audio, subtitle *Stream) {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil, nil
	}
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
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
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil
	}
	var out []*Stream
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
		if s.IsAudio() {
			out = append(out, s)
		}
	}
	return out
}

// Subtitle returns all subtitle streams from the first part of the
// first media. Returns nil when the episode has no parts.
func Subtitle(ep *Episode) []*Stream {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return nil
	}
	var out []*Stream
	for i := range ep.Media[0].Part[0].Stream {
		s := &ep.Media[0].Part[0].Stream[i]
		if s.IsSubtitle() {
			out = append(out, s)
		}
	}
	return out
}

// FirstPartID returns the part ID of the first media part, or 0 when
// the episode has no media/parts.
func FirstPartID(ep *Episode) int {
	if len(ep.Media) == 0 || len(ep.Media[0].Part) == 0 {
		return 0
	}
	return ep.Media[0].Part[0].ID
}
