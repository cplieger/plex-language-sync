package notify

import (
	"context"
	"encoding/json"
	"testing"
)

// nopHandler is a no-op Handler for fuzz dispatch safety.
type nopHandler struct{}

func (nopHandler) OnPlay(_ context.Context, _ PlayEvent)         {}
func (nopHandler) OnTimeline(_ context.Context, _ []TimelineEntry) {}

func FuzzNotificationUnmarshal(f *testing.F) {
	f.Add([]byte(`{"NotificationContainer":{"type":"playing","PlaySessionStateNotification":[{"state":"playing"}]}}`))
	f.Add([]byte(`{"NotificationContainer":{"type":"timeline","TimelineEntry":[{"itemID":"1"}]}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var n Notification
		if err := json.Unmarshal(data, &n); err != nil {
			return
		}
		dispatch(context.Background(), nopHandler{}, &n)
	})
}
