// Package testctl is an unauthenticated HTTP control API for configuring and
// inspecting a gateway from a test.
//
// # This must never be linked into a shipped binary
//
// It has no authentication and what it does is re-point a gateway: whoever can
// reach it can hand this node a relay credential of their choosing, or take the
// ones it already has. That is acceptable only because the binary carrying it —
// cmd/mailgw-go-test — is never built by the release path and never pushed under
// the production image name. cmd/mailgw-go does not import this package, and CI
// asserts it with `go list -deps`.
//
// Adding a token would be worse rather than better: it would imply the API is
// something you might reasonably expose, and the containment here is meant to be
// the binary boundary, not a credential.
//
// # Why it exists
//
// M18 made Central Management the only configuration source, which was right and
// is not being reversed here. The bill it left was on the test suite:
//
//   - The end-to-end suite cannot bootstrap from a clean state. tests/provision.ts
//     waits for the gateway to register itself, but a gateway only registers once
//     an operator walks the wizard behind M12's claim code, and nothing automates
//     that. `docker compose down -v && pnpm start` spins for 120s and throws.
//   - Every configuration change in a test costs a console round trip: sign in,
//     edit a profile, deploy, wait for a WebSocket or a 15s poll, then read a log
//     to find out whether it took.
//
// Enroll addresses the first by automating only the browser step — it calls the
// same agent.Register the wizard calls, and the console still has to approve the
// fingerprint, assign profiles and deploy. Config injection addresses the second
// by writing a cache row and calling the same applyCached everything else calls,
// and answering synchronously with the compiler's own error.
//
// # Layering
//
// The containment is one-directional: this package imports internal/node, and
// neither internal/node nor cmd/mailgw-go may import this. That is the property
// CI checks, and it is the only one that matters — an import going this way
// costs the shipped binary nothing.
//
// The Control interface exists for testability rather than for layering:
// *node.Node satisfies it exactly, and the handler tests use a fake so that
// routing and error mapping are covered without bringing a gateway up.
package testctl
