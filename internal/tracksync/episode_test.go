package tracksync

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/users"
	"github.com/cplieger/plexapi/v3"
)

func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// seedEp is S<season>E<index> of show 1001 (obsEp's keys) with Japanese audio
// marked default, English and French audio, and English and French
// subtitles; English audio and no subtitle are selected.
func seedEp(season, index int) streams.Episode {
	base := 9000 + season*100 + index*10
	jpn := obsAud(base+1, "jpn", "")
	jpn.Default = true
	return obsEp(season, index, []streams.Stream{
		jpn, obsAud(base+2, "eng", ""), obsAud(base+3, "fra", ""),
		obsSub(base+4, "eng"), obsSub(base+5, "fra"),
	}, base+2)
}

// streamOf is the id of ep's stream of kind ("audio" or "subtitle") in lang.
func streamOf(ep *streams.Episode, kind, lang string) int {
	list := streams.Audio(ep)
	if kind == kindSubtitle {
		list = streams.Subtitle(ep)
	}
	for _, st := range list {
		if st.LanguageCode == lang {
			return int(st.ID)
		}
	}
	return 0
}

// cloneEp copies ep down to its streams, so two readers never share a
// selection.
func cloneEp(ep streams.Episode) streams.Episode {
	media := make([]streams.Media, len(ep.Media))
	for i, m := range ep.Media {
		parts := make([]streams.Part, len(m.Part))
		for j, p := range m.Part {
			p.Stream = slices.Clone(p.Stream)
			parts[j] = p
		}
		m.Part = parts
		media[i] = m
	}
	ep.Media = media
	return ep
}

// liveClient is one user's client: an accepted write changes what that
// user's later reads and lists of the part return, as Plex does.
type liveClient struct{ *fakeapi.Plex }

func (c liveClient) SetAudioStream(ctx context.Context, sel plexapi.StreamSelection) error {
	if err := c.Plex.SetAudioStream(ctx, sel); err != nil {
		return err
	}
	c.selectOn(sel.PartID, (*streams.Stream).IsAudio, sel.StreamID)
	return nil
}

func (c liveClient) SetSubtitleStream(ctx context.Context, sel plexapi.StreamSelection) error {
	if err := c.Plex.SetSubtitleStream(ctx, sel); err != nil {
		return err
	}
	c.selectOn(sel.PartID, (*streams.Stream).IsSubtitle, sel.StreamID)
	return nil
}

func (c liveClient) DisableSubtitles(ctx context.Context, partID int) error {
	if err := c.Plex.DisableSubtitles(ctx, partID); err != nil {
		return err
	}
	c.selectOn(partID, (*streams.Stream).IsSubtitle, 0)
	return nil
}

func (c liveClient) selectOn(part int, kind func(*streams.Stream) bool, id int) {
	for _, ep := range c.EpisodeByKey {
		for i := range ep.Media {
			for j := range ep.Media[i].Part {
				p := &ep.Media[i].Part[j]
				if p.ID != part {
					continue
				}
				for k := range p.Stream {
					if st := &p.Stream[k]; kind(st) {
						st.Selected = int(st.ID) == id
					}
				}
			}
		}
	}
}

var _ PlexReadWriter = liveClient{}

// seedLib is show 1001 as the admin reader and each user's own client read
// it, each with its own selections and views, over one cache.
type seedLib struct {
	admin   *fakeapi.Plex
	clients map[string]liveClient
	cache   *fakeapi.Cache
	s       *Syncer
	ids     []string
	clock   int64
}

func newFakePlex() *fakeapi.Plex {
	return &fakeapi.Plex{
		ShowEpisodesByShow:  map[string][]streams.Episode{},
		SeasonEpisodesByKey: map[string][]streams.Episode{},
		EpisodeByKey:        map[string]*streams.Episode{},
	}
}

func newSeedLib(t *testing.T, cfg Config, ids []string, eps ...streams.Episode) *seedLib {
	t.Helper()
	l := &seedLib{admin: newFakePlex(), clients: map[string]liveClient{}, cache: fakeapi.NewCache(), ids: ids, clock: 1000}
	for _, id := range ids {
		l.clients[id] = liveClient{newFakePlex()}
	}
	for i := range eps {
		l.add(eps[i])
	}
	l.build(cfg)
	return l
}

// build replaces the syncer with one under cfg over the same library and
// cache, as a restart with changed settings does.
func (l *seedLib) build(cfg Config) {
	accounts := make([]users.Account, len(l.ids))
	for i, id := range l.ids {
		accounts[i] = users.Account{ID: id}
	}
	l.s = New(cfg, Deps{
		Cache: l.cache, Users: &fakeUsers{AllResult: accounts},
		UserClient: func(id string) PlexReadWriter {
			if c, ok := l.clients[id]; ok {
				return c
			}
			return nil
		},
	})
	l.s.apply.now = func() time.Time { return time.Unix(l.clock, 0) }
}

// add puts ep in the library; every reader gets its own copy.
func (l *seedLib) add(ep streams.Episode) {
	put := func(f *fakeapi.Plex) {
		c := cloneEp(ep)
		f.ShowEpisodesByShow[c.GrandparentRatingKey] = append(f.ShowEpisodesByShow[c.GrandparentRatingKey], c)
		f.SeasonEpisodesByKey[c.ParentRatingKey] = append(f.SeasonEpisodesByKey[c.ParentRatingKey], c)
		f.EpisodeByKey[c.RatingKey] = &c
	}
	put(l.admin)
	for _, c := range l.clients {
		put(c.Plex)
	}
}

// reader is the admin's fake for "admin", else the user's.
func (l *seedLib) reader(user string) *fakeapi.Plex {
	if user == "admin" {
		return l.admin
	}
	return l.clients[user].Plex
}

// selectFor makes exactly the ids in selected the user's selection on ep.
func (l *seedLib) selectFor(user string, ep *streams.Episode, selected ...int) {
	stored := l.reader(user).EpisodeByKey[ep.RatingKey]
	for i := range stored.Media[0].Part[0].Stream {
		st := &stored.Media[0].Part[0].Stream[i]
		st.Selected = slices.Contains(selected, int(st.ID))
	}
}

// viewFor is the user finishing ep at at, a play the app did not see.
func (l *seedLib) viewFor(user string, ep *streams.Episode, at int64) {
	f := l.reader(user)
	f.EpisodeByKey[ep.RatingKey].LastViewedAt = streams.FlexInt(at)
	for _, list := range []([]streams.Episode){f.ShowEpisodesByShow[ep.GrandparentRatingKey], f.SeasonEpisodesByKey[ep.ParentRatingKey]} {
		for i := range list {
			if list[i].RatingKey == ep.RatingKey {
				list[i].LastViewedAt = streams.FlexInt(at)
			}
		}
	}
}

// seed runs SeedEpisode on ep as the admin reads it.
func (l *seedLib) seed(t *testing.T, ep *streams.Episode) {
	t.Helper()
	l.s.SeedEpisode(t.Context(), l.admin.EpisodeByKey[ep.RatingKey], "scan_new")
}

// play is user picking the ids in selected on ep and playing it.
func (l *seedLib) play(t *testing.T, user string, ep *streams.Episode, selected ...int) {
	t.Helper()
	l.selectFor(user, ep, selected...)
	read := cloneEp(*l.reader(user).EpisodeByKey[ep.RatingKey])
	if err := l.s.Observe(t.Context(), user, &read, "play"); err != nil {
		t.Fatalf("Observe = %v, want nil", err)
	}
}

func (l *seedLib) reconcile(t *testing.T, user string, ep *streams.Episode) {
	t.Helper()
	read := cloneEp(*l.reader(user).EpisodeByKey[ep.RatingKey])
	if err := l.s.Reconcile(t.Context(), user, &read, "scheduler"); err != nil {
		t.Fatalf("Reconcile = %v, want nil", err)
	}
}

// writes are the user's accepted writes from index from on.
func (l *seedLib) writes(user string, from int) []fakeapi.Write {
	return l.clients[user].Writes()[from:]
}

func (l *seedLib) basis(user string, ep *streams.Episode) streams.Basis {
	rec, _ := l.cache.AppliedFor(user, ep.RatingKey)
	return rec.Basis
}

// choose records user's choice of audio (and sub, when set) under key.
func choose(c *fakeapi.Cache, user, key, audio, sub string, anchor streams.Anchor, at int64) {
	a := obsAud(0, audio, "")
	pair := streams.Pair{Audio: &a}
	if sub != "" {
		s := obsSub(0, sub)
		pair.Subtitle = &s
	}
	c.RecordIntent(user, key, streams.NewIntent(pair, anchor, at))
}

func anchorOn(season, index int) streams.Anchor {
	ep := obsEp(season, index, nil)
	return streams.Anchor{RatingKey: ep.RatingKey, Season: season, Index: index}
}

func audioWrite(ep *streams.Episode, lang string) fakeapi.Write {
	return fakeapi.Write{Kind: kindAudio, PartID: partOf(ep), StreamID: streamOf(ep, kindAudio, lang)}
}

func subWrite(ep *streams.Episode, lang string) fakeapi.Write {
	return fakeapi.Write{Kind: kindSubtitle, PartID: partOf(ep), StreamID: streamOf(ep, kindSubtitle, lang)}
}

var (
	allShowSeeding  = Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll, SeedNewEpisodes: true}
	allShowProfiles = Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll, LanguageProfiles: true, SeedNewEpisodes: true}
	nextShow        = Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyNext, LanguageProfiles: true, SeedNewEpisodes: true}
)

func profileBasis(audio, sub string) streams.Basis {
	return streams.Basis{Version: profileVersion(streams.ProfileKey(audio), sub), Source: streams.SourceProfile}
}

func TestSeedNeverUsesAdminSelection(t *testing.T) {
	t.Parallel()
	e1, e2 := seedEp(1, 1), seedEp(1, 2)
	tests := []struct {
		name     string
		want     []fakeapi.Write
		profiles bool
	}{
		{"no profile writes nothing", nil, false},
		{"the profile sets only the subtitle", []fakeapi.Write{subWrite(&e2, "eng")}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newSeedLib(t, Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll, LanguageProfiles: tc.profiles, SeedNewEpisodes: true}, []string{"alice"}, e1, e2)
			l.selectFor("admin", &e1, streamOf(&e1, kindAudio, "jpn"), streamOf(&e1, kindSubtitle, "fra"))
			l.selectFor("admin", &e2, streamOf(&e2, kindAudio, "jpn"), streamOf(&e2, kindSubtitle, "fra"))
			l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})

			l.seed(t, &e2)

			if got := l.writes("alice", 0); !slices.Equal(got, tc.want) {
				t.Errorf("alice's writes = %+v, want %+v (the admin's own Japanese and French are no one's choice)", got, tc.want)
			}
		})
	}
}

func TestSeedHouseholdFromRecordedIntent(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowSeeding, []string{"alice", "bob"}, e2)
	choose(l.cache, "bob", obsShow, "jpn", "", streams.Anchor{}, 1000)

	l.seed(t, &e2)

	if got, want := l.writes("alice", 0), []fakeapi.Write{audioWrite(&e2, "jpn")}; !slices.Equal(got, want) {
		t.Fatalf("alice's writes = %+v, want bob's Japanese %+v", got, want)
	}
	if got, want := l.basis("alice", &e2), (streams.Basis{From: "bob", Version: 1000, Source: streams.SourceHousehold}); got != want {
		t.Errorf("alice's record basis = %+v, want %+v", got, want)
	}

	choose(l.cache, "bob", obsShow, "fra", "", streams.Anchor{}, 1100)
	l.clock = 2000
	l.seed(t, &e2)

	if got, want := l.writes("alice", 1), []fakeapi.Write{audioWrite(&e2, "fra")}; !slices.Equal(got, want) {
		t.Errorf("after bob's newer choice alice's writes = %+v, want %+v", got, want)
	}

	choose(l.cache, "alice", obsShow, "eng", "eng", streams.Anchor{}, 1200)
	l.clock = 3000
	l.seed(t, &e2)

	want := []fakeapi.Write{audioWrite(&e2, "eng"), subWrite(&e2, "eng")}
	if got := l.writes("alice", 2); !slices.Equal(got, want) {
		t.Errorf("after alice's own choice her writes = %+v, want %+v", got, want)
	}
	if got, want := l.basis("alice", &e2), (streams.Basis{Version: 1200, Source: streams.SourceIntent}); got != want {
		t.Errorf("alice's record basis = %+v, want her own choice %+v", got, want)
	}
}

func TestSeedProfileDistinctFromHousehold(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	newLib := func(t *testing.T) *seedLib {
		t.Helper()
		l := newSeedLib(t, allShowProfiles, []string{"alice", "bob"}, e2)
		l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})
		l.seed(t, &e2)
		if got, want := l.basis("alice", &e2), profileBasis("jpn", "eng"); got != want {
			t.Fatalf("first seed basis = %+v, want the profile's %+v", got, want)
		}
		l.clock = 2000
		return l
	}

	t.Run("an unchanged profile is not applied twice", func(t *testing.T) {
		t.Parallel()
		l := newLib(t)
		calls := len(l.clients["alice"].CallNames())

		l.seed(t, &e2)

		if got := l.clients["alice"].CallNames()[calls:]; len(got) != 0 {
			t.Errorf("second seed requests = %v, want none", got)
		}
	})
	t.Run("a household choice replaces the profile seed", func(t *testing.T) {
		t.Parallel()
		l := newLib(t)
		choose(l.cache, "bob", obsShow, "jpn", "fra", streams.Anchor{}, 1500)

		l.seed(t, &e2)

		want := []fakeapi.Write{audioWrite(&e2, "jpn"), subWrite(&e2, "fra")}
		if got := l.writes("alice", 1); !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want bob's choice %+v", got, want)
		}
	})
	t.Run("a changed profile is applied again", func(t *testing.T) {
		t.Parallel()
		l := newLib(t)
		l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "fra"})

		l.seed(t, &e2)

		if got, want := l.writes("alice", 1), []fakeapi.Write{subWrite(&e2, "fra")}; !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want the new subtitle %+v", got, want)
		}
		if got, want := l.basis("alice", &e2), profileBasis("jpn", "fra"); got != want {
			t.Errorf("basis = %+v, want %+v", got, want)
		}
	})
}

func TestSeedProfileUsesUserNotAdmin(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowProfiles, []string{"alice"}, e2)
	l.selectFor("admin", &e2, streamOf(&e2, kindAudio, "eng"), streamOf(&e2, kindSubtitle, "eng"))
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})

	l.seed(t, &e2)

	if got, want := l.writes("alice", 0), []fakeapi.Write{subWrite(&e2, "eng")}; !slices.Equal(got, want) {
		t.Errorf("alice's writes = %+v, want %+v (the admin's subtitle already being English says nothing of hers)", got, want)
	}
}

func TestSeedProfileFromDefaultAudio(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowProfiles, []string{"alice"}, e2)
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "eng", Subtitle: "fra"})
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: ""})
	l.selectFor("alice", &e2, streamOf(&e2, kindAudio, "eng"), streamOf(&e2, kindSubtitle, "eng"))

	l.seed(t, &e2)

	want := []fakeapi.Write{{Kind: "subtitle-off", PartID: partOf(&e2)}}
	if got := l.writes("alice", 0); !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want subtitles off from the default Japanese audio's profile %+v", got, want)
	}
}

func TestSeedNoChoiceReadsNothing(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	noAudio := obsEp(1, 3, []streams.Stream{obsSub(9134, "eng")})
	tests := []struct {
		name    string
		ep      streams.Episode
		profile bool
	}{
		{"no record and no profile", e2, false},
		{"a profile for another language", e2, true},
		{"no audio stream", noAudio, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newSeedLib(t, allShowProfiles, []string{"alice"}, tc.ep)
			if tc.profile {
				l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "kor", Subtitle: "eng"})
			}

			l.seed(t, &tc.ep)

			if got := l.clients["alice"].CallNames(); len(got) != 0 {
				t.Errorf("requests = %v, want none", got)
			}
		})
	}
}

func TestSeedProtectsPlayedEpisode(t *testing.T) {
	t.Parallel()
	e1, e2 := seedEp(1, 1), seedEp(1, 2)
	l := newSeedLib(t, allShowSeeding, []string{"alice"}, e1, e2)
	choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
	l.selectFor("alice", &e2, streamOf(&e2, kindAudio, "fra"))
	l.viewFor("alice", &e2, 1500)
	l.clock = 2000

	l.reconcile(t, "alice", &e2)
	l.seed(t, &e2)

	if got, want := l.writes("alice", 0), []fakeapi.Write{audioWrite(&e1, "jpn")}; !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want only E1 put on Japanese %+v (E2 was played after the choice)", got, want)
	}
}

func TestSeedUpdatedSameBasisSkips(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	basis := streams.Basis{Version: 1000, Source: streams.SourceIntent}

	t.Run("a record at the current basis", func(t *testing.T) {
		t.Parallel()
		l := newSeedLib(t, allShowSeeding, []string{"alice"}, e2)
		choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
		l.cache.RecordApplied("alice", e2.RatingKey, streams.Applied{Basis: basis, At: 1500, PartID: partOf(&e2), AudioID: streamOf(&e2, kindAudio, "eng")})

		l.seed(t, &e2)

		if got := l.clients["alice"].CallNames(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})
	t.Run("a replaced file", func(t *testing.T) {
		t.Parallel()
		replaced := cloneEp(e2)
		replaced.Media[0].Part[0].ID = 5999
		l := newSeedLib(t, allShowSeeding, []string{"alice"}, replaced)
		choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
		l.cache.RecordApplied("alice", e2.RatingKey, streams.Applied{Basis: basis, At: 1500, PartID: partOf(&e2), AudioID: streamOf(&e2, kindAudio, "jpn")})
		l.viewFor("alice", &replaced, 3000)
		l.clock = 4000

		l.seed(t, &replaced)

		if got, want := l.writes("alice", 0), []fakeapi.Write{audioWrite(&replaced, "jpn")}; !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want the new file put on Japanese %+v", got, want)
		}
		if rec, _ := l.cache.AppliedFor("alice", e2.RatingKey); rec.PartID != 5999 {
			t.Errorf("record part = %d, want 5999", rec.PartID)
		}
	})
}

func TestSeedOutOfScopeFallsThrough(t *testing.T) {
	t.Parallel()
	e3 := seedEp(1, 3)
	l := newSeedLib(t, nextShow, []string{"alice"}, e3)
	choose(l.cache, "alice", obsShow, "jpn", "", anchorOn(1, 5), 1000)
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})

	l.seed(t, &e3)

	if got, want := l.writes("alice", 0), []fakeapi.Write{subWrite(&e3, "eng")}; !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want the profile's subtitle only %+v", got, want)
	}
	if got, want := l.basis("alice", &e3), profileBasis("jpn", "eng"); got != want {
		t.Errorf("basis = %+v, want %+v", got, want)
	}
}

func TestSeedHouseholdNewestOutOfScopeOlderSeeds(t *testing.T) {
	t.Parallel()
	e5 := seedEp(1, 5)
	tests := []struct {
		name      string
		bobAnchor streams.Anchor
		want      fakeapi.Write
		wantBasis streams.Basis
	}{
		{"the newest choice starts after the episode", anchorOn(1, 9), audioWrite(&e5, "fra"), streams.Basis{From: "carol", Version: 1000, Source: streams.SourceHousehold}},
		{"the newest choice covers the episode", anchorOn(1, 3), audioWrite(&e5, "jpn"), streams.Basis{From: "bob", Version: 1001, Source: streams.SourceHousehold}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newSeedLib(t, nextShow, []string{"alice", "bob", "carol"}, e5)
			choose(l.cache, "bob", obsShow, "jpn", "", tc.bobAnchor, 1001)
			choose(l.cache, "carol", obsShow, "fra", "", anchorOn(1, 1), 1000)

			l.seed(t, &e5)

			if got := l.writes("alice", 0); !slices.Equal(got, []fakeapi.Write{tc.want}) {
				t.Errorf("alice's writes = %+v, want %+v", got, tc.want)
			}
			if got := l.basis("alice", &e5); got != tc.wantBasis {
				t.Errorf("basis = %+v, want %+v", got, tc.wantBasis)
			}
		})
	}
}

func TestSeedHouseholdSkipsSupersededRecord(t *testing.T) {
	t.Parallel()
	s2e5 := seedEp(2, 5)
	cfg := Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyNext, LanguageProfiles: true, SeedNewEpisodes: true}
	tests := []struct {
		name      string
		wantBasis streams.Basis
		want      []fakeapi.Write
		carol     bool
	}{
		{"an older choice that still governs seeds", streams.Basis{From: "carol", Version: 999, Source: streams.SourceHousehold}, []fakeapi.Write{audioWrite(&s2e5, "fra")}, true},
		{"with none, the profile seeds", profileBasis("jpn", "eng"), []fakeapi.Write{subWrite(&s2e5, "eng")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newSeedLib(t, cfg, []string{"alice", "bob", "carol"}, s2e5)
			choose(l.cache, "bob", obsShow, "jpn", "", anchorOn(1, 1), 1000)
			choose(l.cache, "bob", "1001/s2", "eng", "", anchorOn(2, 9), 1001)
			if tc.carol {
				choose(l.cache, "carol", obsShow, "fra", "", anchorOn(1, 1), 999)
			}
			l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})

			l.seed(t, &s2e5)

			if got := l.writes("alice", 0); !slices.Equal(got, tc.want) {
				t.Errorf("alice's writes = %+v, want %+v (bob's show choice no longer governs his season 2)", got, tc.want)
			}
			if got := l.basis("alice", &s2e5); got != tc.wantBasis {
				t.Errorf("basis = %+v, want %+v", got, tc.wantBasis)
			}
		})
	}
}

func TestSeedEachUserThroughOwnClient(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowSeeding, []string{"alice", "bob"}, e2)
	choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
	choose(l.cache, "bob", obsShow, "fra", "", streams.Anchor{}, 1001)

	l.seed(t, &e2)

	if got, want := l.writes("alice", 0), []fakeapi.Write{audioWrite(&e2, "jpn")}; !slices.Equal(got, want) {
		t.Errorf("alice's writes = %+v, want her Japanese %+v", got, want)
	}
	if got, want := l.writes("bob", 0), []fakeapi.Write{audioWrite(&e2, "fra")}; !slices.Equal(got, want) {
		t.Errorf("bob's writes = %+v, want his French %+v", got, want)
	}
	if got := l.admin.Writes(); len(got) != 0 {
		t.Errorf("admin client writes = %+v, want none", got)
	}
}

// Not parallel: it swaps the process-global default logger.
func TestSeedSkipsUserWithNilClient(t *testing.T) {
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowSeeding, []string{"alice", "carol"}, e2)
	delete(l.clients, "carol")
	choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
	choose(l.cache, "carol", obsShow, "jpn", "", streams.Anchor{}, 1000)
	buf := captureLogs(t)

	l.seed(t, &e2)

	if got := l.writes("alice", 0); len(got) != 1 {
		t.Errorf("alice's writes = %+v, want one", got)
	}
	if got := l.admin.Writes(); len(got) != 0 {
		t.Errorf("admin client writes = %+v, want none: a user without a client is skipped, never written as the admin", got)
	}
	if out := buf.String(); !strings.Contains(out, "sync: no per-user client, skipping user") || !strings.Contains(out, "user_id=carol") {
		t.Errorf("log = %q, want a warning skipping user_id=carol", out)
	}
}

func TestSeedStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	l := newSeedLib(t, allShowProfiles, []string{"alice"}, e2)
	choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled on purpose; Background, not t.Context()

	l.s.SeedEpisode(ctx, l.admin.EpisodeByKey[e2.RatingKey], "scan_new")

	if got := l.clients["alice"].CallNames(); len(got) != 0 {
		t.Errorf("requests = %v, want none on a cancelled context", got)
	}
}

func TestSeedEpisodeOffIsNoop(t *testing.T) {
	t.Parallel()
	e2 := seedEp(1, 2)
	off := allShowProfiles
	off.SeedNewEpisodes = false
	l := newSeedLib(t, off, []string{"alice", "bob"}, e2)
	choose(l.cache, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000)
	choose(l.cache, "bob", obsShow, "fra", "", streams.Anchor{}, 1100)
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "eng", Subtitle: "eng"})

	l.seed(t, &e2)

	for _, user := range []string{"alice", "bob"} {
		if got := l.clients[user].CallNames(); len(got) != 0 {
			t.Errorf("%s's requests = %v, want none with seeding off", user, got)
		}
		if rec, ok := l.cache.AppliedFor(user, e2.RatingKey); ok {
			t.Errorf("%s's record = %+v, want none with seeding off", user, rec)
		}
	}
}

// Not parallel: it swaps the process-global default logger.
func TestSeedLogsEachSource(t *testing.T) {
	e2 := seedEp(1, 2)
	tests := []struct {
		name   string
		setup  func(c *fakeapi.Cache)
		msg    string
		attrs  map[string]string
		silent bool
	}{
		{
			name:  "own choice",
			setup: func(c *fakeapi.Cache) { choose(c, "alice", obsShow, "jpn", "", streams.Anchor{}, 1000) },
			msg:   "new/updated episode language set",
			attrs: map[string]string{"source": "intent", "trigger": "scan_new"},
		},
		{
			name:  "another user's choice",
			setup: func(c *fakeapi.Cache) { choose(c, "bob", obsShow, "jpn", "", streams.Anchor{}, 1000) },
			msg:   "new/updated episode language set",
			attrs: map[string]string{"source": "household", "from": "user-bob"},
		},
		{
			name: "the profile",
			setup: func(c *fakeapi.Cache) {
				c.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: "jpn", Subtitle: "eng"})
			},
			msg:   "language profile applied to new show",
			attrs: map[string]string{"audio_lang": "jpn", "subtitle_lang": "eng"},
		},
		{
			name: "a seed that changed nothing",
			setup: func(c *fakeapi.Cache) {
				choose(c, "alice", obsShow, "eng", "", streams.Anchor{}, 1000)
			},
			msg:    "new/updated episode language set",
			silent: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newSeedLib(t, allShowProfiles, []string{"alice"}, e2)
			tc.setup(l.cache)
			rec := capture.Default(t)

			l.seed(t, &e2)

			for key, want := range tc.attrs {
				if !rec.HasAttr(capture.Message(tc.msg), capture.AttrName(key), want) {
					t.Errorf("%q with %s=%q not logged: %v", tc.msg, key, want, rec.Records())
				}
			}
			if tc.silent && rec.Contains(tc.msg) {
				t.Errorf("%q logged for a seed that wrote nothing: %v", tc.msg, rec.Records())
			}
		})
	}
}

// An unreadable language code is learned and matched verbatim, so a
// malformed file tag reaches the profile-applied record.
//
// Not parallel: it swaps the process-global default logger.
func TestSeedProfileSanitizesLanguageFieldsInLog(t *testing.T) {
	const (
		hostileAudio = "jpn\nlevel=ERROR \u202ea"
		hostileSub   = "eng\nlevel=ERROR \u202es"
	)
	ep := obsEp(1, 2, []streams.Stream{obsAud(9121, hostileAudio, ""), obsSub(9124, hostileSub)})
	l := newSeedLib(t, allShowProfiles, []string{"alice"}, ep)
	l.cache.LearnLanguageProfile("alice", streams.LanguageChoice{Audio: hostileAudio, Subtitle: hostileSub})
	rec := capture.Default(t)

	l.seed(t, &ep)

	for key, raw := range map[string]string{"audio_lang": hostileAudio, "subtitle_lang": hostileSub} {
		want := textsafe.SanitizeSingleLine(raw)
		if !rec.HasAttr("language profile applied", capture.AttrName(key), want) {
			t.Errorf("%s was not logged as %q; a learned language comes from the media file and reached the log raw: %v", key, want, rec.Records())
		}
	}
}
