# The queue

Every accepted message is on disk before the gateway answers `250`. Delivery
happens afterwards, on a schedule, and is retried until it succeeds or the
message is older than `max_lifetime`.

## The spool

```
<spool_dir>/
    tmp/               partial writes, never referenced
    data/              message bodies
    q/                 envelopes waiting to be delivered
    inflight/          envelopes being delivered right now
    quarantine/        envelopes held back, needing a person
    dead/              envelopes given up on — metadata only
    failed-events/     audit events logservice would not take
```

Every transition is a `rename(2)` **within one filesystem**, so a crash leaves a
file in its old location or its new one, never half-written. Put the spool on one
volume; splitting it across mount points breaks that guarantee.

### Where `<spool_dir>` actually is

`outbound.spool_dir` in the deployed server profile, if it names one. If it does
not — the normal case — the gateway spools to **`<data>/queue`**, i.e.
`/var/lib/mailgw-go/queue` inside the container.

On an edge node deployed from `deploy/gateway/`, that is bind-mounted to
**`/opt/mailgw-go/queue`** on the host, which is the path to inspect and to back
up. Note it is a *sibling* of the data directory on the host but a *child* of it
inside the container.

Queue filenames carry a zero-padded due-second, so a lexical sort *is* due order
and nothing has to be opened to find the next job. That is also what lets the
scheduler sleep until something is genuinely due, rather than waking on a fixed
tick.

::: warning A crash mid-delivery can duplicate
If the process dies after a relay accepted a message but before the spool was
updated, the message is delivered again on recovery. This is inherent to any
spooling MTA — the alternative is losing mail — and it is why `inflight/` is
moved back to `q/` at startup rather than discarded.
:::

## Inspecting it

```bash
mailgw-go mailq          # everything, as a table
mailgw-go mailq -json    # the same, machine-readable
```

You get one line per envelope: its uuid, which directory it is in, its sender and
recipients, how many attempts it has had, when the next one is due, and the last
error.

## Acting on it

```bash
mailgw-go mailq flush                    # make every ready envelope due now
mailgw-go mailq flush <uuid>...          # just these
mailgw-go mailq rm <uuid>...             # delete, collecting their bodies
mailgw-go mailq release <uuid>...        # quarantine -> queue
mailgw-go mailq hold <uuid>...           # queue -> quarantine
```

All of them read the gateway's own cached configuration to find the spool, so
run them as the user the gateway runs as. `-data <dir>` overrides where that
cache is looked for.

**`flush` after a relay comes back.** Otherwise the backoff schedule keeps a
recovered relay waiting for up to eight hours.

**An envelope being delivered right now cannot be flushed, removed or held.**
Deleting the file would not cancel anything: the worker finishes and writes the
envelope back, so `rm` would resurrect exactly what it deleted.

## Retry schedule

```yaml
outbound:
    backoff: [60s, 5m, 15m, 30m, 1h, 2h, 4h, 8h]
    jitter: 0.15
    max_lifetime: 96h
    delay_warning_after: 4h
```

The list is walked one entry per attempt; past the end, the last value repeats.
`jitter` spreads retries by ±15% so a relay coming back does not meet every
deferred message at once.

At `delay_warning_after` the sender gets a warning that the message is late but
not lost — sent once. At `max_lifetime` the envelope is buried in `dead/` and the
sender gets a failure notification.

## dead/ is final

`dead/` holds **metadata only** — the body is collected when the envelope is
buried. There is deliberately no way to requeue from it: the message it described
no longer exists.

It is there so that "what happened to this message?" has an answer. A buried
envelope records every recipient, its final status, the last reply from the
relay, and whether the sender was notified.

## quarantine/ needs a person

Nothing drains it automatically. That is the point — a quarantined message is one
a rule decided a human should look at.

```bash
mailgw-go mailq                                    # find the uuid
mailgw-go mailq release <uuid>    # put it back in the queue
```

::: tip There is no console button for this yet
Configuration flows one way — the console composes bundles, gateways pull them —
so releasing quarantine from the console would need a command channel that does
not exist. It is CLI-only today.
:::

## Bodies

A message body is written once and shared by every envelope the message split
into. It is deleted when the last envelope referencing it is gone.

Which envelopes those are is resolved **from filenames**, not by opening every
queued envelope. A reference count was considered and rejected: increments and
decrements are not crash-atomic, and while a lost decrement merely leaks a file,
a lost increment would delete a body that is still live — which loses mail.

Orphaned bodies from an unclean shutdown are swept at startup, before the
listeners bind.

## Depth

```
mailgw_queue_ready         waiting
mailgw_queue_inflight      being delivered
mailgw_queue_quarantine    held for a person
mailgw_queue_dead          given up on
```

Alert on `mailgw_queue_ready` growing steadily — that is a relay that stopped
accepting. `mailgw_queue_quarantine` growing means rules are holding mail nobody
is releasing.
