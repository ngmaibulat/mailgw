package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Queue names, as an operator refers to them. These are the directory names, so
// `mailq` output and the spool layout cannot drift apart.
const (
	QueueReady      = dirReady
	QueueInflight   = dirInflight
	QueueQuarantine = dirQuarantine
	QueueDead       = dirDead
)

// inspectDirs is every directory an envelope can rest in, in the order `mailq`
// lists them: active work first, then what is waiting on a person, then what has
// been given up on.
var inspectDirs = []string{dirReady, dirInflight, dirQuarantine, dirDead}

// ErrInFlight is returned when an operator asks to change an envelope that a
// worker has already claimed.
//
// Refusing is the only safe answer. Deleting the inflight file cancels nothing:
// the worker still holds the envelope in memory and finishes its attempt with
// Reschedule, which writes the new copy BEFORE removing the old one — so the
// removal fails harmlessly and the envelope an operator just deleted reappears
// in the queue.
var ErrInFlight = errors.New("envelope is being delivered; try again shortly")

// ErrNotQueued is returned when no envelope with the given uuid exists.
var ErrNotQueued = errors.New("no such envelope")

// Entry is one envelope as `mailq` sees it: where it is, what file holds it, and
// what is in it.
type Entry struct {
	// Queue is the directory name — one of the Queue* constants.
	Queue string
	// Name is the filename within that directory.
	Name string
	// Env is the parsed envelope, nil when it could not be read.
	Env *Envelope
	// Err is why Env is nil. A torn or foreign file is exactly what an operator
	// running mailq is looking for, so it is reported rather than skipped.
	Err error
	// Due is when this envelope is next owed, for entries in the ready queue.
	Due time.Time
}

// UUID is the envelope's identifier, recovered from its filename when the file
// itself could not be read.
func (e Entry) UUID() string {
	if e.Env != nil {
		return e.Env.UUID
	}
	if id, ok := envelopeUUID(e.Queue, e.Name); ok {
		return id
	}
	return ""
}

// ListAll returns every envelope in the spool, across all four directories.
//
// Distinct from List, which walks only the two directories mail is actively
// moving through and returns bare envelopes. An operator needs the other two —
// quarantine/ is the whole reason to run mailq — and needs the filename, because
// that is what flush, rm and release act on.
func (s *Spool) ListAll() ([]Entry, error) {
	var out []Entry
	for _, d := range inspectDirs {
		entries, err := os.ReadDir(filepath.Join(s.root, d))
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			e := Entry{Queue: d, Name: ent.Name()}
			e.Env, e.Err = readEnvelope(filepath.Join(s.root, d, ent.Name()))
			// Only q/ and inflight/ carry a due-time prefix, and only inflight/
			// carries a claim prefix in front of it — stripping one from a q/
			// name would eat the due time instead.
			switch d {
			case dirReady:
				if due, ok := parseDue(ent.Name()); ok {
					e.Due = time.Unix(due, 0)
				}
			case dirInflight:
				if due, ok := parseDue(stripClaimPrefix(ent.Name())); ok {
					e.Due = time.Unix(due, 0)
				}
			}
			out = append(out, e)
		}
	}

	// Deterministic order, so two runs of mailq are comparable and a scripted
	// caller sees a stable list.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Queue != out[j].Queue {
			return queueRank(out[i].Queue) < queueRank(out[j].Queue)
		}
		if !out[i].Due.Equal(out[j].Due) {
			return out[i].Due.Before(out[j].Due)
		}
		return out[i].UUID() < out[j].UUID()
	})
	return out, nil
}

func queueRank(q string) int {
	for i, d := range inspectDirs {
		if d == q {
			return i
		}
	}
	return len(inspectDirs)
}

// find locates an envelope by uuid across every directory.
func (s *Spool) find(uuid string) (Entry, error) {
	all, err := s.ListAll()
	if err != nil {
		return Entry{}, err
	}
	for _, e := range all {
		if e.UUID() == uuid {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%q: %w", uuid, ErrNotQueued)
}

// Flush makes a queued envelope due immediately.
//
// A plain rename within q/ is enough — the due time lives in the filename, and
// the scheduler re-reads the directory. If a worker claims the envelope first
// the rename fails with ENOENT, which is reported as ErrInFlight rather than
// treated as an error: the envelope is being delivered, which is what flush was
// asking for anyway.
func (s *Spool) Flush(uuid string) error {
	e, err := s.find(uuid)
	if err != nil {
		return err
	}
	switch e.Queue {
	case dirReady:
	case dirInflight:
		return fmt.Errorf("%q: %w", uuid, ErrInFlight)
	default:
		return fmt.Errorf("%q is in %s, not the ready queue (use release)", uuid, e.Queue)
	}

	from := filepath.Join(s.root, dirReady, e.Name)
	to := filepath.Join(s.root, dirReady, readyName(s.now().UnixMilli(), uuid))
	if from == to {
		return nil
	}
	if err := os.Rename(from, to); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%q: %w", uuid, ErrInFlight)
		}
		return err
	}
	return nil
}

// Remove deletes a queued or held envelope and collects its body.
//
// Refuses anything in flight; see ErrInFlight for why deleting one would not
// only fail to cancel the delivery but resurrect the envelope afterwards.
func (s *Spool) Remove(uuid string) error {
	e, err := s.find(uuid)
	if err != nil {
		return err
	}
	if e.Queue == dirInflight {
		return fmt.Errorf("%q: %w", uuid, ErrInFlight)
	}

	if err := os.Remove(filepath.Join(s.root, e.Queue, e.Name)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%q: %w", uuid, ErrInFlight)
		}
		return err
	}
	// After the removal, so the envelope being deleted does not count as a
	// referrer to its own body.
	return s.gcBody(e.Env)
}

// Release moves a held envelope out of quarantine and back into the queue, due
// immediately.
//
// It rewrites rather than renames, and that is not incidental: quarantine/ names
// files "<uuid>.json" while the ready queue needs "<due12>.<uuid>.json". A bare
// rename produces a file whose name parseDue rejects, which ReadyAndNext skips
// entirely — the envelope would sit in q/, counted by LenAll as ready, claimed
// by nobody, and untouched by Recover. Released mail that silently never moves
// is worse than mail still visibly held.
//
// The scheduler notices without a restart: poll_interval is a ceiling precisely
// so something appearing in q/ by another hand is picked up.
func (s *Spool) Release(uuid string) error {
	e, err := s.find(uuid)
	if err != nil {
		return err
	}
	if e.Queue != dirQuarantine {
		return fmt.Errorf("%q is in %s, not quarantine", uuid, e.Queue)
	}
	if e.Env == nil {
		return fmt.Errorf("%q: cannot release an unreadable envelope: %w", uuid, e.Err)
	}

	// Attempts are reset: the message was held by a person, not failing, and it
	// should not start on the far end of a backoff ladder it never earned.
	e.Env.Attempts = 0
	e.Env.NextAt = s.now().UnixMilli()

	if err := s.Enqueue(e.Env); err != nil {
		return err
	}
	// Committed before removed, in that order — the same rule every transition
	// here follows. A crash in between leaves a duplicate that Recover resolves,
	// rather than losing the message.
	if err := os.Remove(filepath.Join(s.root, dirQuarantine, e.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Hold moves a queued envelope into quarantine, the inverse of Release.
//
// The reason it exists is that Release without it is a one-way door: an operator
// who releases a message and immediately realises the mistake has, at present,
// no way to stop it other than racing the scheduler.
func (s *Spool) Hold(uuid string) error {
	e, err := s.find(uuid)
	if err != nil {
		return err
	}
	switch e.Queue {
	case dirReady:
	case dirInflight:
		return fmt.Errorf("%q: %w", uuid, ErrInFlight)
	default:
		return fmt.Errorf("%q is already in %s", uuid, e.Queue)
	}
	if e.Env == nil {
		return fmt.Errorf("%q: cannot hold an unreadable envelope: %w", uuid, e.Err)
	}

	if err := s.Quarantine(e.Env); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.root, dirReady, e.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
