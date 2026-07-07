package notify

import (
	"context"
	"encoding/json"
	"testing"
)

// countingHandler records how many times dispatch invoked each callback so
// the fuzz target can assert the routing invariant, not merely that dispatch
// did not panic.
type countingHandler struct {
	plays     int
	timelines int
}

func (h *countingHandler) OnPlay(context.Context, PlayEvent)           { h.plays++ }
func (h *countingHandler) OnTimeline(context.Context, []TimelineEntry) { h.timelines++ }

// FuzzNotificationUnmarshal feeds arbitrary bytes through the same
// Unmarshal+dispatch path the live read loop uses. Beyond crash-safety it
// asserts the routing invariant: OnPlay fires exactly once per
// PlaySessionStateNotification only for a "playing" envelope, OnTimeline
// fires exactly once only for a "timeline" envelope, and every other
// NotificationContainer.Type is dropped silently. A dispatch regression
// (wrong type routed, timeline fanned out per-entry, unknown type leaking
// through) fails here even on a 2-minute weekly run.
func FuzzNotificationUnmarshal(f *testing.F) {
	f.Add([]byte(`{"NotificationContainer":{"type":"playing","PlaySessionStateNotification":[{"state":"playing"}]}}`))
	f.Add([]byte(`{"NotificationContainer":{"type":"timeline","TimelineEntry":[{"itemID":"1"}]}}`))
	f.Add([]byte(`{"NotificationContainer":{"type":"activity"}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var n Notification
		if err := json.Unmarshal(data, &n); err != nil {
			return
		}
		var rec countingHandler
		dispatch(context.Background(), &rec, &n)

		wantPlays := 0
		if n.NotificationContainer.Type == wsTypePlaying {
			wantPlays = len(n.NotificationContainer.PlaySessionStateNotification)
		}
		if rec.plays != wantPlays {
			t.Errorf("type=%q: OnPlay fired %d times, want %d",
				n.NotificationContainer.Type, rec.plays, wantPlays)
		}
		wantTimelines := 0
		if n.NotificationContainer.Type == wsTypeTimeline {
			wantTimelines = 1
		}
		if rec.timelines != wantTimelines {
			t.Errorf("type=%q: OnTimeline fired %d times, want %d",
				n.NotificationContainer.Type, rec.timelines, wantTimelines)
		}
	})
}
