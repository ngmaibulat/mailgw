# Milestones

The full plans live in **`plans/`** — one file per milestone, status on line 3,
indexed by `plans/README.md`. This page is the orientation: what each one was
for, and which ones you should read before touching a given area.

## The arc

**M1–M2** built the gateway: SMTP, the rule DSL, spool, delivery, audit events.

**M3–M6** built Central Management, from both ends — console, identity and
wizard, config pull with versioned apply and rollback, fleet observability. This
is the through-line: a gateway stopped owning its configuration.

**M7–M8** finished the queue (bounces, `mailq`, MX, ordered shutdown) and closed
the parity gaps with Haraka (attachment scanning, inbound TLS, event replay).
After M8, nothing Haraka did was missing.

**M9–M11, M16** are defect and hardening passes from three audits.

**M12–M13** closed the two authentication gaps: the admin UI, then inbound SMTP.

**M14** gave the gateway an opinion about who a message is *from*: SPF, DKIM and
DMARC verified inbound, DKIM signed outbound, every result a rule field rather
than a config boolean — so nothing is refused that was not refused before.

**M15** finished the pair M11 started: `max.connections` bounds how many
connections exist at once, rate limits bound how *often* anything happens — per
IP, per sender, per user, per recipient domain, per failed AUTH. All off by
default, all 4xx, all read live.

**Every milestone in `plans/` is now done.**

## Which to read before changing what

| Touching | Read |
|---|---|
| the rule engine | `M2-routing-dsl.md` |
| the spool or delivery | `M1`, `M7-queue-completeness.md`, `M11`, `M16` |
| Central Management | `M3`, `M4`, `M5-config-pull.md` |
| counters | `M6-observability.md` |
| inbound TLS or attachments | `M8-parity-hardening.md` |
| the listener chain | `M10`, `M11-resource-bounds.md`, `M16` |
| the admin UI | `M12-admin-ui-auth.md` |
| AUTH or DSN | `M13-inbound-auth.md` |
| SPF, DKIM or DMARC | `M14-message-authentication.md` |
| rate limits or the listener chain's order | `M15-rate-limiting.md`, `M11`, `M16` |

## Numbers are identity

**Never reused, never renumbered.** Running order lives in the index, not in the
number. M9 was written after M8 and worked *first*; M11 and M16 were worked
before M12. None of that renumbered anything.

## Read the "What was built differently" sections

Every finished milestone has one, and they are the highest-value paragraphs in
the repository, because they record where a plan written by someone who had read
the code was still wrong.

A sample of what they contain:

- **M10** — `smtp.ErrTooLongLine` *does* reach the gateway during DATA (go-smtp
  answers it itself only for command lines), so the branch the plan declined to
  write was not dead code.
- **M11** — the connection cap has to sit *outside* the allowlist or it becomes
  the attack it was meant to stop.
- **M16** — the re-audit of M11's own code, run before any of it was committed on
  the premise that a green test run proves nothing. It found that M11 had put a
  resource *leak* into the resource-bounds milestone. **The lesson worth
  keeping: every M11 test constructed its subject directly, and three of its
  items only took effect through `cmd/mailgw-go`'s wiring, which had no test at
  all.**
- **M12** — its own plan contained a dead end: a claim code that is "consumed"
  leaves the node reachable by exactly one browser for ever.
- **M13** — the plan had the new rule fields at the wrong *stage*; helo policy
  runs before a client can possibly have authenticated, so they could never have
  matched.
- **M14** — its plan was wrong in four places, three of them the same shape:
  it named a seam that could not hold the code. The results headers cannot be
  written in `receivedHeader()` (that runs *before* DATA is read, so the DKIM
  result does not exist yet); signing cannot live in `internal/deliver`
  (`Message.Body` is a one-shot reader, and a signature has to precede bytes the
  signer already consumed); and stripping *every* inbound
  `Authentication-Results` overshoots RFC 7601 §5, which asks only for the ones
  forging our own authserv-id. **Before committing to a seam, check what it has
  already consumed and what it does not yet know.**
- **M15** — its plan asked for sliding windows and got token buckets, because a
  window's memory per key grows with the configured rate and the plan's own
  "bound the maps" then bounds something unpredictable. The deeper find is that
  **M11's placement rule inverts**: a cap on a shared semaphore must sit outside
  the allowlist, a limiter that is a *map* must sit inside it. **Ask what
  resource a control bounds before deciding where it goes.**

## Two defects worth internalising

Both were invisible for months and both were found by a milestone doing something
adjacent.

**The wrapped connection.** Wrapping an accepted connection to track a resource
hid the `*tls.Conn` from go-smtp, which finds TLS by a bare type assertion with
no unwrap interface. Cost: an `implicit_tls` listener lost its TLS identity in
rules, headers and audit rows — and lost the only pre-handshake read deadline the
server arms, so a silent peer held its slot for ever.

**The shared body filename.** A notification's body was named for the envelope it
reported on, so two notifications about one envelope overwrote each other's
bytes. A delay warning at four hours and a failure at four days could already do
it; relayed notifications made it routine.

The common shape: **a change that is locally correct, breaking an invariant held
somewhere else that nothing asserted.**
