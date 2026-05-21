package streams

import (
	"fmt"
	"testing"
)

func makeStreams(n int) []*Stream {
	ss := make([]*Stream, n)
	for i := range n {
		ss[i] = &Stream{
			ID:                   i + 1,
			StreamType:           StreamTypeAudio,
			LanguageCode:         "eng",
			Codec:                "aac",
			Channels:             2,
			DisplayTitle:         fmt.Sprintf("English (AAC Stereo) %d", i),
			ExtendedDisplayTitle: fmt.Sprintf("English (AAC Stereo) %d", i),
		}
	}
	return ss
}

func benchBestByScore(b *testing.B, n int) {
	streams := makeStreams(n)
	scoreFn := func(s *Stream) int { return ScoreAudio(streams[0], s) }
	b.ResetTimer()
	for range b.N {
		BestByScore(streams, scoreFn)
	}
}

func BenchmarkBestByScore10(b *testing.B)   { benchBestByScore(b, 10) }
func BenchmarkBestByScore100(b *testing.B)  { benchBestByScore(b, 100) }
func BenchmarkBestByScore1000(b *testing.B) { benchBestByScore(b, 1000) }

func benchMatchAudio(b *testing.B, n int) {
	streams := makeStreams(n)
	ref := streams[0]
	b.ResetTimer()
	for range b.N {
		MatchAudio(ref, streams)
	}
}

func BenchmarkMatchAudio10(b *testing.B)   { benchMatchAudio(b, 10) }
func BenchmarkMatchAudio100(b *testing.B)  { benchMatchAudio(b, 100) }
func BenchmarkMatchAudio1000(b *testing.B) { benchMatchAudio(b, 1000) }
