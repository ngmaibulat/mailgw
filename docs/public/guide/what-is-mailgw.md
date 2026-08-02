# What is mailgw?

mailgw is an SMTP gateway. It accepts mail on port 25 (or 465, or wherever you
put it), decides what to do with each recipient using rules you write, hands the
message to a relay, and records what happened.

It is a **relay**, not a mailbox server. It has no POP or IMAP, no local
delivery, and no user mailboxes. Mail arrives, is routed, and leaves.

## What it does

**Routes per recipient.** A message to `a@partner.com` and `b@internal.example`
becomes two envelopes, each sent wherever its own rule says. This is the thing
most simple relays get wrong: they route the whole message by its first
recipient.

**Decides as early as it can.** A rule that only looks at the recipient address
is evaluated at `RCPT TO`, so the sender is told about a rejected address on the
line where it named it — not in a bounce twenty minutes later. A rule that reads
the message body necessarily waits for the body.

**Spools before it acknowledges.** When the gateway answers `250`, the message
is already on disk. Every state change afterwards is a `rename(2)` within one
filesystem, so a crash leaves a file in its old place or its new one, never
half-written.

**Retries, then gives up honestly.** A deferred message is retried on a backoff
schedule, warned about while it is still being tried, and eventually bounced back
to its sender with the reason. A message that fails permanently bounces
immediately.

**Reports.** Every connection, transaction and delivery is posted as a
structured event to a logging service. Counters for every stage are exposed for
scraping. If the logging service is down, events are parked on disk and replayed
when it returns — mail flow never waits on the audit trail.

## What it does not do

- **It is not a mailbox server.** No local delivery, no POP, no IMAP.
- **It does not deliver direct to MX by default.** Mail goes to relays you
  configure. A relay *can* be named by domain and resolved through MX
  ([`use_mx`](/config/relays#use-mx)), but the recipient's own domain is never
  consulted to pick a destination.
- **It does not filter spam.** There is no content scoring, no RBL lookups, no
  greylisting. It will check attachment digests against a blocklist you supply,
  and it will do whatever your rules say — that is the extent of it.
- **It does not sign or verify messages.** No DKIM, no SPF, no DMARC.
- **It does not rewrite addresses.** What arrives is what is relayed.

## Two ways to run it

**From files.** Point it at a configuration directory with `-config`. Everything
lives in that directory, you edit it with an editor, and `SIGHUP` reloads it.
Good for a single gateway, for development, and for anyone who already has a
configuration-management system.

**Centrally managed.** Run it with no arguments at all. It generates an identity
on first boot, serves a small local provisioning page, and pulls its entire
configuration from a management console as a versioned bundle. An operator
approves the node once; after that, configuration is deployed and rolled back
centrally. Good for a fleet.

The two modes run the same code and the same validators. See
[Central Management](/guide/central-management).

## Where to go next

- [How a message flows](/guide/message-flow) — the path a message takes, stage
  by stage
- [Installation](/guide/installation) — getting one running
- [Routing rules](/rules/overview) — the part you will spend the most time on
