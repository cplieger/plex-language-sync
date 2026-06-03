package plex

import (
	"encoding/json"
	"encoding/xml"
	"strconv"
	"testing"

	"plex-language-sync/internal/streams"
)

func FuzzSharedServersXMLUnmarshal(f *testing.F) {
	f.Add([]byte(`<MediaContainer><SharedServer userID="1" username="a" accessToken="t"/></MediaContainer>`))
	f.Add([]byte(`<MediaContainer></MediaContainer>`))
	f.Add([]byte(`not xml`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var v SharedServersXML
		_ = xml.Unmarshal(data, &v)
	})
}

func FuzzEpisodeUnmarshal(f *testing.F) {
	f.Add([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"1","index":1,"parentIndex":2}]}}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var env mc[struct {
			Metadata []streams.Episode `json:"Metadata"`
		}]
		if err := json.Unmarshal(data, &env); err != nil {
			return
		}
		for i := range env.MediaContainer.Metadata {
			_ = env.MediaContainer.Metadata[i].SeasonNum()
			_ = env.MediaContainer.Metadata[i].EpisodeNum()
			_ = env.MediaContainer.Metadata[i].ShortName()
		}
	})
}

func FuzzRatingKeyValidate(f *testing.F) {
	f.Add("12345")
	f.Add("")
	f.Add("abc")
	f.Add("-1")

	f.Fuzz(func(t *testing.T, s string) {
		rk := RatingKey(s)
		err := rk.Validate()
		if err == nil {
			if _, atoiErr := strconv.Atoi(s); atoiErr != nil {
				t.Fatalf("Validate returned nil but Atoi fails for %q", s)
			}
		}
	})
}
