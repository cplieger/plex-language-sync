package streams

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plexapi/v2"
)

// This file measures allocation COST, split into two kinds of check.
//
//   - Benchmark* feeds the weekly tracker, which compares each series to its
//     previous value and alerts on ratio — so it catches zero becoming
//     non-zero but misses an added allocation diluted into a larger total
//     (425 -> 500 is a ratio of 1.18, below any sane threshold).
//   - Test* asserts the per-candidate RATE and SIZE INDEPENDENCE, properties a
//     single charted number can't express. Per-candidate cost matters here
//     because this app scans every track of every episode of a whole library.
//
// # Rate contracts: a slope, not an absolute count
//
// Each sweep measures allocations at three candidate counts and asserts the
// slope between the smallest and largest is at most a stated ceiling.
//   - A slope, not an absolute count: every absolute includes a fixed
//     per-call floor (reference language parse, the generic Best call) that
//     the slope cancels out, and the floor moves with the toolchain.
//   - A ceiling, not equality: measured slopes carry a small residue (4.02
//     rather than 4.00); ceilings sit about half an allocation above the
//     measured value.
//   - Three sizes with every adjacent rate logged, so a super-linear
//     regression reads as a rising rate rather than one bigger number.
//
// Every measured number here was read off the package before the assertion
// was written.
//
// # Fixture rules every sweep depends on
//
//   - Every fixture is built OUTSIDE the measured closure, or its own
//     allocations get counted.
//   - No measured function may mutate its candidate slice — AllocsPerRun
//     calls the closure many times against one fixture. Pinned separately by
//     TestSelectionDoesNotMutateItsCandidateSlice /
//     TestMatchingDoesNotMutateItsCandidateSlice.
//
// Each sweep also asserts its own REGIME before measuring (a candidate set
// that starts or stops matching takes a different code path and would chart
// the wrong thing).

// costRuns is the AllocsPerRun run count for every contract in this file and
// match_bench_test.go. Spot-checked not to matter: results are identical at
// 10, 50, 100 and 500 runs.
const costRuns = 100

// costSizes is the candidate sweep every rate contract walks: a 100x span,
// wide enough that a per-candidate allocation cannot hide in the fixed
// floor, with two adjacent gaps to distinguish super-linear growth from a
// larger constant.
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

// costStreams builds n audio candidates and applies mut to each, so one sweep
// can vary the content class that decides which matcher path runs — an
// untagged candidate set skips the language parse and costs a quarter of a
// tagged one, so a single fixture would pin only one of the paths a real
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

// costSubtitles builds n subtitle candidates (MatchSubtitle /
// FindSubtitleByLanguage shape); mut applies after the subtitle fields are
// set so a class can still override the codec or a flag.
func costSubtitles(n int, mut func(i int, s *Stream)) []*Stream {
	return costStreams(n, func(i int, s *Stream) {
		s.StreamType = StreamTypeSubtitle
		s.Codec = "srt"
		if mut != nil {
			mut(i, s)
		}
	})
}

// costTitle returns a title of exactly bytes bytes with mixed case so
// strings.ToLower cannot short-circuit. Length is a parameter because
// allocation-count independence from title length is itself a contract; see
// TestMatchAudioAllocationCountIsIndependentOfTitleLength.
func costTitle(bytes int) string {
	const unit = "VeryVerboseUpstreamMetadata "
	return strings.Repeat(unit, bytes/len(unit)+1)[:bytes]
}

// streamValues dereferences a candidate slice into its Stream values for a
// non-mutation check: catches a field write, and (since distinct candidates
// carry distinct IDs/titles) an in-place reorder too.
func streamValues(ss []*Stream) []Stream {
	out := make([]Stream, len(ss))
	for i, s := range ss {
		out[i] = *s
	}
	return out
}

// TestBestByScoreAllocatesNothingAtEverySize is an equality, not a ceiling,
// because the property is EVERY size, not one.
//
// BestByScore returns an element of the slice it was handed (tracks best
// index/score in two locals) rather than building a result, so it should
// allocate nothing at any candidate count — a result slice or a boxed score
// would put one allocation on every episode of every show. The weekly chart
// would catch that at the sizes it charts (zero to non-zero divides to
// Infinity), but not that the property holds at EVERY size, which is why
// this sweeps 0 to 1000.
//
// Measured: a result slice added to this function does NOT register at all
// up to 8 candidates (the compiler keeps a small non-escaping slice on the
// stack, only reaching the allocator past ~64 bytes) — a contract at one
// small size would have missed exactly that break.
//
// The scoreFn shapes vary because the count could plausibly depend on the
// data and does not: winner-last walks the whole slice, all-tied never
// updates the best index, all-negative is the zero-floor edge case a naive
// seed would get wrong.
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

	// nil rather than empty: FilterByLanguage returns nil (not an empty
	// slice) when nothing matched, which is the shape production passes.
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
			// Best score arrives first: no update after seeding.
			"winner first": func(s *Stream) int {
				if s.ID == 1 {
					return 100
				}
				return 0
			},
			// Best score arrives last: updates on the final iteration.
			"winner last": func(s *Stream) int {
				if int(s.ID) == len(streams) {
					return 100
				}
				return 0
			},
			// Every candidate ties (tie-goes-to-earliest path); the one a
			// "collect the winners" implementation would allocate hardest on.
			"all tied": func(_ *Stream) int { return 7 },
			// A new best on every iteration.
			"strictly ascending": func(s *Stream) int { return int(s.ID) },
			// Nothing above zero: the case a seed-from-first-element design
			// exists to handle.
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

// TestFilterByLanguageAllocationRatePerCandidate pins the language-grading
// rate: langtag.Parse runs once per candidate via (*Stream).Lang (three
// allocations for a plain alpha-3 code), so a second parse or an added
// lowercase pass would double the per-track cost of every library scan.
//
// Measured slope 3.01/candidate; ceiling 3.5 absorbs floor residue while
// still failing on one added allocation per candidate.
func TestFilterByLanguageAllocationRatePerCandidate(t *testing.T) {
	const maxPerCandidate = 3.5

	counts := make([]float64, len(costSizes))
	for i, n := range costSizes {
		// Built outside the closure: costSubtitles itself allocates.
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

// TestFindSubtitleByLanguageAllocationRatePerCandidate covers the same
// composition (FilterByLanguage + BestByScore) on the new-show seeding path
// (no watch history). Gets its own contract because BestByScore contributing
// nothing is what makes the composed rate equal FilterByLanguage's; a higher
// rate here would localize a regression in the codec ranking to this call.
//
// Measured 3.01/candidate, same as FilterByLanguage alone.
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

// TestFilterByBoolPrefAllocationRateIsEffectivelyBounded pins the cheapest
// step in the matcher, called twice per audio match, split into two branches
// with different costs.
//
// MATCHING branch: appends into one slice, so the count is amortised
// slice-growth (logarithmic, not linear). Measured slope 0.006/candidate
// (8 allocations at 1000 candidates vs 2 at 10); ceiling 0.5 is a tenth of
// what one-allocation-per-candidate would produce.
//
// FALLBACK branch is not a rate: nothing matched, nothing was appended, the
// function hands back the caller's own slice — exactly zero at every size,
// true only as long as the fallback returns the input rather than a copy.
// Verified by breaking it: replacing `return streams` with a copy moves the
// count from 0 to 1 without moving the slope at all.
func TestFilterByBoolPrefAllocationRateIsEffectivelyBounded(t *testing.T) {
	// Every candidate is a subtitle (IsAudio false for all), so
	// desired=false is the all-match fixture and desired=true is the
	// nothing-matches fixture.
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
	// preference rather than a requirement, and it is on the hot path (a
	// reference track with no HI/descriptive counterpart takes it every
	// episode).
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
// AllocsPerRun contract in this file rests on: AllocsPerRun reuses one
// fixture across many calls, so a function that sorts or writes through its
// input would be measured against a different input after the first call.
// langtag's Preference.Best and this package's filters are read-only today,
// but that is a property of the current implementation, not the signature.
//
// Comparison is over dereferenced Stream values, so it also catches a
// reorder (candidates carry distinct IDs/titles).
func TestSelectionDoesNotMutateItsCandidateSlice(t *testing.T) {
	candidates := costSubtitles(50, nil)
	before := streamValues(candidates)

	// Several calls: a mutation idempotent after the first call is the one
	// a single call cannot see.
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
