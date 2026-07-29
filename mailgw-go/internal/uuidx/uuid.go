// Package uuidx mints the hierarchical identifiers that tie a connection, its
// transactions, and their deliveries together.
//
// The hierarchy is a hard contract, asserted by tests/smtp/tests/smtp.e2e.test.ts:
//
//	Connection.uuid   X
//	Transaction.uuid  X.1        (second message on the same connection is X.2)
//	Delivery.uuid     X.1.1      (second envelope of that message is X.1.2)
//
// The e2e test finds all three rows with WHERE uuid LIKE 'X%', so every child
// must be a literal prefix extension of its parent.
package uuidx

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// ID is a connection, transaction, or delivery identifier.
type ID string

// New mints a root (connection) identifier.
//
// The alphabet matters: tests/smtp/src/smtp.ts:143 scrapes the queued reply with
// /\(([0-9A-F-]+)(?:\.\d+)?\)/i, so the rendered form must contain only hex
// digits and dashes. A canonical UUID satisfies that; uppercase matches Haraka's
// presentation.
func New() ID {
	return ID(strings.ToUpper(uuid.NewString()))
}

// Child derives the n'th descendant of id, counting from 1.
func (id ID) Child(n int) ID {
	return ID(fmt.Sprintf("%s.%d", string(id), n))
}

// Root returns the connection-level prefix — everything before the first dot.
func (id ID) Root() ID {
	s := string(id)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return ID(s[:i])
	}
	return id
}

// String renders the identifier.
func (id ID) String() string { return string(id) }

// Valid reports whether id is safe to embed in an SMTP reply and to use as a
// spool filename: hex digits, dashes, and dot-separated numeric suffixes only.
// This is a defence against ever interpolating attacker-influenced text into
// either position.
func (id ID) Valid() bool {
	parts := strings.Split(string(id), ".")

	// The root must be hex digits and dashes. Splitting rather than cutting
	// matters: it makes a trailing or doubled dot show up as an empty component
	// instead of silently vanishing.
	if parts[0] == "" {
		return false
	}
	for _, r := range parts[0] {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}

	// Every remaining component must be a non-empty decimal number.
	for _, part := range parts[1:] {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
