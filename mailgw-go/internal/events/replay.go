package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/obs"
)

// RejectedDir holds spilled events a replay gave up on permanently. It lives
// inside the spill directory so one path is still the whole story.
const RejectedDir = "rejected"

// claimPrefix marks a file one process has taken responsibility for. A dot so a
// listing does not show it as ordinary work, and the pid so an abandoned claim
// says who abandoned it.
const claimPrefix = "."

// tornGrace is how long an unparseable file is given the benefit of the doubt.
//
// Spills are committed through a rename and so are never torn, but a directory
// this old can hold files written by an earlier build that used os.WriteFile,
// and a file being written right now by a process using one. Below this age
// "unparseable" means "wait"; above it, it means "broken".
const tornGrace = time.Minute

// claimGrace is how long a claim is left alone before it is treated as
// abandoned and returned to the pending set.
//
// A claim lives for one HTTP round trip, so anything still holding one this much
// later is a process that died between the claim and the outcome — SIGKILL past
// the grace period being the ordinary way that happens. Until it was reclaimed
// such a file was lost outright: Pending() skips it, `mailgw-go events` cannot
// see it, and no counter mentions it. Reclaiming can re-post an event whose
// removal failed after a successful POST, which is a duplicate audit row — the
// same at-least-once trade this pipeline already makes everywhere else, and much
// cheaper than a row nobody knows is missing.
const claimGrace = 15 * time.Minute

// maxConsecutiveFailures stops a pass that is clearly shouting into the void.
// Logservice being down is the normal reason for a full spill directory, and
// resending a thousand events at it one by one helps neither side.
const maxConsecutiveFailures = 3

// Replayer resends the audit events that could not be delivered when they
// happened.
//
// It is safe to run while the gateway is still spilling into the same directory,
// and safe to run from the CLI at the same time as the gateway's own background
// pass: every file is claimed with a rename first, and rename is the lock.
type Replayer struct {
	// Dir is the spill directory, normally Spool.FailedEventsDir().
	Dir string
	// Client supplies the HTTP transport, timeout and API key. Its Stats are not
	// touched; a replay is not a send.
	Client *Client
	// URLFor resolves the current endpoint for a kind. A record carries the URL
	// it was originally posted to, but a managed gateway's logservice URL changes
	// with a bundle, and replaying forever at an address recorded weeks ago helps
	// nobody. Nil falls back to the recorded URL.
	URLFor func(Kind) string
	Log    *slog.Logger
	// Retention is how long rejected/ keeps a file. Zero never deletes.
	//
	// Those files are the evidence of what logservice refused, which is why they
	// are kept at all — but nothing else drains that directory, so without this
	// it grows for the life of the installation and only an operator running
	// `mailgw-go events rm` ever empties it.
	Retention time.Duration
	// Metrics may be nil.
	Metrics *obs.Metrics
	// now exists so tests can age a file without sleeping.
	now func() time.Time
}

// Result is what one pass did.
type Result struct {
	// Replayed were accepted and deleted.
	Replayed int
	// Rejected were refused 4xx and moved to rejected/ — terminal, because an
	// identical body cannot start passing.
	Rejected int
	// Deferred were left where they are, for the next pass.
	Deferred int
}

// Total is how many files the pass touched.
func (r Result) Total() int { return r.Replayed + r.Rejected + r.Deferred }

func (r *Replayer) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Replayer) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Pending lists the spilled events waiting to be replayed, oldest first.
//
// Claimed files are skipped: something is already dealing with them, and showing
// them as work would invite an operator to act on both copies.
func (r *Replayer) Pending() ([]string, error) {
	ents, err := os.ReadDir(r.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A gateway that has never failed to post an event has no such
			// directory. That is the healthy case, not an error.
			return nil, nil
		}
		return nil, err
	}

	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") ||
			strings.HasPrefix(e.Name(), claimPrefix) {
			continue
		}
		names = append(names, e.Name())
	}
	// The fixed-width nanosecond prefix makes lexical order chronological, so
	// the oldest event is retried first and the log tables fill in order.
	sort.Strings(names)
	return names, nil
}

// Reclaim returns abandoned claims to the pending set and reports how many.
//
// A claim is the lock a replay takes over one file, and nothing used to release
// one that its holder never got back to: the process is gone, the file keeps a
// name Pending() skips, and that audit row is invisible to the CLI, to the next
// pass and to every counter. Spool.LenAll does still count it, which is why the
// symptom is a gauge that never returns to zero over a listing that shows
// nothing.
//
// A claim whose timestamp cannot be read is left alone. It was written by a
// build that named claims differently, and guessing at an unknown format is how
// a file that is genuinely in flight gets posted twice.
func (r *Replayer) Reclaim() (int, error) {
	ents, err := os.ReadDir(r.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := r.clock().Add(-claimGrace)
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), claimPrefix+"claim-") {
			continue
		}
		orig, at, ok := parseClaim(e.Name())
		if !ok || !at.Before(cutoff) {
			continue
		}
		r.log().Warn("reclaiming an abandoned audit event; the process holding "+
			"it never finished", "file", orig, "claimed", at)
		r.unclaim(filepath.Join(r.Dir, e.Name()), filepath.Join(r.Dir, orig))
		n++
	}
	return n, nil
}

// parseClaim splits ".claim-<pid>-<unixnano>-<name>" back up.
func parseClaim(name string) (orig string, at time.Time, ok bool) {
	rest, found := strings.CutPrefix(name, claimPrefix+"claim-")
	if !found {
		return "", time.Time{}, false
	}
	// The original name contains dashes of its own, so exactly two fields come
	// off the front.
	parts := strings.SplitN(rest, "-", 3)
	if len(parts) != 3 {
		return "", time.Time{}, false
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || parts[2] == "" {
		return "", time.Time{}, false
	}
	return parts[2], time.Unix(0, nanos), true
}

// RunOnce replays everything currently pending.
//
// It returns what it managed rather than an error for an event that failed: one
// unusable record must not stop the other nine hundred from being delivered.
func (r *Replayer) RunOnce(ctx context.Context) (Result, error) {
	var res Result

	// Before listing, so anything an earlier process abandoned is part of this
	// pass rather than of no pass at all.
	if _, err := r.Reclaim(); err != nil {
		r.log().Warn("cannot reclaim abandoned audit events", "dir", r.Dir, "err", err)
	}

	names, err := r.Pending()
	if err != nil {
		return res, err
	}
	if len(names) == 0 {
		return res, nil
	}

	fails := 0
	for _, name := range names {
		// Checked between files rather than only around the POST: a shutdown
		// should stop the pass promptly, and everything not yet claimed is
		// simply still pending.
		if ctx.Err() != nil {
			return res, nil
		}

		switch r.one(ctx, name) {
		case outcomeReplayed:
			res.Replayed++
			fails = 0
		case outcomeRejected:
			res.Rejected++
			fails = 0
		default:
			res.Deferred++
			fails++
			if fails >= maxConsecutiveFailures {
				r.log().Warn("stopping the replay pass; logservice is not taking events",
					"replayed", res.Replayed, "remaining", len(names)-res.Total())
				return res, nil
			}
		}
	}
	return res, nil
}

type outcome int

const (
	outcomeDeferred outcome = iota
	outcomeReplayed
	outcomeRejected
)

// one claims a single spilled event and tries to deliver it.
func (r *Replayer) one(ctx context.Context, name string) outcome {
	src := filepath.Join(r.Dir, name)
	// The claim carries WHEN it was taken, in the name. It cannot carry it in the
	// mtime: rename(2) preserves that, so a claim's mtime is the moment the
	// event was spilled, and stamping it instead would break handleUnparseable,
	// which reads the same field to decide whether a file may still be half
	// written.
	claim := filepath.Join(r.Dir, fmt.Sprintf("%sclaim-%d-%d-%s",
		claimPrefix, os.Getpid(), r.clock().UnixNano(), name))

	// The rename is the lock: exactly one caller can succeed, so the gateway's
	// background pass and an operator's `mailgw-go events replay` cannot both
	// post the same row.
	if err := os.Rename(src, claim); err != nil {
		// Somebody else got there first, or it has already been dealt with.
		return outcomeDeferred
	}

	raw, err := os.ReadFile(claim)
	if err != nil {
		r.unclaim(claim, src)
		return outcomeDeferred
	}

	var rec SpillRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Kind == "" {
		return r.handleUnparseable(claim, src, name, err)
	}

	url := r.urlFor(Kind(rec.Kind), rec.URL)
	if url == "" {
		r.log().Error("spilled event has no endpoint to replay to",
			"file", name, "kind", rec.Kind)
		return r.reject(claim, name)
	}

	status, err := r.Client.post(ctx, url, rec.Body)
	switch {
	case err == nil && status >= 200 && status < 300:
		if err := os.Remove(claim); err != nil {
			r.log().Error("replayed an event but could not remove its file; "+
				"it will be reclaimed and sent again",
				"file", claim, "grace", claimGrace, "err", err)
		}
		r.count(func(m *obs.Metrics) { m.EventsReplayed.Add(1) })
		return outcomeReplayed

	case err == nil && status >= 400 && status < 500:
		// The same reasoning deliver() applies: the payload does not match the
		// server's schema, so an identical body will be refused forever.
		r.log().Error("logservice refused a replayed event permanently",
			"file", name, "kind", rec.Kind, "status", status, "spilled_because", rec.Reason)
		return r.reject(claim, name)

	default:
		r.unclaim(claim, src)
		return outcomeDeferred
	}
}

// handleUnparseable decides between "wait" and "broken" for a record that will
// not decode.
func (r *Replayer) handleUnparseable(claim, src, name string, cause error) outcome {
	if fi, err := os.Stat(claim); err == nil && r.clock().Sub(fi.ModTime()) < tornGrace {
		// Recent enough that it may still be being written by something that
		// does not commit through a rename. Put it back and look again later.
		r.unclaim(claim, src)
		return outcomeDeferred
	}
	r.log().Error("spilled event is unreadable and is being set aside",
		"file", name, "err", errOr(cause, errors.New("no kind recorded")))
	return r.reject(claim, name)
}

// reject files a record under rejected/, where it stops being retried but does
// not stop existing: it is the evidence of what logservice would not accept.
func (r *Replayer) reject(claim, name string) outcome {
	dir := filepath.Join(r.Dir, RejectedDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		r.log().Error("cannot create the rejected directory", "dir", dir, "err", err)
		return outcomeDeferred
	}
	dst := filepath.Join(dir, name)
	if err := os.Rename(claim, dst); err != nil {
		r.log().Error("cannot file a rejected event", "file", name, "err", err)
		return outcomeDeferred
	}
	// Stamped, because rename(2) does not touch mtime — it changes ctime — so
	// without this the retention sweep measures age since the event was SPILLED.
	// An event that failed to post a month ago and is refused today would then be
	// filed and deleted on the same pass, destroying the evidence before anybody
	// could see it. Best-effort: a sweep that runs early is a far smaller problem
	// than not filing the record at all.
	if now := r.clock(); !now.IsZero() {
		_ = os.Chtimes(dst, now, now)
	}
	r.count(func(m *obs.Metrics) { m.EventsReplayFailed.Add(1) })
	return outcomeRejected
}

// SweepRejected removes rejected events older than Retention and reports how
// many went.
//
// Shaped like Spool.SweepBodies: an age, a count, and a missing directory is
// zero rather than an error — rejected/ is created by the first rejection, so on
// a healthy gateway it never exists.
//
// Age is taken from the file's mtime, which is when the rename into rejected/
// happened rather than when the event was originally spilled. That is the more
// useful clock here: retention is "how long we keep evidence after deciding it
// is evidence", and reading every record just to recover SpillRecord.At would
// cost an open per file to move the boundary by minutes.
//
// One unreadable file does not stop the pass, for the same reason RunOnce keeps
// going after a failed post.
func (r *Replayer) SweepRejected() (int, error) {
	if r.Retention <= 0 {
		return 0, nil
	}
	dir := filepath.Join(r.Dir, RejectedDir)

	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := r.clock().Add(-r.Retention)
	removed := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			r.log().Warn("cannot remove an expired rejected event",
				"file", e.Name(), "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// unclaim returns a file to the pending set. A failure here is loud because the
// alternative is an event nobody will ever look at again.
func (r *Replayer) unclaim(claim, src string) {
	if err := os.Rename(claim, src); err != nil {
		r.log().Error("cannot release a claimed event; it will need moving by hand",
			"claim", claim, "err", err)
	}
}

func (r *Replayer) urlFor(kind Kind, recorded string) string {
	if r.URLFor != nil {
		if u := r.URLFor(kind); u != "" {
			return u
		}
	}
	return recorded
}

func (r *Replayer) count(f func(*obs.Metrics)) {
	if r.Metrics != nil {
		f(r.Metrics)
	}
}

// Run replays on a fixed interval until ctx is cancelled.
//
// A slow interval on purpose. This is a repair mechanism for an outage that has
// already ended, not a delivery path — the delivery path is Send, and it has its
// own retries. It also runs once immediately, so a gateway restarted after an
// outage does not wait a full interval to catch up.
func (r *Replayer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}

	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		res, err := r.RunOnce(ctx)
		if err != nil {
			r.log().Warn("cannot read the failed-events directory", "dir", r.Dir, "err", err)
		} else if res.Total() > 0 {
			r.log().Info("replayed spilled audit events",
				"replayed", res.Replayed, "rejected", res.Rejected, "deferred", res.Deferred)
		}

		// After the pass, not before: this pass may have just filed something
		// under rejected/, and deleting on the same tick it arrived would be
		// surprising even when Retention makes it impossible. Logged at Info
		// whenever it deletes anything — destroying the record of what
		// logservice refused should never be silent.
		if n, err := r.SweepRejected(); err != nil {
			r.log().Warn("cannot sweep rejected audit events", "dir", r.Dir, "err", err)
		} else if n > 0 {
			r.log().Info("removed rejected audit events past their retention",
				"count", n, "retention", r.Retention)
		}

		t.Reset(interval)
	}
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
