package tracksync

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/cplieger/go/keylock"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plexapi/v3"
)

// userShow is one user's show. Plex keeps a selection per (user, part), so one
// user's show lock covers every record and selection a decision on that show
// reads or writes.
type userShow struct{ UserID, Show string }

func newShowLocks() *keylock.Set[userShow] {
	return keylock.New(func(a, b userShow) int {
		return cmp.Or(strings.Compare(a.UserID, b.UserID), strings.Compare(a.Show, b.Show))
	})
}

// plan is one decision to put on a set of episodes for one user.
type plan struct {
	pick       func(*streams.Episode) selection
	ref        streams.Pair // the choice's own streams for the substitution log; zero for a profile seed
	targets    []streams.Episode
	basis      streams.Basis
	observedAt int64 // the intent's ObservedAt; 0 for a household or profile seed
	userList   bool  // targets came from the user's own list, so their LastViewedAt is the user's
}

// appliedOnce reports a current record of this plan's decision: what the part
// holds now is that or the user's own later change, neither the app's to
// overwrite.
func (p *plan) appliedOnce(rec streams.Applied, hasRec bool, part int) bool {
	return hasRec && rec.PartID == part && rec.Basis == p.basis
}

// playedSince reports a view after the last decision the app knows about on
// the part, which the app did not see. A stale record never holds a write
// back, since a view of the previous file says nothing about the current
// one; nor does a record whose subtitle is unknown, since that gap is in the
// app's own record, not a choice of the user's, and only a write closes it.
func (p *plan) playedSince(rec streams.Applied, hasRec bool, part int, viewed int64) bool {
	if hasRec && (rec.PartID != part || rec.SubtitleUnknown) {
		return false
	}
	return viewed > max(rec.At, p.observedAt)
}

// applier is the only code that writes stream selections; each applyLocked
// call writes one plan for one user.
type applier struct {
	cache cacheStore
	users userLookup
	locks *keylock.Set[userShow]
	now   func() time.Time
}

func newApplier(c cacheStore, u userLookup) *applier {
	return &applier{cache: c, users: u, locks: newShowLocks(), now: time.Now}
}

// lock takes the user's show until unlock is called, or returns ctx.Err()
// holding nothing. keylock is not reentrant, so a caller takes it once, then
// reads the records it decides from and calls applyLocked.
func (a *applier) lock(ctx context.Context, userID, show string) (unlock func(), err error) {
	return a.locks.Lock(ctx, userShow{UserID: userID, Show: show})
}

// applyLocked puts p on each target through the user's client, recording
// what it left on each part, and returns how many episodes it changed. The
// caller holds the user's show lock. A failed read or write is logged and
// leaves no record of the plan, so a later pass retries it. The only error is
// ctx's, returned between targets or for a request the context ended.
func (a *applier) applyLocked(ctx context.Context, client PlexReadWriter, userID string, p *plan) (written int, err error) {
	run := applyRun{applier: a, client: client, p: p, userID: userID, username: a.users.Name(userID)}
	for i := range p.targets {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		changed, err := run.target(ctx, &p.targets[i])
		if changed {
			written++
		}
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// applyRun is one applyLocked call's fixed arguments.
type applyRun struct {
	*applier
	client   PlexReadWriter
	p        *plan
	userID   string
	username string
}

func (r *applyRun) target(ctx context.Context, listed *streams.Episode) (bool, error) {
	rec, hasRec := r.cache.AppliedFor(r.userID, listed.RatingKey)
	if part := streams.FirstPartID(listed); part != 0 {
		if r.p.appliedOnce(rec, hasRec, part) ||
			r.p.userList && r.p.playedSince(rec, hasRec, part, int64(listed.LastViewedAt)) {
			return false, nil
		}
	}
	ep, err := r.client.Episode(ctx, plex.RatingKey(listed.RatingKey))
	switch {
	case errors.Is(err, plex.ErrNotFound):
		r.cache.ForgetApplied(r.userID, listed.RatingKey)
		slog.Debug("episode no longer in the library, record dropped",
			"key", textsafe.SanitizeSingleLine(listed.RatingKey), "user", r.username)
		return false, nil
	case err != nil:
		return false, r.failed(ctx, "failed to reload episode", listed, err)
	}
	// The read is the user's own, so its part and view decide when the list
	// could not.
	part := streams.FirstPartID(ep)
	if part == 0 || r.p.appliedOnce(rec, hasRec, part) || r.p.playedSince(rec, hasRec, part, int64(ep.LastViewedAt)) {
		return false, nil
	}
	return r.write(ctx, ep, part)
}

// write PUTs each kind whose wanted stream differs from what the part holds
// for the user, then records the ids the part holds after. When the part
// could not be read back after its audio write, it records the written audio
// and an unknown subtitle.
func (r *applyRun) write(ctx context.Context, ep *streams.Episode, part int) (bool, error) {
	want := r.p.pick(ep)
	cur := streams.Selected(ep)
	got := streams.Applied{Basis: r.p.basis, PartID: part, AudioID: streams.ID(cur.Audio), SubtitleID: streams.ID(cur.Subtitle)}
	changed, msg, err := r.put(ctx, ep, &want, &got)
	if err != nil {
		if !changed {
			return false, r.failed(ctx, msg, ep, err)
		}
		// A part an earlier PUT changed is recorded with what it is known
		// to hold and no basis: Classify reads the app's write as an echo
		// rather than the user's pick, and no plan's basis is zero, so the
		// next pass retries.
		got.Basis = streams.Basis{}
	}
	got.At = r.now().Unix()
	r.cache.RecordApplied(r.userID, ep.RatingKey, got)
	if err != nil {
		return true, r.failed(ctx, msg, ep, err)
	}
	return changed, nil
}

// put sends want's PUTs on got.PartID, updating got's ids as each is
// accepted. On a failed request it returns the log message for it, with
// changed telling whether an earlier PUT was accepted.
func (r *applyRun) put(ctx context.Context, ep *streams.Episode, want *selection, got *streams.Applied) (changed bool, msg string, err error) {
	if changed, msg, err = r.putAudio(ctx, ep, want, got); err != nil {
		return changed, msg, err
	}
	subChanged, msg, err := r.putSubtitle(ctx, ep, want, got)
	return changed || subChanged, msg, err
}

// putAudio writes want's audio when it differs, then reads the part's
// subtitle back into got, or marks it unknown when that read fails.
func (r *applyRun) putAudio(ctx context.Context, ep *streams.Episode, want *selection, got *streams.Applied) (changed bool, msg string, err error) {
	id := streams.ID(want.Audio)
	if want.Audio == nil || id == got.AudioID {
		return false, "", nil
	}
	err = r.client.SetAudioStream(ctx, plexapi.StreamSelection{PartID: got.PartID, StreamID: id})
	if err != nil {
		return false, "failed to set audio stream", err
	}
	got.AudioID = id
	if r.p.ref.Audio != nil {
		logSubstitution(ep, r.username, kindAudio, r.p.ref.Audio, want.Audio)
	}
	// On a part where the user never set a subtitle, Plex picks a new one
	// for the new audio, so the subtitle is compared with a fresh read.
	after, err := r.client.Episode(ctx, plex.RatingKey(ep.RatingKey))
	if err != nil {
		got.SubtitleID, got.SubtitleUnknown = 0, true
		return true, "failed to reload episode", err
	}
	got.SubtitleID = streams.ID(streams.Selected(after).Subtitle)
	return true, "", nil
}

// putSubtitle writes want's subtitle, or turns subtitles off, when it
// differs from got's.
func (r *applyRun) putSubtitle(ctx context.Context, ep *streams.Episode, want *selection, got *streams.Applied) (changed bool, msg string, err error) {
	switch id := streams.ID(want.Subtitle); {
	case want.SubtitleOff && got.SubtitleID != 0:
		err = r.client.DisableSubtitles(ctx, got.PartID)
		if err != nil {
			return false, "failed to disable subtitles", err
		}
		got.SubtitleID = 0
	case want.Subtitle != nil && id != got.SubtitleID:
		err = r.client.SetSubtitleStream(ctx, plexapi.StreamSelection{PartID: got.PartID, StreamID: id})
		if err != nil {
			return false, "failed to set subtitle stream", err
		}
		got.SubtitleID = id
		if r.p.ref.Subtitle != nil {
			logSubstitution(ep, r.username, kindSubtitle, r.p.ref.Subtitle, want.Subtitle)
		}
	default:
		return false, "", nil
	}
	return true, "", nil
}

// failed returns ctx.Err() for a request the context ended, which the caller
// reports without a warning; any other failure is logged and skips the target.
func (r *applyRun) failed(ctx context.Context, msg string, ep *streams.Episode, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	slog.Warn(msg, "episode", ep.ShortName(), "key", textsafe.SanitizeSingleLine(ep.RatingKey),
		"user", r.username, "error", err)
	return nil
}
