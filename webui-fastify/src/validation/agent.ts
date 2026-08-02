import { z } from "zod/v4";

// Payloads posted by a mailgw-go gateway. Everything here is self-declared by
// a caller we have not authenticated yet (registration) or have authenticated
// but not necessarily approved (report), so it is display/diagnostic data only
// and must never gate a decision.

// base64 of a raw 32-byte Ed25519 public key -> 44 chars with one '=' of
// padding. Checked here so a junk value fails as a 400 rather than surfacing as
// a signature error.
const publicKeyB64 = z
    .string()
    .regex(/^[A-Za-z0-9+/]{43}=$/, "pubkey must be base64 of 32 raw bytes");

export const RegisterInfo = z.object({
    pubkey: publicKeyB64,
    hostname: z.string().max(255).optional(),
    os: z.string().max(64).optional(),
    arch: z.string().max(64).optional(),
    cpus: z.number().int().nonnegative().max(4096).optional(),
    mem_bytes: z.number().int().nonnegative().optional(),
    // Serialised into the `ip_addrs` TEXT column for display.
    ip_addrs: z.array(z.string().max(64)).max(64).optional(),
    version: z.string().max(64).optional(),
});

export type RegisterInfo = z.infer<typeof RegisterInfo>;

// Counters the gateway reports on each heartbeat, stored as the latest
// snapshot per gateway (GatewayMetrics, logservice migration 024).
//
// Bounded on every axis, because this is self-declared data from a caller we
// have authenticated but do not otherwise trust, and it is now WRITTEN rather
// than discarded. The gateway's own set is 28 keys as of M8 (the `counters`
// table in mailgw-go/internal/obs, pinned there by a golden test); the cap
// leaves generous room for it to keep growing without letting a misbehaving or
// hostile node push an unbounded blob into the row.
//
// Values are finite non-negative integers because every one of them is a
// monotonic counter. Rejecting NaN and Infinity here matters: they are not
// representable in JSON, so they would surface as a serialisation failure
// somewhere further downstream instead of a 400 here.
export const AgentMetrics = z
    .record(z.string().min(1).max(64), z.number().int().nonnegative().finite())
    .refine((m) => Object.keys(m).length <= 128, {
        message: "at most 128 metrics keys",
    })
    .optional();

export const ReportInfo = z.object({
    // The ConfigVersions.id the gateway currently has applied. Null while it is
    // running its bundled defaults or has never successfully applied one.
    applied_version_id: z.number().int().positive().nullable().optional(),
    // Set when a pulled bundle failed to parse or type-check. The gateway keeps
    // its last-good configuration; this is how that failure reaches the console
    // instead of being silent.
    apply_error: z.string().max(4096).nullable().optional(),
    // A bundle changed something only a restart can pick up (relay table,
    // listeners, TLS, spool dir).
    restart_required: z.boolean().optional(),
    version: z.string().max(64).optional(),
    metrics: AgentMetrics,
});

export type ReportInfo = z.infer<typeof ReportInfo>;
