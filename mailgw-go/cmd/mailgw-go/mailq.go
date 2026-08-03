package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/store"
)

const mailqUsage = `usage: mailgw-go mailq [subcommand] [-config <dir> | -data <dir>] [flags]

Inspect and manage the outbound queue.

  mailq                     list every envelope in the spool
  mailq flush [uuid...]     make envelopes due now (all ready ones if none named)
  mailq rm <uuid>...        delete envelopes and collect their bodies
  mailq release <uuid>...   move envelopes out of quarantine, back into the queue
  mailq hold <uuid>...      move queued envelopes into quarantine

flags:
  -json     emit the listing as JSON instead of a table
  -q <name> list only one queue: q, inflight, quarantine or dead

Run this as the user the gateway runs as. The spool directories are 0750 and
this command deliberately will not create them: a root-owned directory beside
the gateway's own is far harder to notice than "no such spool".

An envelope currently being delivered cannot be flushed, removed or held.
Deleting its file would not cancel the delivery — the worker finishes and
rewrites it — so those are refused rather than pretended.

Released mail is picked up without a restart: outbound.poll_interval is a
ceiling, so the scheduler rescans regardless of whether anything nudged it.

The spool is found from -config, or from the cached bundle under -data. As with
` + "`config show`" + `, -data is opened read-write.
`

// runMailq implements the `mailq` subcommand group.
func runMailq(args []string) int {
	sub := "list"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}

	var asJSON bool
	var only string
	var fs *flag.FlagSet
	o := mustParse("mailq", args, func(f *flag.FlagSet) {
		fs = f
		f.BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
		f.StringVar(&only, "q", "", "list only one queue: q, inflight, quarantine or dead")
	})

	// The flag set's own remainder, not a hand-rolled scan for arguments that do
	// not start with a dash — that would take the VALUE of `-config <dir>` for an
	// envelope uuid. As everywhere else in this binary, flags come before
	// positional arguments.
	uuids := fs.Args()

	// Checked before the configuration is touched, so a typo reports the typo
	// rather than whatever happens to be wrong with the config directory.
	switch sub {
	case "list", "flush", "rm", "release", "hold":
	default:
		fmt.Fprintf(os.Stderr, "unknown mailq subcommand %q\n\n%s", sub, mailqUsage)
		return 2
	}

	spoolDir, err := resolveSpoolDir(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailq: %v\n", err)
		return 1
	}
	sp, err := queue.Open(spoolDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailq: %v\n", err)
		return 1
	}

	switch sub {
	case "list":
		return mailqList(sp, only, asJSON)
	case "flush":
		return mailqFlush(sp, uuids)
	case "rm":
		return mailqAct(sp, "rm", uuids, sp.Remove)
	case "release":
		return mailqAct(sp, "release", uuids, sp.Release)
	default: // "hold"; the set was validated above
		return mailqAct(sp, "hold", uuids, sp.Hold)
	}
}

// resolveSpoolDir finds the queue the same way the running gateway would.
//
// The answer comes from the cached bundle rather than from <data>/queue,
// because an operator who set outbound.spool_dir in a server profile would
// otherwise be shown an empty queue and conclude their mail had vanished.
func resolveSpoolDir(o opts) (string, error) {
	st, err := store.Open(o.dataDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	fallback := filepath.Join(o.dataDir, "queue")
	cached := bootConfig(st, newLogger(config.LogConfig{}))
	if cached == nil {
		// Nothing deployed yet. The gateway would use the same default, and an
		// empty queue there is the truthful answer.
		return fallback, nil
	}
	l, err := loadBundle(cached.Bundle, config.BundleOptions{SpoolDir: fallback}, bundleSource(cached))
	if err != nil {
		return "", err
	}
	return l.cfg.Server.Outbound.SpoolDir, nil
}

func mailqList(sp *queue.Spool, only string, asJSON bool) int {
	entries, err := sp.ListAll()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailq: %v\n", err)
		return 1
	}
	if only != "" {
		entries = filterQueue(entries, only)
	}

	if asJSON {
		return emitJSON(entries)
	}
	return emitTable(sp, entries)
}

func filterQueue(entries []queue.Entry, only string) []queue.Entry {
	out := entries[:0]
	for _, e := range entries {
		if e.Queue == only {
			out = append(out, e)
		}
	}
	return out
}

// row is the JSON shape. Deliberately flat and stable: this is what a monitoring
// script parses, so it must not be the internal Envelope struct.
type row struct {
	Queue      string   `json:"queue"`
	UUID       string   `json:"uuid"`
	Sender     string   `json:"sender"`
	Rcpts      []string `json:"rcpts"`
	RelayGroup string   `json:"relay_group,omitempty"`
	Size       int64    `json:"size"`
	Attempts   int      `json:"attempts"`
	QueuedAt   string   `json:"queued_at,omitempty"`
	Due        string   `json:"due,omitempty"`
	IsDSN      bool     `json:"is_dsn,omitempty"`
	LastErr    string   `json:"last_error,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func emitJSON(entries []queue.Entry) int {
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		r := row{Queue: e.Queue, UUID: e.UUID()}
		if e.Err != nil {
			r.Error = e.Err.Error()
		}
		if !e.Due.IsZero() {
			r.Due = e.Due.Format(time.RFC3339)
		}
		if e.Env != nil {
			r.Sender = e.Env.MailFrom
			r.Rcpts = e.Env.RcptAddrs()
			r.RelayGroup = e.Env.RelayGrp
			r.Size = e.Env.BodySize
			r.Attempts = e.Env.Attempts
			r.QueuedAt = time.UnixMilli(e.Env.QueuedAt).Format(time.RFC3339)
			r.IsDSN = e.Env.IsDSN
			r.LastErr = e.Env.LastErr
		}
		rows = append(rows, r)
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mailq: %v\n", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

func emitTable(sp *queue.Spool, entries []queue.Entry) int {
	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "# %s\n", sp.Root())
		fmt.Println("the queue is empty")
		return 0
	}

	fmt.Fprintf(os.Stderr, "# %s\n", sp.Root())

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUEUE\tUUID\tAGE\tTRIES\tDUE\tSIZE\tSENDER\tRECIPIENTS")

	now := time.Now()
	for _, e := range entries {
		if e.Env == nil {
			fmt.Fprintf(w, "%s\t%s\t-\t-\t-\t-\tUNREADABLE\t%v\n", e.Queue, e.Name, e.Err)
			continue
		}
		env := e.Env
		sender := env.MailFrom
		if sender == "" {
			// The null sender is a value, not an absence, and the difference
			// matters when reading a queue full of bounces.
			sender = "<>"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			e.Queue, env.UUID,
			short(now.Sub(time.UnixMilli(env.QueuedAt))),
			env.Attempts,
			dueText(now, e.Due),
			size(env.BodySize),
			sender,
			strings.Join(env.RcptAddrs(), ", "))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "mailq: %v\n", err)
		return 1
	}

	summarise(entries)
	return 0
}

func summarise(entries []queue.Entry) {
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Queue]++
	}
	names := make([]string, 0, len(counts))
	for q := range counts {
		names = append(names, q)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, q := range names {
		parts = append(parts, fmt.Sprintf("%d %s", counts[q], q))
	}
	fmt.Fprintf(os.Stderr, "\n# %d envelopes: %s\n", len(entries), strings.Join(parts, ", "))
	if counts[queue.QueueQuarantine] > 0 {
		fmt.Fprintf(os.Stderr,
			"# nothing drains quarantine on its own; release with `mailgw-go mailq release <uuid>`\n")
	}
}

// mailqFlush makes envelopes due now. With no uuids it flushes the whole ready
// queue, which is the "the relay is back, stop waiting" gesture.
func mailqFlush(sp *queue.Spool, uuids []string) int {
	if len(uuids) == 0 {
		entries, err := sp.ListAll()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mailq flush: %v\n", err)
			return 1
		}
		for _, e := range entries {
			if e.Queue == queue.QueueReady {
				uuids = append(uuids, e.UUID())
			}
		}
		if len(uuids) == 0 {
			fmt.Fprintln(os.Stderr, "mailq flush: the ready queue is empty")
			return 0
		}
	}
	return mailqAct(sp, "flush", uuids, sp.Flush)
}

// mailqAct applies one operation to each named envelope, reporting per uuid and
// continuing past failures — an operator naming ten messages should not have the
// run abandoned because the third is being delivered right now.
func mailqAct(sp *queue.Spool, verb string, uuids []string, fn func(string) error) int {
	if len(uuids) == 0 {
		fmt.Fprintf(os.Stderr, "mailq %s: name at least one envelope uuid\n", verb)
		return 2
	}

	failed := 0
	for _, u := range uuids {
		switch err := fn(u); {
		case err == nil:
			fmt.Printf("%s: %s\n", verb, u)
		case errors.Is(err, queue.ErrInFlight):
			// Not counted as a failure of the command: the envelope is busy,
			// and the operator is being told to try again rather than that
			// something is broken.
			fmt.Fprintf(os.Stderr, "%s: %v\n", verb, err)
			failed++
		default:
			fmt.Fprintf(os.Stderr, "%s: %v\n", verb, err)
			failed++
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func dueText(now, due time.Time) string {
	if due.IsZero() {
		return "-"
	}
	if !due.After(now) {
		return "now"
	}
	return "in " + short(due.Sub(now))
}

// short renders a duration the way a queue listing wants it: coarse, and never
// wider than a few characters.
func short(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func size(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%dK", n/1024)
	default:
		return fmt.Sprintf("%dM", n/(1024*1024))
	}
}
