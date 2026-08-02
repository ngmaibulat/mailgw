package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/ngmaibulat/mailgw/mailgw-go/internal/config"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/events"
	"github.com/ngmaibulat/mailgw/mailgw-go/internal/queue"
)

const eventsUsage = `usage: mailgw-go events [subcommand] [-config <dir> | -data <dir>] [flags]

Inspect and replay audit events that logservice would not accept.

  events                 list what is parked in failed-events/
  events replay          resend everything parked, oldest first
  events rm <file>...    delete parked events without sending them

flags:
  -json     emit the listing as JSON instead of a table
  -all      list rejected/ as well (events a replay gave up on permanently)

An event lands in failed-events/ when the buffer could not be drained before
shutdown, when logservice was unreachable after every retry, or when it answered
4xx. The first two are worth replaying; the third is not, and a replay files it
under failed-events/rejected/ rather than trying forever.

Safe to run against a live gateway: every file is claimed with a rename before it
is sent, so this and the gateway's own background pass cannot both post the same
row. That pass runs every events.replay_interval (default 5m, 0 disables it), so
this command is for when you do not want to wait.

Replay posts to the endpoints in the CURRENT configuration, not the ones recorded
when the event failed — a managed gateway's logservice URL arrives in a bundle
and can change.
`

// runEvents implements the `events` subcommand group.
func runEvents(args []string) int {
	sub := "list"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		sub, args = args[0], args[1:]
	}

	var asJSON, all bool
	var fs *flag.FlagSet
	o := mustParse("events", args, func(f *flag.FlagSet) {
		fs = f
		f.BoolVar(&asJSON, "json", false, "emit JSON instead of a table")
		f.BoolVar(&all, "all", false, "include rejected/ in the listing")
	})
	names := fs.Args()

	switch sub {
	case "list", "replay", "rm":
	default:
		fmt.Fprintf(os.Stderr, "unknown events subcommand %q\n\n%s", sub, eventsUsage)
		return 2
	}

	// The same resolution mailq uses, and for the same reason: a managed node's
	// spool is wherever its bundle put it, not <data>/queue.
	cfg, err := loadForEvents(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "events: %v\n", err)
		return 1
	}
	// Not queue.Open: that requires the four envelope directories, and
	// failed-events/ is meaningful on its own — a gateway whose spool was wiped
	// can still have audit events worth resending.
	dir := queue.FailedEventsDirFor(cfg.Server.Outbound.SpoolDir)

	switch sub {
	case "list":
		return eventsList(dir, all, asJSON)
	case "rm":
		return eventsRemove(dir, names)
	default:
		return eventsReplay(cfg, dir)
	}
}

// loadForEvents resolves the configuration the running gateway would use, so
// both the spool location and the logservice endpoints come from one place.
func loadForEvents(o opts) (*config.Config, error) {
	l, _, release, err := loadFor(o)
	if err != nil {
		return nil, err
	}
	defer release()
	return l.cfg, nil
}

type eventRow struct {
	File   string `json:"file"`
	Queue  string `json:"queue"` // pending | rejected
	Kind   string `json:"kind,omitempty"`
	URL    string `json:"url,omitempty"`
	At     string `json:"at,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
}

func eventsList(dir string, all, asJSON bool) int {
	rows, err := collectEvents(dir, all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "events: %v\n", err)
		return 1
	}

	if asJSON {
		out, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "events: %v\n", err)
			return 1
		}
		fmt.Println(string(out))
		return 0
	}

	fmt.Fprintf(os.Stderr, "# %s\n", dir)
	if len(rows) == 0 {
		fmt.Println("nothing parked; every audit event has reached logservice")
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "QUEUE\tKIND\tSPILLED\tWHY\tFILE")
	for _, r := range rows {
		why := r.Reason
		if r.Error != "" {
			why = "UNREADABLE: " + r.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Queue, dash(r.Kind), dash(r.At), dash(why), r.File)
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "events: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stderr, "\n# %d parked event(s)\n", len(rows))
	if !all {
		fmt.Fprintf(os.Stderr,
			"# add -all to include failed-events/rejected/, which a replay will not retry\n")
	}
	return 0
}

func collectEvents(dir string, all bool) ([]eventRow, error) {
	var rows []eventRow

	pending, err := (&events.Replayer{Dir: dir}).Pending()
	if err != nil {
		return nil, err
	}
	for _, n := range pending {
		rows = append(rows, describeEvent(dir, n, "pending"))
	}
	if !all {
		return rows, nil
	}

	rejDir := filepath.Join(dir, events.RejectedDir)
	ents, err := os.ReadDir(rejDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rows, nil
		}
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() {
			rows = append(rows, describeEvent(rejDir, e.Name(), "rejected"))
		}
	}
	return rows, nil
}

// describeEvent reports an unreadable file rather than skipping it — the same
// choice mailq's ListAll makes, and for the same reason: a torn or foreign file
// is exactly what someone running this command is looking for.
func describeEvent(dir, name, queueName string) eventRow {
	row := eventRow{File: name, Queue: queueName}

	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		row.Error = err.Error()
		return row
	}
	var rec events.SpillRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		row.Error = err.Error()
		return row
	}
	row.Kind, row.URL, row.At, row.Reason = rec.Kind, rec.URL, rec.At, rec.Reason
	return row
}

func eventsReplay(cfg *config.Config, dir string) int {
	c := events.New(events.Options{
		Timeout: cfg.Server.Events.Timeout.D(),
		Senders: 1,
		APIKey:  logserviceAPIKey(cfg),
	})
	defer c.Close(context.Background())

	r := &events.Replayer{
		Dir:    dir,
		Client: c,
		URLFor: logserviceURLFor(cfg),
	}

	res, err := r.RunOnce(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "events replay: %v\n", err)
		return 1
	}

	fmt.Printf("replayed %d, rejected %d, still parked %d\n",
		res.Replayed, res.Rejected, res.Deferred)
	if res.Deferred > 0 {
		// Not an error exit: the events are safe where they are and the next
		// pass will find them. But a script watching this should know.
		fmt.Fprintln(os.Stderr,
			"# some events could not be delivered; is logservice reachable from this host?")
	}
	if res.Rejected > 0 {
		fmt.Fprintf(os.Stderr,
			"# %d event(s) were refused permanently and moved to %s/%s\n",
			res.Rejected, dir, events.RejectedDir)
	}
	return 0
}

func eventsRemove(dir string, names []string) int {
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "events rm: name at least one file (see `mailgw-go events`)")
		return 2
	}

	failed := 0
	for _, n := range names {
		// filepath.Base so an argument copied from a listing cannot walk out of
		// the spool: this command may well be run as root.
		base := filepath.Base(n)
		path := filepath.Join(dir, base)
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(dir, events.RejectedDir, base)
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(os.Stderr, "rm: %v\n", err)
			failed++
			continue
		}
		fmt.Printf("rm: %s\n", base)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
