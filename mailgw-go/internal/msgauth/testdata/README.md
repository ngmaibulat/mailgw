# Test fixtures

`test-rsa.key` and `test-ed25519.key` are **throwaway keys generated for this
test suite and committed on purpose.** They sign nothing outside `go test`, no
public half is published in any DNS zone, and they are not secrets — a secret
scanner flagging them has the right instinct and the wrong file.

They are committed rather than generated per run so the sign-then-verify
round trip is reproducible and a 2048-bit RSA keygen does not run on every
`go test`. `internal/queue/sign_test.go` reads `test-rsa.key` from here too,
deliberately: one fixture means the two suites cannot end up verifying different
things.

Regenerate with:

```bash
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out test-rsa.key
openssl genpkey -algorithm ED25519 -out test-ed25519.key
```

`headers.golden` pins the rendered `Authentication-Results` and `Received-SPF`
headers. Regenerate it with `UPDATE_GOLDEN=1 go test ./internal/msgauth/` — and
read the diff, because those two strings are a contract with every system
downstream.

The `.eml` files are stored **LF-only** so they stay diffable; `crlf()` in the
tests renders them the way they arrive over SMTP, and the DKIM cases run both
ways. A line ending is exactly the difference that makes a signature stop
verifying in production but not in a test.
