package fakeapi

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Plex implements api.PlexReadWriter for tests. All methods are
// concurrency-safe. Fields are exported for direct test setup.
type Plex struct {
	UserFromSessionResult struct {
		Err      error
		UserID   string
		Username string
	}
	EpisodeErr          error
	SetAudioErr         error
	SetSubtitleErr      error
	DisableErr          error
	ShowEpisodesByShow  map[string][]streams.Episode
	EpisodeByKey        map[string]*streams.Episode
	SeasonEpisodesByKey map[string][]streams.Episode
	ShowMetadataByKey   map[string]*plex.Show
	RecentlyAddedBySec  map[string][]streams.Episode
	HistoryItems        []plex.HistoryItem
	Sections            []plex.Section
	callNames           []string
	Calls               atomic.Int64
	mu                  sync.Mutex
}

// CallNames returns a copy of the ordered call log.
func (f *Plex) CallNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.callNames))
	copy(out, f.callNames)
	return out
}

// Episode returns the episode identified by key, or plex.ErrNotFound if
// EpisodeByKey has no matching entry.
func (f *Plex) Episode(_ context.Context, key plex.RatingKey) (*streams.Episode, error) {
	f.record("Episode:" + key.String())
	if f.EpisodeErr != nil {
		return nil, f.EpisodeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ep, ok := f.EpisodeByKey[key.String()]
	if !ok {
		return nil, plex.ErrNotFound
	}
	return ep, nil
}

// ShowEpisodes returns all episodes stored under showRatingKey in ShowEpisodesByShow.
func (f *Plex) ShowEpisodes(_ context.Context, showRatingKey plex.RatingKey) ([]streams.Episode, error) {
	f.record("ShowEpisodes:" + showRatingKey.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ShowEpisodesByShow[showRatingKey.String()], nil
}

// SeasonEpisodes returns all episodes stored under key in SeasonEpisodesByKey.
func (f *Plex) SeasonEpisodes(_ context.Context, key plex.RatingKey) ([]streams.Episode, error) {
	f.record("SeasonEpisodes:" + key.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SeasonEpisodesByKey[key.String()], nil
}

// ShowMetadata returns the show metadata stored under key in ShowMetadataByKey,
// or plex.ErrNotFound if no entry exists.
func (f *Plex) ShowMetadata(_ context.Context, key plex.RatingKey) (*plex.Show, error) {
	f.record("ShowMetadata:" + key.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	show, ok := f.ShowMetadataByKey[key.String()]
	if !ok {
		return nil, plex.ErrNotFound
	}
	return show, nil
}

// RecentlyAdded returns the recently-added episodes stored under sectionKey in RecentlyAddedBySec.
func (f *Plex) RecentlyAdded(_ context.Context, sectionKey plex.RatingKey, _ int64) ([]streams.Episode, error) {
	f.record("RecentlyAdded:" + sectionKey.String())
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.RecentlyAddedBySec[sectionKey.String()], nil
}

// History returns the HistoryItems slice configured on the fake.
func (f *Plex) History(_ context.Context, _ int64) ([]plex.HistoryItem, error) {
	f.record("History")
	return f.HistoryItems, nil
}

// ShowSections returns the Sections slice configured on the fake.
func (f *Plex) ShowSections(_ context.Context) ([]plex.Section, error) {
	f.record("ShowSections")
	return f.Sections, nil
}

// UserFromSession returns the userID, username, and error configured in UserFromSessionResult.
func (f *Plex) UserFromSession(_ context.Context, _ string) (userID, username string, err error) {
	return f.UserFromSessionResult.UserID, f.UserFromSessionResult.Username, f.UserFromSessionResult.Err
}

// SetAudioStream records the call and returns SetAudioErr.
func (f *Plex) SetAudioStream(_ context.Context, _, _ int) error {
	f.record("SetAudio")
	return f.SetAudioErr
}

// SetSubtitleStream records the call and returns SetSubtitleErr.
func (f *Plex) SetSubtitleStream(_ context.Context, _, _ int) error {
	f.record("SetSubtitle")
	return f.SetSubtitleErr
}

// DisableSubtitles records the call and returns DisableErr.
func (f *Plex) DisableSubtitles(_ context.Context, _ int) error {
	f.record("DisableSubtitle")
	return f.DisableErr
}

func (f *Plex) record(name string) {
	f.Calls.Add(1)
	f.mu.Lock()
	f.callNames = append(f.callNames, name)
	f.mu.Unlock()
}

var _ api.PlexReadWriter = (*Plex)(nil)
