package store

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Identity is this gateway's cryptographic identity.
//
// There are no enrollment tokens and no shared secrets: the keypair is
// generated here on first boot, the public half is registered with Central
// Management, and an operator approves the Fingerprint. Nothing is ever copied
// between the two sides by hand.
type Identity struct {
	// GatewayUID is assigned by Central Management at registration, and is
	// empty until then.
	GatewayUID string
	// PublicKey is the raw 32 bytes, which is what the wire format and the
	// fingerprint are both defined over.
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	// Fingerprint is sha256(PublicKey) in lowercase hex — the string an
	// operator compares between this gateway's status page and the console.
	Fingerprint string
	CreatedAt   time.Time
}

// Fingerprint computes the canonical fingerprint of a raw public key.
//
// Must match webui-fastify/src/agent/verify.ts#fingerprintFromBase64, which
// hashes the raw 32 bytes — not their base64 text.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

// Identity returns this gateway's identity, generating it on the first call.
//
// It is idempotent under concurrency three times over, any one of which would
// be sufficient: the pool is limited to a single connection, the transaction is
// IMMEDIATE and re-reads before inserting, and `id INTEGER PRIMARY KEY CHECK
// (id = 1)` makes a second insert a constraint violation rather than a second
// keypair. Two identities would be unrecoverable — the console would hold a
// public key whose private half the gateway had already forgotten.
func (s *Store) Identity() (*Identity, error) {
	ctx := context.Background()

	// The overwhelmingly common case is that it already exists.
	if id, err := readIdentity(ctx, s.db); err == nil {
		return id, nil
	} else if !errors.Is(err, errNotFound) {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE, per the DSN
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

	// Re-read inside the transaction: whoever lost the race returns the
	// winner's key rather than overwriting it.
	if id, err := readIdentity(ctx, tx); err == nil {
		return id, nil
	} else if !errors.Is(err, errNotFound) {
		return nil, err
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("generate identity: unexpected public key type")
	}
	now := time.Now()

	// private_key holds the 32-byte SEED, not the 64-byte expanded key.
	// NewKeyFromSeed rebuilds the expanded form on read, so there is only one
	// representation of the secret on disk.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO identity (id, gateway_uid, public_key, private_key, fingerprint, created_at)
		 VALUES (1, '', ?, ?, ?, ?)`,
		[]byte(pub), seed, Fingerprint(pub), now.Unix())
	if err != nil {
		return nil, fmt.Errorf("generate identity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("generate identity: commit: %w", err)
	}

	return &Identity{
		PublicKey:   pub,
		PrivateKey:  priv,
		Fingerprint: Fingerprint(pub),
		CreatedAt:   now,
	}, nil
}

func readIdentity(ctx context.Context, q querier) (*Identity, error) {
	var (
		uid         string
		pub, seed   []byte
		fingerprint string
		createdAt   int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT gateway_uid, public_key, private_key, fingerprint, created_at
		   FROM identity WHERE id = 1`).
		Scan(&uid, &pub, &seed, &fingerprint, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read identity: %w", err)
	}
	// Guard before NewKeyFromSeed, which panics on a wrong-sized seed. A
	// truncated key should be a legible error, not a crash loop.
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("read identity: stored private key is %d bytes, want %d",
			len(seed), ed25519.SeedSize)
	}

	return &Identity{
		GatewayUID:  uid,
		PublicKey:   ed25519.PublicKey(pub),
		PrivateKey:  ed25519.NewKeyFromSeed(seed),
		Fingerprint: fingerprint,
		CreatedAt:   time.Unix(createdAt, 0),
	}, nil
}

// SetGatewayUID records the id Central Management assigned at registration.
func (s *Store) SetGatewayUID(uid string) error {
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE identity SET gateway_uid = ? WHERE id = 1`, uid)
	if err != nil {
		return fmt.Errorf("set gateway uid: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return errors.New("set gateway uid: no identity has been generated yet")
	}
	return nil
}
