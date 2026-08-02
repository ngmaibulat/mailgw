# IP allowlist

`ngmfilter.json` decides who may connect at all. It is checked before the banner
is written, so a peer that is not on it never sees a greeting — it gets
`550 Access denied` and the connection closes.

```json
{
    "allowed": [
        "127.0.0.1",
        "10.0.0.0/8",
        "192.168.1.50",
        "::1",
        "2001:db8::/32"
    ]
}
```

Bare addresses and CIDR prefixes, IPv4 and IPv6. An IPv4-mapped IPv6 address
(`::ffff:10.0.0.1`) is normalised, so listing the IPv4 form is enough.

## It fails closed

A missing file, a malformed file, a `allowed` that is not an array — every one of
these **denies everyone**, and the gateway refuses to start rather than serving
with a gate it could not read.

That is the right failure direction for the only thing standing between the
internet and an open relay. The zero value denies too, so even a code path that
ignored a load error could not accidentally open it.

## Allowing everything

An empty list is refused, because an empty list almost always means "the file did
not load" rather than "allow nobody". Saying it on purpose takes a second key:

```json
{ "allowed": [], "allow_all": true }
```

`check` and the startup log both warn loudly when this is set. Combine it with
[inbound AUTH](/config/auth) or you have an open relay.

## Behind a load balancer

If an L4 balancer sits in front, every connection appears to come from the
balancer — so allowlisting it admits everything behind it. Turn on
[PROXY protocol](/config/server#listeners) on that listener so the allowlist sees
the real client.

## Reloading

The allowlist is one of the two things that hot-swap. `SIGHUP` in file mode, or a
deploy in managed mode, and the new list applies to the next connection. Existing
sessions are unaffected — they were already admitted.
