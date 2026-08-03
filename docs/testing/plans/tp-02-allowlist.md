# TP-02 · IP allowlist

**Purpose.** Verify the inbound gate: that an unlisted peer is refused before the
banner, that the check fails closed, and that allowing everything requires saying
so explicitly.

This is the only inbound authorization gate on a gateway without
[AUTH](/plans/tp-05-auth), so it is the highest-consequence plan in this
collection.

**Duration.** ~20 minutes. Requires restarting the gateway several times.

## Preconditions

- A provisioned stack. Where this plan says `ngmfilter.json`, edit the
  **`lab-allowlist` profile** in the console and press Deploy.
- Two addresses you can connect from — loopback and one other. A second container
  or a colleague's machine will do.
- [TP-01](/plans/tp-01-smoke) passed.

## Steps

### 1. A listed peer is accepted

`ngmfilter.json`:

```json
{ "allowed": ["127.0.0.1", "::1"] }
```

Restart, then from loopback:

```bash
swaks --server localhost:2525 --quit-after CONNECT
```

**Expected.** `220` banner.

### 2. An unlisted peer is refused before the banner

From your second address:

```bash
swaks --server <gateway-host>:2525 --quit-after CONNECT
```

**Expected.** `550` with text about access being denied — and **no `220` banner
first.** The refusal must come instead of the greeting, not after it.

**This is the property being tested.** A gateway that greets and then refuses has
already told an attacker it is a mail server and accepted the cost of a session.

### 3. CIDR ranges work

```json
{ "allowed": ["127.0.0.1", "::1", "10.0.0.0/8"] }
```

Restart. Connect from an address inside the range and one outside.

**Expected.** Inside: `220`. Outside: `550`.

### 4. IPv6 and mapped addresses

```json
{ "allowed": ["::1", "2001:db8::/32"] }
```

Restart, then:

```bash
swaks --server '[::1]:2525' --quit-after CONNECT
```

**Expected.** `220`. An IPv4-mapped IPv6 address (`::ffff:127.0.0.1`) is
normalised, so listing the IPv4 form alone is enough — verify by connecting over
IPv4 with only `127.0.0.1` listed.

### 5. A gateway with no allowlist denies everyone

Unassign the allowlist profile from the gateway (`/gateways/<id>`, clear the
allowlist selection) and Deploy.

**Expected.** The gateway **refuses the bundle** and says why, keeping the
configuration it is already running. It does not adopt an empty allowlist, and it
does not start allowing everything. The reason appears as `apply_error` on the
gateway's page in the console.

### 6. A malformed file denies everyone

```json
{ "allowed": "127.0.0.1" }
```

(A string where an array belongs.) Restart.

**Expected.** Refuses to start, naming the file.

### 7. An empty list is refused

```json
{ "allowed": [] }
```

Restart.

**Expected.** Refuses to start. An empty list almost always means "the file did
not load", so saying it takes a second key — which is step 8.

### 8. allow_all is explicit and loud

```json
{ "allowed": [], "allow_all": true }
```

Restart.

**Expected.**
- The gateway starts.
- It logs a **warning** that it is accepting mail from any peer.
- `check` prints the same warning.
- Your second address now gets a `220`.

**Record the exact warning text.** Something this dangerous must be visible in
the log, and this step is what confirms it is.

### 9. It hot-swaps on SIGHUP

Restore a restrictive list including only loopback, then **without restarting**:

```bash
docker compose kill -s HUP mailgw-go
```

**Expected.** The log records a reload. Your second address is now refused, with
no restart and no dropped connections.

### 10. A bad reload keeps the running list

With the gateway running and a working allowlist in force, write a malformed
`ngmfilter.json` and `SIGHUP` again.

**Expected.**
- The log records the failure and says the previous configuration is still in
  force.
- The gateway keeps running.
- The **old** allowlist still applies — connections that worked before still
  work.

**This is the all-or-nothing property.** A half-applied allowlist would be worse
than either state.

## Cleanup

Restore the original `ngmfilter.json` and restart.

## Result

| Step | Result | Notes |
|---|---|---|
| 1 listed accepted | | |
| 2 unlisted refused pre-banner | | |
| 3 CIDR | | |
| 4 IPv6 / mapped | | |
| 5 missing file | | |
| 6 malformed file | | |
| 7 empty list | | |
| 8 allow_all warns | | |
| 9 SIGHUP | | |
| 10 bad reload keeps old | | |
