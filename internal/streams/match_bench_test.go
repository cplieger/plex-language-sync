package streams

import (
	"fmt"
	"slices"
	"testing"

	"github.com/cplieger/langtag/v2"
)

// This file holds the allocation contracts for match.go — the two entry points
// the event and reconcile planes call once per episode per user.
//
// Why they are worth pinning is set out in score_bench_test.go's header, which
// also owns the shared fixtures (costStreams, costSubtitles, costTitle,
// streamValues), the sweep sizes and the run count, and states the two fixture
// rules every contract here depends on: build outside the measured closure, and
// never measure a function that mutates its input. The short version: the weekly
// tracker compares each benchmark series against its previous value and alerts
// on a ratio, so an added allocation per candidate is invisible to it, and this
// app walks every episode of every show — a per-track cost is multiplied by the
// track count of an entire media library.
//
// # Content class is the axis, and it is not a detail
//
// MatchAudio's per-candidate cost varies four-fold with what the candidates
// CONTAIN, because the content decides which path runs:
//
//   - A tagged set pays langtag.Parse per candidate inside the language grading,
//     three allocations for a plain alpha-3 code, plus one lowercase pass for the
//     descriptive check. Measured 4.02 per candidate.
//   - A set carrying region-bearing BCP 47 tags (es-419, zh-TW) pays a more
//     expensive parse: 5.26 per candidate, the most costly class here and the
//     one a real Spanish or Chinese library actually exercises.
//   - An untagged set, and a set whose codes langtag cannot read, skip the parse
//     entirely and cost 1.02.
//   - A set that matches nothing returns before the descriptive check and costs
//     3.00.
//
// A single fixture would pin one of those and leave the rest unguarded, so every
// class gets its own ceiling read off its own measurement. Two consequences worth
// stating because they are easy to get backwards: the classes are not
// interchangeable, and the cheap classes are cheap because work was SKIPPED, so
// their low ceilings are the ones that catch a regression which starts parsing
// what it used to skip.
//
// # A rate that depends on the fixture rather than on the code is not a contract
//
// Two shapes were deliberately left out after measuring them. A candidate set
// mixing four unrelated languages measures 3.27 per candidate, and a partially
// forced subtitle set measures 1.51, but neither number describes the code: both
// fall out of the FRACTION of candidates that survive an early filter, so the
// same implementation yields any rate between 0.76 and 3.01 depending on how the
// fixture is dealt. Pinning one of them would gate a number a library's contents
// can move on their own. Where the surviving fraction is itself the property —
// a discarded candidate never reaching the language parse — it is asserted as a
// RELATIONSHIP between two fractions instead, by
// TestMatchSubtitleDiscardsForcedNonMatchesBeforeParsing.

// TestMatchAudioAllocationRatePerCandidate is the highest-value contract in the
// package.
//
// MatchAudio runs for every episode of every show the app propagates to, so its
// per-candidate rate is multiplied by the track count of the whole library. The
// weekly tracker charts its absolute allocation count at three sizes and cannot
// see this: an added allocation per candidate moves the n=100 series from 422 to
// 522, a ratio of 1.24 that no threshold alerts on, while the same change costs
// a hundred extra allocations on every episode of a hundred-episode show.
//
// Each class asserts the slope between the smallest and largest candidate count
// against a ceiling about half an allocation above what it measures, and logs
// both adjacent rates so a super-linear regression reads as a rising rate rather
// than as one larger number.
func TestMatchAudioAllocationRatePerCandidate(t *testing.T) {
	classes := []struct {
		name string
		// desc names the fixture in a failure message, since the class name
		// alone does not say what the candidates carry.
		desc string
		// build returns the reference and the candidates for a candidate count.
		// Every allocation it makes happens before the measured closure runs.
		build func(n int) (*Stream, []*Stream)
		// maxPerCandidate is the ceiling on the slope, read off the measured
		// rate in the comment beside it.
		maxPerCandidate float64
		// wantNil is the regime: whether this class is meant to match at all.
		// Asserted before measuring, because a class that silently stops
		// matching would chart the early-out instead of the work.
		wantNil bool
	}{
		{
			// The ordinary case: one language, mixed-case titles. Measured 4.02
			// — three allocations for the alpha-3 parse per candidate plus one
			// for the descriptive check's lowercase pass.
			name: "plain alpha-3 tagged",
			desc: "eng candidates with mixed-case titles",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, nil)
				return c[0], c
			},
			maxPerCandidate: 4.5,
		},
		{
			// Region-bearing BCP 47 tags, which (*Stream).Lang prefers over the
			// coarse code. This is the most expensive class in the package at
			// 5.26 per candidate, because parsing a tag with a region costs more
			// than parsing a bare alpha-3 code — and it is the class a Spanish
			// or Chinese library exercises on every episode, so it is the one
			// whose ceiling actually binds in production.
			name: "region-bearing BCP 47 tags",
			desc: "es-ES/es-419/es-MX/es-AR candidates",
			build: func(n int) (*Stream, []*Stream) {
				tags := []string{"es-ES", "es-419", "es-MX", "es-AR"}
				c := costStreams(n, func(i int, s *Stream) {
					s.LanguageCode = "spa"
					s.LanguageTag = tags[i%len(tags)]
				})
				return c[0], c
			},
			maxPerCandidate: 5.75,
		},
		{
			// No language at all on either side, which routes through the
			// langAbsent branch: HasNoLanguage is two TrimSpace calls and no
			// parse, so the rate collapses to the descriptive check's single
			// lowercase pass. Measured 1.02, and the tight ceiling is the point
			// — it is what catches a change that starts parsing a track this
			// path deliberately does not parse.
			name: "untagged",
			desc: "candidates with no language at all",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, func(_ int, s *Stream) {
					s.LanguageCode = ""
					s.LanguageTag = ""
				})
				return c[0], c
			},
			maxPerCandidate: 1.5,
		},
		{
			// A code langtag cannot read, which routes through langUnreadable
			// and compares LanguageCode as a plain string. Also 1.02, and
			// deliberately a separate class from untagged: the package treats an
			// unrecognised code as distinct from an absent one, so a change that
			// folded the two would still pass one of these and not both.
			name: "unreadable language code",
			desc: "candidates carrying a private code langtag cannot parse",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, func(_ int, s *Stream) {
					s.LanguageCode = "qqq-private"
					s.LanguageTag = ""
				})
				return c[0], c
			},
			maxPerCandidate: 1.5,
		},
		{
			// Titles absent, so TitleForMatch returns "" and strings.ToLower on
			// an empty string allocates nothing. Measured 3.02, exactly the
			// tagged rate minus the lowercase pass, which is what identifies
			// that pass as the fourth allocation in the plain class.
			name: "empty titles",
			desc: "eng candidates with no title fields",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, func(_ int, s *Stream) {
					s.DisplayTitle = ""
					s.ExtendedDisplayTitle = ""
					s.Title = ""
				})
				return c[0], c
			},
			maxPerCandidate: 3.5,
		},
		{
			// Commentary tracks on both sides, so the descriptive preference
			// keeps the whole set and the scoring stage still runs. Measured
			// 4.02, the same as plain: this class exists to prove the
			// commentary path is not more expensive per candidate than the
			// ordinary one, since ShouldSkipSubtitleForCommentary calls
			// MatchAudio a second time for exactly these tracks.
			name: "descriptive",
			desc: "eng commentary candidates",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, func(i int, s *Stream) {
					title := fmt.Sprintf("English Commentary %d", i)
					s.DisplayTitle, s.ExtendedDisplayTitle = title, title
				})
				return c[0], c
			},
			maxPerCandidate: 4.5,
		},
		{
			// The visual-impaired preference keeps the whole set. Measured 4.02,
			// again the plain rate: the flag filter costs slice growth, not
			// per-candidate work, which is what TestFilterByBoolPrefAllocation-
			// RateIsEffectivelyBounded pins directly.
			name: "visual-impaired",
			desc: "eng visual-impaired candidates",
			build: func(n int) (*Stream, []*Stream) {
				c := costStreams(n, func(_ int, s *Stream) { s.VisualImpaired = true })
				return c[0], c
			},
			maxPerCandidate: 4.5,
		},
		{
			// Nothing within the audio floor, so selectByLanguage returns empty
			// and MatchAudio returns before the flag filters and the scoring.
			// Measured 3.00 — the parse per candidate and nothing else — and it
			// is worth its own ceiling because it is the path a library gets on
			// every episode of a show whose reference language it does not
			// carry, which is the common case for a multi-language library.
			name: "no match",
			desc: "jpn reference against eng candidates",
			build: func(n int) (*Stream, []*Stream) {
				return &Stream{
					StreamType:   StreamTypeAudio,
					LanguageCode: "jpn",
					DisplayTitle: "Japanese (AAC Stereo)",
				}, costStreams(n, nil)
			},
			maxPerCandidate: 3.5,
			wantNil:         true,
		},
	}

	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			counts := make([]float64, len(costSizes))
			for i, n := range costSizes {
				// Built here, outside the closure below: the slice, the n
				// Streams and their titles are all allocations, and folding them
				// into the measurement would report roughly double the rate.
				ref, candidates := c.build(n)
				if got := MatchAudio(ref, candidates); (got == nil) != c.wantNil {
					t.Fatalf("MatchAudio(ref, %d %s) returned nil = %v, want %v; the fixture drifted out of the %s regime and would measure a different path than the ceiling was read from",
						n, c.desc, got == nil, c.wantNil, c.name)
				}
				counts[i] = testing.AllocsPerRun(costRuns, func() {
					_ = MatchAudio(ref, candidates)
				})
			}

			low, high := costSizes[0], costSizes[len(costSizes)-1]
			rate := (counts[len(counts)-1] - counts[0]) / float64(high-low)
			if rate > c.maxPerCandidate {
				t.Errorf("MatchAudio(ref, %d %s) allocated %v times per run against %v at %d candidates, a rate of %.4f per candidate, want at most %.2f: this app propagates to every episode of every show, so one added allocation per track is multiplied by the track count of the whole media library",
					high, c.desc, counts[len(counts)-1], counts[0], low, rate, c.maxPerCandidate)
			}
			t.Logf("%s: %.4f allocations per candidate over %d..%d (%v at %d, %v at %d, %v at %d); adjacent rates %.4f and %.4f",
				c.name, rate, low, high,
				counts[0], costSizes[0], counts[1], costSizes[1], counts[2], costSizes[2],
				(counts[1]-counts[0])/float64(costSizes[1]-costSizes[0]),
				(counts[2]-counts[1])/float64(costSizes[2]-costSizes[1]))
		})
	}
}

// TestMatchAudioAllocationCountIsIndependentOfTitleLength pins the property that
// stops an upstream with verbose metadata amplifying the app's work.
//
// Titles arrive from Plex, which sources them from agents and filenames, so
// their length is chosen upstream rather than by this app. MatchAudio lowercases
// each candidate's title for the descriptive check, and a lowercase pass over a
// long string is one big allocation rather than many small ones — so the
// allocation COUNT must be a function of how many candidates there are and never
// of how many bytes their titles carry. Measured: 422 allocations at a hundred
// candidates for every title length from 20 bytes to 120000, a span of 6000x.
//
// The distinction this contract draws, and the reason it is a count and not a
// byte assertion: the BYTES allocated do scale with title length and are
// supposed to, which is what BenchmarkMatchAudio* reports as B/op. What must not
// scale is the number of allocations, because that is the quantity a change from
// one lowercase pass to a per-word or per-rune loop would move — turning a
// verbose-metadata library from one allocation per track into hundreds, on every
// episode, with the byte total barely moving and the tracker's ratio staying
// quiet.
//
// The sweep tops out at 12000 bytes rather than the 120000 it was measured to:
// the count is identical at both, and the larger case cost seventeen seconds
// under the race detector on its own, which is more than the rest of this
// package's suite. 600x is already far more span than a per-byte allocation
// could hide in — at 12000 bytes one allocation per kilobyte would show as
// roughly 1200 extra against a baseline of 422.
func TestMatchAudioAllocationCountIsIndependentOfTitleLength(t *testing.T) {
	const candidates = 100
	titleBytes := []int{20, 120, 1200, 12000}

	counts := make([]float64, len(titleBytes))
	for i, size := range titleBytes {
		// Built here, outside the closure: costTitle allocates a string per
		// candidate, and at the top of the sweep that fixture is 1.2 MB of
		// titles — an order of magnitude more than the call being measured.
		streams := costStreams(candidates, func(i int, s *Stream) {
			title := costTitle(size)
			s.DisplayTitle, s.ExtendedDisplayTitle = title, title
			s.ID = i + 1
		})
		ref := streams[0]
		if MatchAudio(ref, streams) == nil {
			t.Fatalf("MatchAudio(ref, %d eng candidates with %d-byte titles) = nil, want a match; the fixture is meant to measure the matching path",
				candidates, size)
		}
		if got := len(streams[0].TitleForMatch()); got != size {
			t.Fatalf("MatchAudio fixture: candidate title is %d bytes, want %d; the sweep must actually vary title length", got, size)
		}
		counts[i] = testing.AllocsPerRun(costRuns, func() {
			_ = MatchAudio(ref, streams)
		})
	}

	for i, got := range counts {
		if got != counts[0] {
			t.Errorf("MatchAudio(ref, %d eng candidates with %d-byte titles) allocated %v times per run, want %v (its count at %d-byte titles): the allocation count must track candidate COUNT and never title LENGTH, or an upstream that starts reporting verbose metadata multiplies the work this app does on every episode of every show without changing a single track",
				candidates, titleBytes[i], got, counts[0], titleBytes[0])
		}
	}
	// Reports the counts rather than asserting constancy in prose, so the line
	// stays true on a failing run and shows the shape of the drift.
	t.Logf("MatchAudio at %d candidates: %v allocations at title lengths %v bytes respectively",
		candidates, counts, titleBytes)
}

// TestMatchAudioAllocatesNothingForANilReference pins the free early-out.
//
// A nil reference is the ordinary case, not an edge: an episode whose user
// selected no audio track reaches here, and the reconcile plane replays history
// items that may carry no audio reference at all. MatchAudio answers nil before
// touching the candidates, so the refusal costs nothing however many tracks the
// target episode has — and that must stay true as a function of the candidate
// count, which is why it is swept rather than checked once.
//
// This is the one assertion in the file the weekly tracker would also catch,
// since zero becoming non-zero divides to Infinity and alerts at any threshold.
// It is here because the tracker charts one candidate count and the property is
// about all of them.
func TestMatchAudioAllocatesNothingForANilReference(t *testing.T) {
	for _, n := range append([]int{0}, costSizes...) {
		candidates := costStreams(n, nil)
		if got := MatchAudio(nil, candidates); got != nil {
			t.Fatalf("MatchAudio(nil, %d candidates) = %v, want nil", n, Desc(got))
		}
		if got := testing.AllocsPerRun(costRuns, func() {
			_ = MatchAudio(nil, candidates)
		}); got != 0 {
			t.Errorf("MatchAudio(nil, %d candidates) allocated %v times per run, want 0: an episode whose user selected no audio must cost nothing to skip, or every episode of every show with no reference selection pays for a decision that was already made",
				n, got)
		}
	}
}

// TestMatchSubtitleAllocationRatePerCandidate is MatchAudio's contract for the
// subtitle path, which runs on the same per-episode schedule and therefore
// carries the same library-scale multiplier.
//
// The rates are lower than MatchAudio's because the subtitle path has no
// descriptive check: there is no lowercase pass per candidate, so the cost is the
// language parse and nothing else. Measured 3.01 across every matching class.
// That the forced and hearing-impaired classes measure the SAME rate as plain is
// the finding worth pinning — both flags are handled by filters whose cost is
// slice growth rather than per-candidate work, so neither flag makes a library's
// subtitle scan more expensive per track.
func TestMatchSubtitleAllocationRatePerCandidate(t *testing.T) {
	classes := []struct {
		name            string
		desc            string
		build           func(n int) (*Stream, []*Stream)
		maxPerCandidate float64
		wantNil         bool
	}{
		{
			// Measured 3.01: the parse per candidate, no lowercase pass.
			name: "plain",
			desc: "eng srt candidates",
			build: func(n int) (*Stream, []*Stream) {
				c := costSubtitles(n, nil)
				return c[0], c
			},
			maxPerCandidate: 3.5,
		},
		{
			// Every candidate forced, which is the worst case for this class:
			// the forced filter keeps the whole set, so every candidate still
			// reaches the parse. Measured 3.01. A partially forced set costs
			// LESS, which is a fixture property rather than a code property and
			// is why the ceiling is read from the all-forced case; that the
			// discarded ones are never paid for is pinned separately by
			// TestMatchSubtitleDiscardsForcedNonMatchesBeforeParsing.
			name: "forced",
			desc: "eng srt candidates all flagged forced",
			build: func(n int) (*Stream, []*Stream) {
				c := costSubtitles(n, func(_ int, s *Stream) { s.Forced = true })
				return c[0], c
			},
			maxPerCandidate: 3.5,
		},
		{
			// The hearing-impaired preference keeps the whole set. Measured
			// 3.01, the same as plain, which is the assertion: the preference
			// runs AFTER the language grading precisely so it cannot capture the
			// candidate set, and that ordering must not cost a per-candidate
			// allocation either.
			name: "hearing-impaired",
			desc: "eng srt candidates all flagged hearing-impaired",
			build: func(n int) (*Stream, []*Stream) {
				c := costSubtitles(n, func(_ int, s *Stream) { s.HearingImpaired = true })
				return c[0], c
			},
			maxPerCandidate: 3.5,
		},
		{
			// Nothing within the floor. Measured 3.00, the parse alone, and it
			// is the path a library takes on every episode of a show carrying no
			// subtitle in the reference language — which is the case the "no
			// subtitle means no subtitle" policy makes common.
			name: "no match",
			desc: "jpn reference against eng srt candidates",
			build: func(n int) (*Stream, []*Stream) {
				return &Stream{
					StreamType:   StreamTypeSubtitle,
					LanguageCode: "jpn",
					Codec:        "srt",
					DisplayTitle: "Japanese (SRT)",
				}, costSubtitles(n, nil)
			},
			maxPerCandidate: 3.5,
			wantNil:         true,
		},
	}

	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			counts := make([]float64, len(costSizes))
			for i, n := range costSizes {
				// Outside the closure, as everywhere in this file.
				ref, candidates := c.build(n)
				if got := MatchSubtitle(ref, candidates, langtag.TierSameLanguage); (got == nil) != c.wantNil {
					t.Fatalf("MatchSubtitle(ref, %d %s, same-language) returned nil = %v, want %v; the fixture drifted out of the %s regime",
						n, c.desc, got == nil, c.wantNil, c.name)
				}
				counts[i] = testing.AllocsPerRun(costRuns, func() {
					_ = MatchSubtitle(ref, candidates, langtag.TierSameLanguage)
				})
			}

			low, high := costSizes[0], costSizes[len(costSizes)-1]
			rate := (counts[len(counts)-1] - counts[0]) / float64(high-low)
			if rate > c.maxPerCandidate {
				t.Errorf("MatchSubtitle(ref, %d %s, same-language) allocated %v times per run against %v at %d candidates, a rate of %.4f per candidate, want at most %.2f: the subtitle path runs for every episode of every show alongside the audio path, so one added allocation per track is multiplied by the whole library twice over",
					high, c.desc, counts[len(counts)-1], counts[0], low, rate, c.maxPerCandidate)
			}
			t.Logf("%s: %.4f allocations per candidate over %d..%d (%v at %d, %v at %d, %v at %d); adjacent rates %.4f and %.4f",
				c.name, rate, low, high,
				counts[0], costSizes[0], counts[1], costSizes[1], counts[2], costSizes[2],
				(counts[1]-counts[0])/float64(costSizes[1]-costSizes[0]),
				(counts[2]-counts[1])/float64(costSizes[2]-costSizes[1]))
		})
	}
}

// TestMatchSubtitleDiscardsForcedNonMatchesBeforeParsing pins that a candidate
// the forced filter throws away is never PAID for.
//
// TestMatchSubtitleForcedFilterRunsBeforeGrading in langmatch_test.go already
// pins the ordering itself, and pins it the right way: as a correctness
// property, because reversing the two steps returns no subtitle for an episode
// that has a forced track one tier further out. This is not a second witness of
// that. It catches the change that ordering test cannot see — a language parse
// hoisted ahead of the forced filter, or precomputed for the whole candidate
// set, which leaves every outcome identical and starts charging for tracks that
// are immediately discarded.
//
// That regression is invisible twice over: the behavioural suite passes because
// nothing about the result changed, and the weekly tracker passes because the
// absolute count moves by a ratio well under its threshold. What moves is the
// per-candidate rate on a partially forced library, which is the ordinary shape
// — most libraries carry a handful of forced tracks among many.
//
// The assertion is the RELATIONSHIP between two forced fractions, not either
// rate. The half-forced rate alone would be no contract at all: it is a function
// of how many candidates happen to carry the flag (measured 3.01 all-forced,
// 1.51 half, 0.76 one-in-four), so a library's contents could move it with no
// code change. The ratio between two fractions of the same fixture can only be
// moved by charging for the discarded ones.
func TestMatchSubtitleDiscardsForcedNonMatchesBeforeParsing(t *testing.T) {
	// The discarded fraction must cost at most this share of the all-forced
	// rate. Measured 0.50, which is what halving the parsed set produces and is
	// stable because the fixed floor cancels out of a slope. Verified against the
	// break: precomputing every candidate's language ahead of the filter drives
	// the share to 0.75, so 0.60 sits clear of both with room either side.
	const maxShareOfAllForced = 0.60

	ref := &Stream{
		StreamType:   StreamTypeSubtitle,
		LanguageCode: "eng",
		Codec:        "srt",
		Forced:       true,
		DisplayTitle: "English (Forced)",
	}

	// rateFor measures the per-candidate slope for a set where one candidate in
	// forcedEvery carries the forced flag. It returns a value and takes no
	// *testing.T, so every assertion in this test stays in the test.
	rateFor := func(forcedEvery int) (rate float64, counts []float64, err error) {
		counts = make([]float64, len(costSizes))
		for i, n := range costSizes {
			// Outside the closure.
			candidates := costSubtitles(n, func(i int, s *Stream) {
				s.Forced = i%forcedEvery == 0
			})
			if MatchSubtitle(ref, candidates, langtag.TierSameLanguage) == nil {
				return 0, nil, fmt.Errorf("1-in-%d forced set of %d candidates matched nothing", forcedEvery, n)
			}
			counts[i] = testing.AllocsPerRun(costRuns, func() {
				_ = MatchSubtitle(ref, candidates, langtag.TierSameLanguage)
			})
		}
		span := float64(costSizes[len(costSizes)-1] - costSizes[0])
		return (counts[len(counts)-1] - counts[0]) / span, counts, nil
	}

	allForced, allCounts, err := rateFor(1)
	if err != nil {
		t.Fatalf("MatchSubtitle forced fixture: %v", err)
	}
	halfForced, halfCounts, err := rateFor(2)
	if err != nil {
		t.Fatalf("MatchSubtitle forced fixture: %v", err)
	}

	share := halfForced / allForced
	if share > maxShareOfAllForced {
		t.Errorf("MatchSubtitle(forced ref, candidates half of which are forced, same-language) allocated at %.4f per candidate against %.4f for an all-forced set, a share of %.2f, want at most %.2f: a candidate the forced filter discards must never reach the language parse, or every episode of every show pays for tracks it threw away — a rate change no outcome test and no benchmark ratio can see",
			halfForced, allForced, share, maxShareOfAllForced)
	}
	t.Logf("MatchSubtitle: %.4f allocations per candidate all-forced (%v) against %.4f half-forced (%v), a share of %.2f — the discarded half is never parsed",
		allForced, allCounts, halfForced, halfCounts, share)
}

// TestMatchingDoesNotMutateItsCandidateSlice is the precondition every
// AllocsPerRun contract in this file rests on; see
// TestSelectionDoesNotMutateItsCandidateSlice for the reasoning and the
// comparison's reach. In short: AllocsPerRun calls its closure many times
// against one fixture, so a matcher that sorted or wrote through its input would
// be measured against a different input on every call after the first, and every
// rate above would describe nothing.
func TestMatchingDoesNotMutateItsCandidateSlice(t *testing.T) {
	audio := costStreams(50, nil)
	audioBefore := streamValues(audio)
	subtitles := costSubtitles(50, func(i int, s *Stream) { s.Forced = i%2 == 0 })
	subtitlesBefore := streamValues(subtitles)

	// Several calls: a mutation that is idempotent after the first is the one a
	// single call cannot see.
	for range 3 {
		_ = MatchAudio(audio[0], audio)
		_ = MatchSubtitle(subtitles[0], subtitles, langtag.TierSameLanguage)
		_ = ShouldSkipSubtitleForCommentary(audio[0], audio)
	}

	if got := streamValues(audio); !slices.Equal(got, audioBefore) {
		t.Fatalf("MatchAudio and ShouldSkipSubtitleForCommentary over %d audio candidates left the slice changed, want it untouched: every allocation contract in this file measures one fixture repeatedly, so an in-place sort or write makes those rates describe an input that no longer exists",
			len(audio))
	}
	if got := streamValues(subtitles); !slices.Equal(got, subtitlesBefore) {
		t.Fatalf("MatchSubtitle over %d subtitle candidates left the slice changed, want it untouched: see the audio case above",
			len(subtitles))
	}
}
