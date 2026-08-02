package queue

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
)

// Spool directory names.
const (
	dirTmp        = "tmp"
	dirData       = "data"
	dirReady      = "q"
	dirInflight   = "inflight"
	dirDead       = "dead"
	dirQuarantine = "quarantine"
	dirFailedEvts = "failed-events"
)

// dueWidth is the zero-padded width of the due-time filename prefix. Twelve
// digits covers Unix seconds well past year 33000, and a fixed width means a
// plain lexical sort of the directory yields due order — no file needs to be
// opened to find the next piece of work.
const dueWidth = 12

// Spool is the on-disk queue.
//
// Nothing is ever written in place: a file is built under tmp/ and renamed into
// its destination, and rename(2) within one filesystem is atomic, so a reader
// can never observe a partial envelope or a partial body.
//
// A *transition* between directories is two operations, not one — Reschedule
// and Bury commit the new copy before removing the inflight one, because the
// envelope's contents change (attempt count, statuses, due time) and a rename
// alone cannot express that. The order is deliberate: a crash in between leaves
// the envelope in both places, which is recoverable, rather than in neither,
// which would lose mail. Recover() resolves the duplicate.
type Spool struct {
	root string
	now  func() time.Time
}

// NewSpool creates the directory layout under root.
func NewSpool(root string) (*Spool, error) {
	s := &Spool{root: root, now: time.Now}
	for _, d := range []string{dirTmp, dirData, dirReady, dirInflight, dirDead, dirQuarantine, dirFailedEvts} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			return nil, fmt.Errorf("create spool dir: %w", err)
		}
	}
	return s, nil
}

// Open attaches to an existing spool without creating anything.
//
// NewSpool is for the gateway, which owns the directory and should create it.
// `mailq` is a second process, often a second user, and MkdirAll there is a trap:
// running it as root against a spool owned by the gateway user silently creates
// root-owned directories beside the 0750 ones, and the gateway then fails to
// write into its own queue. Failing with "no such spool" is the useful answer.
func Open(root string) (*Spool, error) {
	s := &Spool{root: root, now: time.Now}
	for _, d := range []string{dirTmp, dirData, dirReady, dirInflight, dirDead, dirQuarantine} {
		p := filepath.Join(root, d)
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("spool %s: %w (is this the gateway's spool_dir, and are you the user it runs as?)", root, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("spool %s: %s is not a directory", root, d)
		}
	}
	return s, nil
}

// Root returns the spool root directory.
func (s *Spool) Root() string { return s.root }

// FailedEventsDir is where undeliverable audit events are parked.
func (s *Spool) FailedEventsDir() string { return FailedEventsDirFor(s.root) }

// FailedEventsDirFor names that directory without opening the spool.
//
// `mailgw-go events` needs it on a spool Open would refuse: the four envelope
// directories can be missing or wiped and the parked audit events are still
// worth resending.
func FailedEventsDirFor(root string) string { return filepath.Join(root, dirFailedEvts) }

// WriteBody stores a message body and returns its filename.
//
// The body is written to tmp/ and then renamed into data/, so a reader can
// never observe a partial message. Bodies are shared by every envelope of a
// transaction, hence the txn-scoped name.
func (s *Spool) WriteBody(txnUUID string, r io.Reader) (name string, size int64, err error) {
	name = txnUUID + ".eml"
	tmp, err := os.CreateTemp(filepath.Join(s.root, dirTmp), "body-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp body: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	size, err = io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("write body: %w", err)
	}
	// fsync before the rename: the rename is atomic with respect to crashes,
	// but only useful if the bytes are already durable.
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return "", 0, fmt.Errorf("sync body: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("close body: %w", err)
	}
	if err = os.Rename(tmpName, filepath.Join(s.root, dirData, name)); err != nil {
		return "", 0, fmt.Errorf("commit body: %w", err)
	}
	return name, size, nil
}

// BodyPath returns the absolute path of a stored body.
func (s *Spool) BodyPath(name string) string { return filepath.Join(s.root, dirData, name) }

// OpenBody opens a stored body for reading.
func (s *Spool) OpenBody(name string) (*os.File, error) { return os.Open(s.BodyPath(name)) }

// ReadBody is OpenBody behind an io.ReadCloser, so an interface can name it
// without naming *os.File.
func (s *Spool) ReadBody(name string) (io.ReadCloser, error) { return s.OpenBody(name) }

// Enqueue writes an envelope into the ready queue, due at e.NextAt.
func (s *Spool) Enqueue(e *Envelope) error {
	if e.Version == 0 {
		e.Version = EnvelopeVersion
	}
	if err := e.validate(); err != nil {
		return err
	}
	if e.NextAt == 0 {
		e.NextAt = s.now().UnixMilli()
	}
	return s.writeEnvelope(dirReady, readyName(e.NextAt, e.UUID), e)
}

// Quarantine files an envelope in quarantine/ instead of the ready queue. It
// is never picked up by a worker; releasing it is a deliberate operator action.
func (s *Spool) Quarantine(e *Envelope) error {
	if e.Version == 0 {
		e.Version = EnvelopeVersion
	}
	if err := e.validate(); err != nil {
		return err
	}
	return s.writeEnvelope(dirQuarantine, e.UUID+".json", e)
}

// RemoveBody deletes a spooled body that no envelope will reference.
//
// A message is written to disk before the data-stage rules run, because
// deciding needs its size and headers. When those rules then reject it, this
// is what stops the spool growing without bound.
func (s *Spool) RemoveBody(name string) error {
	if name == "" {
		return nil
	}
	err := os.Remove(s.BodyPath(name))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Ready lists envelopes due at or before `at`, in due order.
func (s *Spool) Ready(at time.Time) ([]string, error) {
	names, _, _, err := s.ReadyAndNext(at)
	return names, err
}

// ReadyAndNext lists envelopes due at or before `at`, in due order, and also
// reports when the earliest *not yet* due envelope comes due.
//
// Both answers come from one ReadDir. The scheduler needs them together: what
// to run now, and how long it may sleep before something else is owed. Asking
// separately would mean reading the directory twice per wake-up for no reason.
func (s *Spool) ReadyAndNext(at time.Time) (names []string, next time.Time, hasNext bool, err error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dirReady))
	if err != nil {
		return nil, time.Time{}, false, err
	}

	cutoff := at.Unix()
	names = make([]string, 0, len(entries))
	var earliest int64

	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		n := ent.Name()
		due, ok := parseDue(n)
		if !ok {
			continue
		}
		if due > cutoff {
			if !hasNext || due < earliest {
				earliest, hasNext = due, true
			}
			continue
		}
		names = append(names, n)
	}

	// Fixed-width due prefix makes lexical order equal due order.
	sort.Strings(names)
	if hasNext {
		next = time.Unix(earliest, 0)
	}
	return names, next, hasNext, nil
}

// Claim moves a ready envelope into inflight/ and returns it.
//
// The rename is the lock: exactly one caller can succeed, so two workers can
// never process the same envelope. A miss (someone else won) returns
// os.ErrNotExist, which the caller should treat as "try the next one".
func (s *Spool) Claim(name string) (*Envelope, string, error) {
	from := filepath.Join(s.root, dirReady, name)
	inflightName := fmt.Sprintf("%d.%s", os.Getpid(), name)
	to := filepath.Join(s.root, dirInflight, inflightName)

	if err := os.Rename(from, to); err != nil {
		return nil, "", err
	}

	e, err := readEnvelope(to)
	if err != nil {
		// The file is unreadable; park it rather than spin on it forever.
		_ = os.Rename(to, filepath.Join(s.root, dirDead, inflightName))
		return nil, "", err
	}
	return e, inflightName, nil
}

// Complete removes a finished envelope and, if no sibling still references it,
// its body.
//
// The body is collected after the envelope is gone, so a crash between the two
// orphans a body. That leaks disk, never mail, and the boot-time SweepBodies
// reclaims it.
func (s *Spool) Complete(inflightName string, e *Envelope) error {
	if err := os.Remove(filepath.Join(s.root, dirInflight, inflightName)); err != nil {
		return err
	}
	return s.gcBody(e)
}

// Reschedule returns an envelope to the ready queue with a new due time.
func (s *Spool) Reschedule(inflightName string, e *Envelope, due time.Time) error {
	e.NextAt = due.UnixMilli()
	if err := s.writeEnvelope(dirReady, readyName(e.NextAt, e.UUID), e); err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.root, dirInflight, inflightName))
}

// Bury moves an envelope to dead/ — used when a message has expired, and when a
// DSN itself cannot be delivered, where generating another bounce would loop.
//
// It collects the body, so **dead/ is metadata-only**: a buried envelope records
// what happened and to whom, and there is deliberately no way to resurrect it.
// The alternative — having dead/ pin its body — means a queue that is given up
// on still consumes the disk of everything in it, with nothing that ever drains
// it. That is why gcBody does not scan dead/, and why `mailq` has no requeue
// verb.
func (s *Spool) Bury(inflightName string, e *Envelope) error {
	if err := s.writeEnvelope(dirDead, e.UUID+".json", e); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.root, dirInflight, inflightName)); err != nil {
		return err
	}
	return s.gcBody(e)
}

// Recover moves everything left in inflight/ back to the ready queue, due
// immediately, and drops any inflight copy whose envelope was already committed
// elsewhere. Run once at startup.
//
// The dedupe is what makes the two-step transitions in Reschedule and Bury safe.
// Crashing between their write and their remove leaves the same envelope in both
// q/ (or dead/) and inflight/; without this the inflight copy would be re-queued
// under a different filename — the due second is re-stamped — and the message
// would be delivered twice. The committed copy is always the newer state,
// because both writers commit the new location before removing the old one.
//
// A crash between "relay accepted the message" and "envelope removed" will still
// redeliver on restart. That duplicate is inherent to any spooling MTA — the
// alternative is losing mail — and is documented rather than papered over. This
// one was not inherent.
func (s *Spool) Recover() (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dirInflight))
	if err != nil {
		return 0, err
	}

	committed, err := s.committedUUIDs()
	if err != nil {
		return 0, err
	}

	due := s.now().Unix()
	n := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		orig := stripClaimPrefix(name)

		if uuid, ok := uuidFromReadyName(orig); ok {
			if _, dup := committed[uuid]; dup {
				if err := os.Remove(filepath.Join(s.root, dirInflight, name)); err != nil {
					return n, fmt.Errorf("recover %s: %w", name, err)
				}
				continue
			}
		}

		// Re-stamp the due time so recovered work runs now.
		if _, ok := parseDue(orig); ok {
			orig = fmt.Sprintf("%0*d%s", dueWidth, due, orig[dueWidth:])
		}
		from := filepath.Join(s.root, dirInflight, name)
		to := filepath.Join(s.root, dirReady, orig)
		if err := os.Rename(from, to); err != nil {
			return n, fmt.Errorf("recover %s: %w", name, err)
		}
		n++
	}
	return n, nil
}

// committedUUIDs collects the envelope uuids that already exist somewhere other
// than inflight/.
//
// Filenames carry the uuid in every directory, so this is a name scan — no
// envelope has to be opened, which matters because Recover runs on a queue that
// may hold everything a crashed process was working on.
//
// quarantine/ has no transition out of inflight/ today, so including it is
// inert; it is here so that adding one cannot silently reintroduce the
// duplicate this function exists to prevent.
func (s *Spool) committedUUIDs() (map[string]struct{}, error) {
	seen := map[string]struct{}{}
	for _, d := range []struct {
		dir   string
		parse func(string) (string, bool)
	}{
		{dirReady, uuidFromReadyName},
		{dirDead, uuidFromPlainName},
		{dirQuarantine, uuidFromPlainName},
	} {
		entries, err := os.ReadDir(filepath.Join(s.root, d.dir))
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			if uuid, ok := d.parse(ent.Name()); ok {
				seen[uuid] = struct{}{}
			}
		}
	}
	return seen, nil
}

// stripClaimPrefix removes the "<pid>." Claim adds, leaving the ready-queue
// filename the envelope had before it was claimed.
func stripClaimPrefix(name string) string {
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return name
	}
	if _, err := strconv.Atoi(name[:i]); err != nil {
		return name
	}
	return name[i+1:]
}

// uuidFromReadyName extracts the uuid from "<due12>.<uuid>.json". The uuid
// itself contains dots (a transaction child is "<conn>.<txn>.<envelope>"), so
// this trims the fixed-width prefix and the suffix rather than splitting.
func uuidFromReadyName(name string) (string, bool) {
	if _, ok := parseDue(name); !ok {
		return "", false
	}
	return trimEnvelopeSuffix(name[dueWidth+1:])
}

// uuidFromPlainName extracts the uuid from "<uuid>.json", the form dead/ and
// quarantine/ use.
func uuidFromPlainName(name string) (string, bool) {
	return trimEnvelopeSuffix(name)
}

func trimEnvelopeSuffix(s string) (string, bool) {
	s, ok := strings.CutSuffix(s, ".json")
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// Counts is how many envelopes sit in each directory one can come to rest in.
//
// A struct rather than four positional ints: four same-typed returns is exactly
// where a caller transposes two of them, and nothing would ever catch it.
type Counts struct {
	Ready      int
	Inflight   int
	Quarantine int
	Dead       int
	// FailedEvents is not envelopes at all — it is undeliverable audit events.
	// Counted here because this is the one place that already walks the spool,
	// and because a growing pile of them means the log tables are missing rows,
	// which nothing else reports.
	FailedEvents int
	// RejectedEvents is audit events a replay gave up on permanently. They are
	// the evidence of what logservice refused, so nothing retries them and only
	// events.rejected_retention removes them — which makes an unwatched pile the
	// exact failure mode FailedEvents was given a gauge to avoid.
	RejectedEvents int
}

// LenAll counts every directory an envelope can rest in.
//
// quarantine/ and dead/ are the point. Len() reports only the two directories
// mail is actively moving through, which is the right answer for "is this
// gateway busy?" and the wrong one for "is anything stuck?" — nothing drains
// quarantine on its own, so a backlog there is invisible until someone counts
// it.
func (s *Spool) LenAll() (Counts, error) {
	var c Counts
	for _, d := range []struct {
		dir string
		n   *int
	}{
		{dirReady, &c.Ready},
		{dirInflight, &c.Inflight},
		{dirQuarantine, &c.Quarantine},
		{dirDead, &c.Dead},
	} {
		ents, err := os.ReadDir(filepath.Join(s.root, d.dir))
		if err != nil {
			return Counts{}, err
		}
		*d.n = len(ents)
	}

	// Tolerated missing, unlike the four above: Open() does not require this
	// directory (a spool from before it existed is still a valid spool), and an
	// absent one means zero parked events, not a broken gateway.
	if ents, err := os.ReadDir(filepath.Join(s.root, dirFailedEvts)); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				c.FailedEvents++
			}
		}
	} else if !os.IsNotExist(err) {
		return Counts{}, err
	}

	// Same tolerance, and for a stronger reason: rejected/ is created lazily by
	// the first rejection, so on a healthy gateway it never exists at all.
	if ents, err := os.ReadDir(filepath.Join(s.root, dirFailedEvts, events.RejectedDir)); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				c.RejectedEvents++
			}
		}
	} else if !os.IsNotExist(err) {
		return Counts{}, err
	}

	return c, nil
}

// Len reports how many envelopes are queued (ready plus in flight).
//
// Prefer LenAll wherever quarantine/ and dead/ matter, which is anything a
// console renders.
func (s *Spool) Len() (ready, inflight int, err error) {
	c, err := s.LenAll()
	return c.Ready, c.Inflight, err
}

// List returns every queued envelope, for `mailgw-go mailq`.
func (s *Spool) List() ([]*Envelope, error) {
	var out []*Envelope
	for _, d := range []string{dirReady, dirInflight} {
		entries, err := os.ReadDir(filepath.Join(s.root, d))
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			e, err := readEnvelope(filepath.Join(s.root, d, ent.Name()))
			if err != nil {
				continue // a torn or foreign file should not break mailq
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// writeEnvelope serialises e into dir/name via tmp/ + rename.
func (s *Spool) writeEnvelope(dir, name string, e *Envelope) error {
	enc, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode envelope %q: %w", e.UUID, err)
	}

	tmp, err := os.CreateTemp(filepath.Join(s.root, dirTmp), "env-*")
	if err != nil {
		return fmt.Errorf("create temp envelope: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(enc); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write envelope: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("sync envelope: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close envelope: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.root, dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("commit envelope: %w", err)
	}
	return nil
}

// bodyDirs are the directories whose envelopes keep a body alive.
//
// quarantine/ is on the list and that is the whole point: split() buckets by
// (relay group, quarantined, headers), so one transaction can produce a queued
// envelope AND a quarantined one sharing a body. Without quarantine/ here,
// completing the queued half deletes the body out from under the held one — and
// releasing it would then hand a worker an envelope pointing at nothing.
//
// dead/ is deliberately absent; see Bury.
var bodyDirs = []string{dirReady, dirInflight, dirQuarantine}

// gcBody deletes the message body once no envelope refers to it.
//
// The scan resolves from filenames, falling back to a read.
//
// A body is data/<txnUUID>.eml, and Envelope.validate enforces that an envelope
// may only reference its own transaction's body — so an envelope whose uuid does
// not extend <txnUUID> provably does not refer to it, and its file never has to
// be opened. Almost every envelope in a queue is ruled out that way, which turns
// a completion from "parse every queued envelope" into one ReadDir per directory
// and typically zero reads.
//
// Only names that fail to parse fall through to a read, and an unreadable
// candidate is treated as a reference. Both failure directions are chosen: a
// spurious reference keeps an orphan, which costs disk and is reclaimed by
// SweepBodies; a missed one deletes a live body, which loses mail.
//
// A reference-counting file was considered and rejected. Incrementing and
// decrementing a counter is not crash-atomic: a lost decrement leaks a body,
// but a lost increment DELETES A LIVE ONE. Deriving the answer from names that
// are already there adds no state that can drift out of sync.
func (s *Spool) gcBody(e *Envelope) error {
	if e == nil || e.Body == "" {
		return nil
	}
	referenced, err := s.bodyReferenced(e.Body, e.UUID)
	if err != nil || referenced {
		return err
	}
	if err := os.Remove(s.BodyPath(e.Body)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// bodyReferenced reports whether any envelope other than `except` still needs
// the named body. An unreadable candidate counts as a reference: keeping an
// orphan costs disk, deleting a live body loses mail.
func (s *Spool) bodyReferenced(body, except string) (bool, error) {
	for _, d := range bodyDirs {
		entries, err := os.ReadDir(filepath.Join(s.root, d))
		if err != nil {
			return false, err
		}
		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			uuid, ok := envelopeUUID(d, ent.Name())
			// A name that does not parse is a foreign or torn file. It cannot be
			// ruled out on its name, so it has to be opened.
			if ok {
				if uuid == except {
					continue
				}
				// The same rule Envelope.validate enforces, so a candidate can
				// be ruled out on its filename alone.
				if !bodyOwnedBy(body, uuid) {
					continue
				}
			}
			other, err := readEnvelope(filepath.Join(s.root, d, ent.Name()))
			if err != nil {
				return true, nil
			}
			if other.UUID != except && other.Body == body {
				return true, nil
			}
		}
	}
	return false, nil
}

// envelopeUUID extracts the envelope uuid from a filename, given the directory
// it was found in.
//
// The directory matters: inflight/ names carry a "<pid>." claim prefix, and
// stripping one from a q/ name would eat the due-time prefix instead — both
// lead with a numeric dot-component and are indistinguishable on their own.
func envelopeUUID(dir, name string) (string, bool) {
	switch dir {
	case dirReady:
		return uuidFromReadyName(name)
	case dirInflight:
		return uuidFromReadyName(stripClaimPrefix(name))
	default:
		return uuidFromPlainName(name)
	}
}

// SweepBodies deletes spooled bodies no envelope references.
//
// It reclaims what a crash between Complete's remove and its gcBody leaves
// behind — a leak of disk, never of mail (see Complete).
//
// Two things keep it from deleting live mail. It runs at bring-up, BEFORE the
// listeners bind, because a session spends the whole data-stage policy pass
// between WriteBody and Enqueue and during that window a perfectly good body has
// no envelope pointing at it. And it ignores anything modified recently, so even
// a stray concurrent writer is out of range.
func (s *Spool) SweepBodies(minAge time.Duration) (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dirData))
	if err != nil {
		return 0, err
	}

	cutoff := s.now().Add(-minAge)
	n := 0
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		referenced, err := s.bodyReferenced(ent.Name(), "")
		if err != nil {
			return n, err
		}
		if referenced {
			continue
		}
		if err := os.Remove(s.BodyPath(ent.Name())); err != nil && !os.IsNotExist(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

func readEnvelope(path string) (*Envelope, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := e.validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// readyName builds "<due12>.<uuid>.json".
func readyName(dueMillis int64, uuid string) string {
	return fmt.Sprintf("%0*d.%s.json", dueWidth, dueMillis/1000, uuid)
}

// parseDue extracts the Unix second encoded in a queue filename.
func parseDue(name string) (int64, bool) {
	if len(name) < dueWidth+1 || name[dueWidth] != '.' {
		return 0, false
	}
	n, err := strconv.ParseInt(name[:dueWidth], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
