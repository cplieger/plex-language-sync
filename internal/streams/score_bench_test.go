package streams

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plexapi/v2"
)

// This file measures what stream selection COSTS, and it holds two kinds of
// check doing different jobs. The split exists because the repo is enrolled in a
// weekly benchmark tracker and the tracker cannot see the regression class that
// matters most here.
//
//   - The Benchmark* series feed that tracker. It compares each series against
//     its previous value and alerts above a ratio, so it catches a change that
//     multiplies cost and is blind to one that adds to it: 425 allocations
//     becoming 500 is a ratio of 1.18 and passes silently. The one shape it
//     always catches is zero becoming non-zero, which divides to Infinity.
//   - The Test* allocation contracts gate what the chart cannot express. Two
//     properties live here and nowhere else: the per-candidate RATE, which a
//     single charted number cannot separate from the fixed floor it includes,
//     and SIZE INDEPENDENCE, which is a claim about every candidate count at
//     once while the chart records one.
//
// Why the rate is the valuable half: this app propagates a language selection
// across every episode of every show, so a function's per-candidate cost is
// multiplied by the track count of an entire media library. One extra
// allocation per track is invisible in a ratio and is the difference between a
// library-wide pass that allocates thousands of times and one that allocates
// tens of thousands.
//
// # How a rate contract is written here, and why it is not an equality
//
// Each sweep measures the allocation count at three candidate counts, takes the
// slope between the smallest and the largest, and asserts it is at most a stated
// ceiling. Three deliberate choices:
//
//   - A slope, not an absolute count, because every absolute here includes a
//     fixed per-call floor (the reference's own language parse, the generic
//     Best call) that a rate cancels out. The floor also moves with the
//     toolchain; the rate is the library's own behaviour.
//   - A ceiling, not an equality, because the measured slopes carry a small
//     residue from that floor (4.02 rather than 4.00) and because a toolchain
//     change may shift an absolute count without changing what the code does.
//     The ceilings sit about half an allocation above the measured value, which
//     is loose enough to absorb that residue and tight enough that one added
//     allocation per candidate (a slope shift of 1.0) fails.
//   - Three sizes, with every adjacent rate logged, so a super-linear
//     regression reads as a RISING rate rather than as one larger number. The
//     asserted 10-to-1000 slope alone could not tell quadratic growth from a
//     bigger constant.
//
// Every measured number in this file was read off this package before the
// assertion was written; no ceiling was picked to make a test pass.
//
// # The fixture rules these tests depend on
//
// Two preconditions, both verified rather than assumed, because either one
// silently corrupts an AllocsPerRun measurement:
//
//   - Every fixture is built OUTSIDE the measured closure. A candidate slice or
//     a Stream allocated inside the closure is counted, which would make a
//     genuinely allocation-free function look like it allocates.
//   - No function measured here mutates its candidate slice. AllocsPerRun calls
//     the closure many times, so a function that sorted or appended in place
//     would be measuring a different input on every call after the first.
//     TestSelectionDoesNotMutateItsCandidateSlice and
//     TestMatchingDoesNotMutateItsCandidateSlice pin this for both halves of
//     the package; the sweeps rely on it.
//
// Each sweep also asserts its own REGIME before measuring, the way a matcher
// fixture has to: a candidate set that stops matching (or starts matching)
// takes a different path through MatchAudio, and without the check the case
// would quietly chart the early-out instead of the work.

// costRuns is the AllocsPerRun run count for every contract in this file and in
// match_bench_test.go. Spot-checked not to matter: BestByScore at every score
// distribution, MatchAudio at 100 plain candidates and MatchAudio at 1000
// region-tagged candidates all report the same count at 10, 50, 100 and 500
// runs, so the number is a cost choice rather than a tuning knob.
const costRuns = 100

// costSizes is the candidate sweep every rate contract walks. The span is 100x,
// which is wide enough that a per-candidate allocation cannot hide inside the
// fixed floor, and the two adjacent gaps let a rising rate distinguish
// super-linear growth from a larger constant.
var costSizes = []int{10, 100, 1000}

func makeStreams(n int) []*Stream {
	ss := make([]*Stream, n)
	for i := range n {
		ss[i] = &Stream{
			ID:                   plexapi.FlexInt(i + 1),
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

// costStreams builds n audio candidates in the shape the benchmarks below use
// and applies mut to each, so one sweep can vary the content class that decides
// which path through the matcher runs. The class is the whole point: an untagged
// candidate set skips the language parse entirely and costs a quarter of what a
// tagged one does, so a single fixture would pin only one of the paths a real
// library exercises.
func costStreams(n int, mut func(i int, s *Stream)) []*Stream {
	ss := makeStreams(n)
	if mut != nil {
		for i, s := range ss {
			mut(i, s)
		}
	}
	return ss
}

// costSubtitles builds n subtitle candidates carrying a text codec, the shape
// MatchSubtitle and FindSubtitleByLanguage take. mut applies after the subtitle
// fields are set so a class can still override the codec or a flag.
func costSubtitles(n int, mut func(i int, s *Stream)) []*Stream {
	return costStreams(n, func(i int, s *Stream) {
		s.StreamType = StreamTypeSubtitle
		s.Codec = "srt"
		if mut != nil {
			mut(i, s)
		}
	})
}

// costTitle returns a title of exactly bytes bytes carrying mixed case, so
// strings.ToLower on it cannot short-circuit. Length is a parameter because the
// independence of allocation COUNT from title length is itself a contract; see
// TestMatchAudioAllocationCountIsIndependentOfTitleLength.
func costTitle(bytes int) string {
	const unit = "VeryVerboseUpstreamMetadata "
	return strings.Repeat(unit, bytes/len(unit)+1)[:bytes]
}

// streamValues dereferences a candidate slice into its Stream values, which is
// what a non-mutation check compares. Stream holds only comparable fields, so
// slices.Equal over the values catches a field write, and because distinct
// candidates carry distinct IDs and titles it catches a REORDERING too — an
// in-place sort moves the values along with the pointers.
func streamValues(ss []*Stream) []Stream {
	out := make([]Stream, len(ss))
	for i, s := range ss {
		out[i] = *s
	}
	return out
}

// TestBestByScoreAllocatesNothingAtEverySize is the one contract here that is an
// equality rather than a ceiling, and its value is the word EVERY.
//
// BestByScore is the final tie-break of both matchers and the whole of
// FindSubtitleByLanguage, so it runs once per episode per user. It allocates
// nothing because it returns an element of the slice it was handed rather than
// building a result: it tracks the best index and score in two locals and
// returns streams[bestIdx]. A change that collected the winners into a slice,
// or that boxed a score, would put one allocation on every episode of every
// show — and the weekly chart WOULD catch that at the sizes it charts, because
// zero becoming non-zero divides to Infinity and alerts at any threshold.
//
// What the chart cannot say is that the property holds at every size. It
// records one number per series, so it cannot distinguish "allocates nothing"
// from "allocates nothing at 100 candidates". The sweep below spans the empty
// slice to a thousand candidates, which is what makes this an invariant rather
// than a data point.
//
// The span is not ceremony, and this was measured rather than assumed: a result
// slice added to this function (make([]*Stream, 0, len(streams))) does NOT
// register at all up to 8 candidates, because the compiler keeps a small
// non-escaping slice of dynamic size on the stack and only reaches the allocator
// past roughly 64 bytes. A contract that checked one small size would have
// passed that break clean.
//
// The scoreFn shapes are here because the count could plausibly depend on the
// data and does not. A winner in the last position walks the whole slice; an
// all-tied set never updates the best index; an all-negative set is the case a
// naive implementation seeded with a zero floor would get wrong, and it is the
// one that would need a result slice if the seeding were done differently.
func TestBestByScoreAllocatesNothingAtEverySize(t *testing.T) {
	t.Run("across candidate counts", func(t *testing.T) {
		for _, n := range []int{0, 1, 10, 100, 1000} {
			streams := costStreams(n, nil)
			ref := &Stream{LanguageCode: "eng", Codec: "aac", Channels: 2}
			scoreFn := func(s *Stream) int { return ScoreAudio(ref, s) }
			if got := testing.AllocsPerRun(costRuns, func() {
				_ = BestByScore(streams, scoreFn)
			}); got != 0 {
				t.Errorf("BestByScore(%d candidates, ScoreAudio) allocated %v times per run, want 0: it returns an element of the slice it was given, so a non-zero count means a result is being built on a path that runs once per episode of every show in the library",
					n, got)
			}
		}
	})

	// A nil slice rather than an empty one: len(nil) is 0 so the guard is the
	// same branch, but a caller reaching BestByScore through FilterByLanguage
	// gets nil and not an empty slice when nothing matched, so nil is the shape
	// production actually passes.
	t.Run("a nil candidate slice", func(t *testing.T) {
		var streams []*Stream
		if got := testing.AllocsPerRun(costRuns, func() {
			_ = BestByScore(streams, func(_ *Stream) int { return 1 })
		}); got != 0 {
			t.Errorf("BestByScore(nil, scoreFn) allocated %v times per run, want 0: nil is what FilterByLanguage returns when nothing matched, so this is the empty path production reaches",
				got)
		}
	})

	t.Run("across score distributions", func(t *testing.T) {
		streams := costStreams(100, nil)
		shapes := map[string]func(*Stream) int{
			// The best score arrives first, so the loop never updates its best
			// index after seeding.
			"winner first": func(s *Stream) int {
				if s.ID == 1 {
					return 100
				}
				return 0
			},
			// The best score arrives last, so the loop updates on the final
			// iteration having walked everything.
			"winner last": func(s *Stream) int {
				if int(s.ID) == len(streams) {
					return 100
				}
				return 0
			},
			// Every candidate ties, which is the documented
			// tie-goes-to-earliest path and the one a "collect the winners"
			// implementation would allocate hardest on.
			"all tied": func(_ *Stream) int { return 7 },
			// A new best on every iteration.
			"strictly ascending": func(s *Stream) int { return int(s.ID) },
			// Nothing above zero: the case the seed-from-the-first-element
			// design exists to handle, and the one that would need a sentinel
			// or a result slice if it were seeded any other way.
			"all negative": func(s *Stream) int { return -int(s.ID) },
			// The real scorer FindSubtitleByLanguage passes.
			"subtitle codec score": func(s *Stream) int { return SubtitleCodecScore(s.Codec) },
		}
		for name, scoreFn := range shapes {
			t.Run(name, func(t *testing.T) {
				if got := testing.AllocsPerRun(costRuns, func() {
					_ = BestByScore(streams, scoreFn)
				}); got != 0 {
					t.Errorf("BestByScore(%d candidates, %s scoreFn) allocated %v times per run, want 0: the count must not depend on the score distribution, or a library whose tracks happen to tie pays an allocation per episode that a test on distinct scores would never see",
						len(streams), name, got)
				}
			})
		}
	})
}

// TestFilterByLanguageAllocationRatePerCandidate pins the language-grading rate.
//
// FilterByLanguage is the first narrowing step of every match and the whole of
// the learned-profile subtitle path, and it is where the per-candidate cost of
// this package lives: langtag.Parse runs once per candidate through
// (*Stream).Lang, and that parse is three allocations for a plain alpha-3 code.
// The rate is therefore expected to be about 3 and must not creep: a second
// parse per candidate, or a lowercase pass added to the grading, doubles the
// language cost of every episode scan in the library.
//
// The measured slope is 3.01 per candidate; 3.5 leaves room for the floor's
// residue while failing on one added allocation per candidate.
func TestFilterByLanguageAllocationRatePerCandidate(t *testing.T) {
	const maxPerCandidate = 3.5

	counts := make([]float64, len(costSizes))
	for i, n := range costSizes {
		// Built here, outside the closure below: costSubtitles allocates a
		// slice and n Streams, and folding that into the measurement would
		// roughly double the reported rate.
		candidates := costSubtitles(n, nil)
		if got := FilterByLanguage(candidates, "eng", langtag.TierSameLanguage); len(got) != n {
			t.Fatalf("FilterByLanguage(%d eng candidates, \"eng\", same-language) kept %d, want all %d; the fixture is meant to measure the accepting path",
				n, len(got), n)
		}
		counts[i] = testing.AllocsPerRun(costRuns, func() {
			_ = FilterByLanguage(candidates, "eng", langtag.TierSameLanguage)
		})
	}

	low, high := costSizes[0], costSizes[len(costSizes)-1]
	rate := (counts[len(counts)-1] - counts[0]) / float64(high-low)
	if rate > maxPerCandidate {
		t.Errorf("FilterByLanguage(%d eng candidates, \"eng\", same-language) allocated %v times per run against %v at %d candidates, a rate of %.4f per candidate, want at most %.2f: this app grades every track of every episode of every show, so one added allocation per candidate is multiplied by the whole library",
			high, counts[len(counts)-1], counts[0], low, rate, maxPerCandidate)
	}
	t.Logf("FilterByLanguage: %.4f allocations per candidate over %d..%d (%v at %d, %v at %d, %v at %d); adjacent rates %.4f and %.4f",
		rate, low, high, counts[0], costSizes[0], counts[1], costSizes[1], counts[2], costSizes[2],
		(counts[1]-counts[0])/float64(costSizes[1]-costSizes[0]),
		(counts[2]-counts[1])/float64(costSizes[2]-costSizes[1]))
}

// TestFindSubtitleByLanguageAllocationRatePerCandidate pins the learned-profile
// path, which is FilterByLanguage composed with BestByScore over the codec
// score.
//
// It gets its own contract rather than riding FilterByLanguage's because it is
// the composition that runs on a brand-new show with no watch history — the
// Netflix-style seeding path — and because BestByScore contributing nothing is
// exactly what makes the composed rate equal the grading rate. A rate here
// materially above FilterByLanguage's would mean the codec ranking started
// allocating per candidate, which TestBestByScoreAllocatesNothingAtEverySize
// would catch on its own but which this contract localises to the composed
// call.
//
// Measured 3.01 per candidate, the same as FilterByLanguage alone.
func TestFindSubtitleByLanguageAllocationRatePerCandidate(t *testing.T) {
	const maxPerCandidate = 3.5

	counts := make([]float64, len(costSizes))
	for i, n := range costSizes {
		candidates := costSubtitles(n, nil)
		if got := FindSubtitleByLanguage(candidates, "eng", langtag.TierSameLanguage); got == nil {
			t.Fatalf("FindSubtitleByLanguage(%d eng srt candidates, \"eng\", same-language) = nil, want a match; the fixture is meant to measure the selecting path", n)
		}
		counts[i] = testing.AllocsPerRun(costRuns, func() {
			_ = FindSubtitleByLanguage(candidates, "eng", langtag.TierSameLanguage)
		})
	}

	low, high := costSizes[0], costSizes[len(costSizes)-1]
	rate := (counts[len(counts)-1] - counts[0]) / float64(high-low)
	if rate > maxPerCandidate {
		t.Errorf("FindSubtitleByLanguage(%d eng srt candidates, \"eng\", same-language) allocated %v times per run against %v at %d candidates, a rate of %.4f per candidate, want at most %.2f: this is the path that seeds every episode of a show with no watch history, so a per-candidate allocation here is multiplied by the whole library",
			high, counts[len(counts)-1], counts[0], low, rate, maxPerCandidate)
	}
	t.Logf("FindSubtitleByLanguage: %.4f allocations per candidate over %d..%d (%v at %d, %v at %d, %v at %d); adjacent rates %.4f and %.4f",
		rate, low, high, counts[0], costSizes[0], counts[1], costSizes[1], counts[2], costSizes[2],
		(counts[1]-counts[0])/float64(costSizes[1]-costSizes[0]),
		(counts[2]-counts[1])/float64(costSizes[2]-costSizes[1]))
}

// TestFilterByBoolPrefAllocationRateIsEffectivelyBounded pins the cheapest step
// in the matcher, and it splits the two branches because they have different
// costs and only one of them is a rate at all.
//
// FilterByBoolPref is called twice per audio match, so whatever it costs is paid
// twice on every episode of every show.
//
// The MATCHING branch appends into one slice, so its allocation count is the
// amortised growth of that slice — logarithmic in the candidate count, not linear
// in it. Measured, the slope is 0.006 per candidate: eight allocations at a
// thousand candidates against two at ten. A ceiling of 0.5 is a tenth of what an
// implementation allocating once per candidate would produce, which is the
// contract that says the flag filters are free relative to the language parse
// next to them.
//
// The FALLBACK branch is not a rate. Nothing matched, so nothing was appended and
// the function hands back the caller's own slice: the count is exactly zero at
// every size, and it stays zero only as long as the fallback returns the input
// rather than a copy of it. That distinction is invisible to a slope, which is
// why it is asserted separately — a copy is ONE allocation per call whose size
// scales with the candidate count, so it leaves the per-candidate rate untouched
// while allocating the whole candidate set on every episode. Verified by breaking
// it: replacing `return streams` with a copy moves the count from 0 to 1 and does
// not move the slope at all.
func TestFilterByBoolPrefAllocationRateIsEffectivelyBounded(t *testing.T) {
	// Every candidate is a subtitle, so IsAudio is false for all of them. That
	// makes desired=false the all-match fixture and desired=true the nothing-
	// matches fixture, from one candidate set.
	t.Run("the matching branch grows one slice", func(t *testing.T) {
		const maxPerCandidate = 0.5

		counts := make([]float64, len(costSizes))
		for i, n := range costSizes {
			// Outside the closure.
			candidates := costSubtitles(n, nil)
			if got := FilterByBoolPref(candidates, false, (*Stream).IsAudio); len(got) != n {
				t.Fatalf("FilterByBoolPref(%d subtitle candidates, false, IsAudio) kept %d, want all %d; the fixture is meant to make every candidate match",
					n, len(got), n)
			}
			counts[i] = testing.AllocsPerRun(costRuns, func() {
				_ = FilterByBoolPref(candidates, false, (*Stream).IsAudio)
			})
		}

		low, high := costSizes[0], costSizes[len(costSizes)-1]
		rate := (counts[len(counts)-1] - counts[0]) / float64(high-low)
		if rate > maxPerCandidate {
			t.Errorf("FilterByBoolPref(%d subtitle candidates, false, IsAudio) allocated %v times per run against %v at %d candidates, a rate of %.4f per candidate, want at most %.2f: the flag filters run twice per audio match, so a rate approaching one per candidate would double the per-track cost of every episode in the library",
				high, counts[len(counts)-1], counts[0], low, rate, maxPerCandidate)
		}
		t.Logf("matching branch: %.4f allocations per candidate over %d..%d (%v at %d, %v at %d, %v at %d), which is slice growth rather than per-candidate work",
			rate, low, high, counts[0], costSizes[0], counts[1], costSizes[1], counts[2], costSizes[2])
	})

	// The fallback is the documented behaviour that makes the predicate a
	// preference rather than a requirement, and it is on the hot path: a
	// reference track with no visual-impaired or descriptive counterpart in the
	// candidate set takes it on every episode of the show.
	t.Run("the fallback branch returns the input and allocates nothing", func(t *testing.T) {
		for _, n := range costSizes {
			candidates := costSubtitles(n, nil)
			got := FilterByBoolPref(candidates, true, (*Stream).IsAudio)
			if len(got) != n {
				t.Fatalf("FilterByBoolPref(%d subtitle candidates, true, IsAudio) returned %d, want all %d; nothing should match, so the documented fallback must hand back the original list",
					n, len(got), n)
			}
			// Identity, not equality: the fallback must return the caller's
			// slice, and a copy would satisfy every value comparison while
			// allocating the whole candidate set.
			if &got[0] != &candidates[0] {
				t.Errorf("FilterByBoolPref(%d subtitle candidates, true, IsAudio) returned a different backing array, want the input slice itself: copying the candidate set to return it costs one allocation of the whole set on every episode of every show, and it is a cost no per-candidate rate can see",
					n)
			}
			if got := testing.AllocsPerRun(costRuns, func() {
				_ = FilterByBoolPref(candidates, true, (*Stream).IsAudio)
			}); got != 0 {
				t.Errorf("FilterByBoolPref(%d subtitle candidates, true, IsAudio) allocated %v times per run, want 0: nothing matched, so nothing was appended and the input is handed straight back — any allocation here is paid on every episode of every show whose reference flag no candidate carries",
					n, got)
			}
		}
	})
}

// TestSelectionDoesNotMutateItsCandidateSlice is the precondition every
// AllocsPerRun contract in this file rests on, asserted rather than assumed.
//
// AllocsPerRun calls its closure many times with the same fixture. A function
// that sorted its input, appended to it, or wrote through its element pointers
// would therefore be measured against a DIFFERENT input on every call after the
// first, and the resulting number would describe nothing. langtag's Preference.Best
// appends into a fresh slice and never touches the candidates it walks, and
// FilterByBoolPref and BestByScore are equally read-only, so the fixtures here
// are safe to reuse — but that is a property of the current implementations, not
// of the signatures, and an in-place sort added for determinism would break
// every rate contract silently rather than loudly.
//
// The comparison is over dereferenced Stream values, so it catches a field write
// as well as a reordering: the candidates carry distinct IDs and titles, so an
// in-place sort moves the values along with the pointers.
func TestSelectionDoesNotMutateItsCandidateSlice(t *testing.T) {
	candidates := costSubtitles(50, nil)
	before := streamValues(candidates)

	// Several calls, because a mutation that is idempotent after the first call
	// is exactly the one a single call cannot see.
	for range 3 {
		_ = FilterByLanguage(candidates, "eng", langtag.TierSameLanguage)
		_ = FilterByBoolPref(candidates, true, (*Stream).IsSubtitle)
		_ = FindSubtitleByLanguage(candidates, "eng", langtag.TierSameLanguage)
		_ = BestByScore(candidates, func(s *Stream) int { return int(s.ID) })
	}

	if got := streamValues(candidates); !slices.Equal(got, before) {
		t.Fatalf("FilterByLanguage, FilterByBoolPref, FindSubtitleByLanguage and BestByScore over %d candidates left the slice changed, want it untouched: every allocation contract in this file measures one fixture repeatedly, so an in-place sort or write makes those numbers describe an input that no longer exists",
			len(candidates))
	}
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
