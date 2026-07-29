import crypto from "node:crypto";

import type { FastifyReply, FastifyRequest } from "fastify";
import { eq } from "drizzle-orm";

import { db, gateways } from "../../db/index.ts";

// Ed25519 request signing for the gateway agent API.
//
// There are no enrollment tokens and no shared secrets. A gateway generates an
// Ed25519 keypair on first boot, registers its *public* key, and signs every
// request thereafter. Registration is open — anything that can reach us may ask
// — so the key alone grants nothing: the row lands `pending` and an operator
// approves the fingerprint in the web UI.
//
// Canonical string (exactly what the Go client builds):
//
//     <METHOD>\n<url>\n<unix-seconds>\n<sha256-hex of the raw body>
//
// `url` is the request target including any query string. A GET has no body, so
// its digest is the sha256 of the empty string.

export const SIGNATURE_SKEW_SECONDS = 300;

// An Ed25519 SPKI DER is a fixed 12-byte header followed by the raw 32-byte
// key. Node's KeyObject API has no "raw ed25519" import, so we wrap it — this
// keeps the wire format the compact 32 bytes rather than a PEM blob.
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

export function publicKeyFromBase64(b64: string): crypto.KeyObject {
    const raw = Buffer.from(b64, "base64");
    if (raw.length !== 32) {
        throw new Error("ed25519 public key must be 32 bytes");
    }
    return crypto.createPublicKey({
        key: Buffer.concat([ED25519_SPKI_PREFIX, raw]),
        format: "der",
        type: "spki",
    });
}

// SHA-256 of the raw public key, hex. This is the string an operator eyeballs:
// the gateway prints it on its own status page and the console shows it on the
// approval screen, so approving is comparing two hex strings.
export function fingerprintFromBase64(b64: string): string {
    const raw = Buffer.from(b64, "base64");
    return crypto.createHash("sha256").update(raw).digest("hex");
}

export function canonicalString(
    method: string,
    url: string,
    timestamp: string,
    rawBody: string,
): string {
    const digest = crypto.createHash("sha256").update(rawBody).digest("hex");
    return `${method.toUpperCase()}\n${url}\n${timestamp}\n${digest}`;
}

export function verifySignature(
    publicKeyB64: string,
    signatureB64: string,
    canonical: string,
): boolean {
    try {
        return crypto.verify(
            null, // Ed25519 takes no separate digest algorithm
            Buffer.from(canonical, "utf8"),
            publicKeyFromBase64(publicKeyB64),
            Buffer.from(signatureB64, "base64"),
        );
    } catch {
        // A malformed key or signature is a failed verification, not a 500.
        return false;
    }
}

// The raw request body, captured by the scope-local content-type parser in
// src/routes/agent.ts. Signing the parsed-and-reserialised body instead would
// break on any key-order or whitespace difference.
export function rawBodyOf(request: FastifyRequest): string {
    return (request as FastifyRequest & { rawBody?: string }).rawBody ?? "";
}

export interface TimestampCheck {
    ok: boolean;
    reason?: string;
}

export function checkTimestamp(
    header: string | undefined,
    nowSeconds = Math.floor(Date.now() / 1000),
): TimestampCheck {
    if (!header) {
        return { ok: false, reason: "missing X-GW-Timestamp" };
    }
    const ts = Number(header);
    if (!Number.isFinite(ts)) {
        return { ok: false, reason: "malformed X-GW-Timestamp" };
    }
    if (Math.abs(nowSeconds - ts) > SIGNATURE_SKEW_SECONDS) {
        return { ok: false, reason: "timestamp outside the accepted window" };
    }
    return { ok: true };
}

export type GatewayRow = typeof gateways.$inferSelect;

declare module "fastify" {
    interface FastifyRequest {
        // Set by `authenticateGateway` for every signed agent route.
        gateway?: GatewayRow;
    }
}

// `preHandler` for the signed agent routes: resolve the gateway named by
// X-GW-Id, verify the signature against the public key we stored at
// registration, and attach the row.
//
// Deliberately does NOT check `status` — /agent/status must answer a pending
// gateway (that is how it learns it is still waiting). Only /agent/config gates
// on approval.
//
// Known limitation: a captured request can be replayed inside the skew window.
// The signed routes are an idempotent read (status, config) and a metrics
// report, so the worst case is a stale report being reapplied. A nonce table
// would close it if the reports ever drive anything but display.
export async function authenticateGateway(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    const deny = (reason: string) => {
        request.log.warn({ reason, url: request.url }, "agent auth rejected");
        return reply.code(401).send({ status: "error", message: reason });
    };

    const uid = request.headers["x-gw-id"];
    const signature = request.headers["x-gw-signature"];
    const timestamp = request.headers["x-gw-timestamp"];

    if (typeof uid !== "string" || !uid) {
        return deny("missing X-GW-Id");
    }
    if (typeof signature !== "string" || !signature) {
        return deny("missing X-GW-Signature");
    }

    const clock = checkTimestamp(
        typeof timestamp === "string" ? timestamp : undefined,
    );
    if (!clock.ok) {
        return deny(clock.reason ?? "bad timestamp");
    }

    const [gateway] = await db
        .select()
        .from(gateways)
        .where(eq(gateways.gateway_uid, uid))
        .limit(1);

    if (!gateway) {
        // Same answer as a bad signature: an unauthenticated caller must not be
        // able to enumerate which gateway ids exist.
        return deny("unknown gateway");
    }

    const canonical = canonicalString(
        request.method,
        request.url,
        String(timestamp),
        rawBodyOf(request),
    );
    if (!verifySignature(gateway.pubkey, signature, canonical)) {
        return deny("signature verification failed");
    }

    request.gateway = gateway;
}
