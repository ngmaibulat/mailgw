import assert from "node:assert/strict";
import crypto from "node:crypto";
import { after, before, beforeEach, describe, it } from "node:test";

process.env.SIGN_COOKIE = process.env.SIGN_COOKIE || "test-secret";

import type { FastifyInstance } from "fastify";

import { build } from "./app.ts";
import { db, gateways, configVersions } from "../db/index.ts";

// --- db stub ---------------------------------------------------------------
// Table-aware, unlike the single-result stub in app.test.ts: /agent/status
// selects from two tables in one request, so "whatever the last test set" would
// not distinguish them. `.where()` is a no-op — these tests assert routing and
// auth, not query construction.
const tableRows = new Map<unknown, unknown[]>();
const inserted: { table: unknown; values: unknown }[] = [];
const updated: { values: unknown }[] = [];

interface SelectStub extends PromiseLike<unknown[]> {
    from: (table: unknown) => SelectStub;
    where: () => SelectStub;
    limit: () => Promise<unknown[]>;
}

function makeSelectStub(): SelectStub {
    let table: unknown;
    const rows = () => tableRows.get(table) ?? [];
    const stub: SelectStub = {
        from: (t: unknown) => {
            table = t;
            return stub;
        },
        where: () => stub,
        limit: () => Promise.resolve(rows()),
        // biome-ignore lint/suspicious/noThenProperty: drizzle query-builder stub
        then: (onF, onR) => Promise.resolve(rows()).then(onF, onR),
    };
    return stub;
}

(db as unknown as { select: () => SelectStub }).select = () => makeSelectStub();

(
    db as unknown as {
        insert: (table: unknown) => { values: (v: unknown) => Promise<void> };
    }
).insert = (table: unknown) => ({
    values: (values: unknown) => {
        inserted.push({ table, values });
        return Promise.resolve();
    },
});

(
    db as unknown as {
        update: () => {
            set: (v: unknown) => { where: () => Promise<void> };
        };
    }
).update = () => ({
    set: (values: unknown) => ({
        where: () => {
            updated.push({ values });
            return Promise.resolve();
        },
    }),
});

(db as unknown as { delete: () => { where: () => Promise<void> } }).delete =
    () => ({ where: () => Promise.resolve() });

(db as unknown as { $count: () => Promise<number> }).$count = () =>
    Promise.resolve(1);

// --- gateway identity ------------------------------------------------------
// Exactly what mailgw-go will do: an Ed25519 keypair, with the raw 32-byte
// public key on the wire (the SPKI DER header is a fixed 12 bytes).
const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
const rawPublicKey = publicKey
    .export({ format: "der", type: "spki" })
    .subarray(12);
const PUBKEY_B64 = rawPublicKey.toString("base64");
const FINGERPRINT = crypto
    .createHash("sha256")
    .update(rawPublicKey)
    .digest("hex");
const GATEWAY_UID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

function sign(
    method: string,
    url: string,
    timestamp: number,
    body: string,
    key = privateKey,
): string {
    const digest = crypto.createHash("sha256").update(body).digest("hex");
    const canonical = `${method.toUpperCase()}\n${url}\n${timestamp}\n${digest}`;
    return crypto
        .sign(null, Buffer.from(canonical, "utf8"), key)
        .toString("base64");
}

function signedHeaders(
    method: string,
    url: string,
    body = "",
    opts: { uid?: string; timestamp?: number; signature?: string } = {},
) {
    const timestamp = opts.timestamp ?? Math.floor(Date.now() / 1000);
    return {
        "content-type": "application/json",
        "x-gw-id": opts.uid ?? GATEWAY_UID,
        "x-gw-timestamp": String(timestamp),
        "x-gw-signature": opts.signature ?? sign(method, url, timestamp, body),
    };
}

function gatewayRow(overrides: Record<string, unknown> = {}) {
    return {
        id: 1,
        gateway_uid: GATEWAY_UID,
        name: "edge-01",
        fingerprint: FINGERPRINT,
        pubkey: PUBKEY_B64,
        status: "approved",
        hostname: "edge-01",
        os: "linux",
        arch: "amd64",
        cpus: 4,
        mem_bytes: 8_000_000_000,
        ip_addrs: "10.0.0.5",
        version: "0.1.0",
        first_seen: new Date(),
        last_seen: new Date(),
        approved_by: "op@ngm.dev",
        approved_at: new Date(),
        desired_version_id: null,
        applied_version_id: null,
        apply_error: null,
        restart_required: false,
        createdAt: new Date(),
        updatedAt: new Date(),
        ...overrides,
    };
}

let app: FastifyInstance;

before(async () => {
    app = await build({});
    await app.ready();
});

after(async () => {
    await app.close();
});

beforeEach(() => {
    tableRows.clear();
    inserted.length = 0;
    updated.length = 0;
});

describe("POST /agent/register", () => {
    it("creates a pending gateway and never auto-approves", async () => {
        tableRows.set(gateways, []); // no existing row for this fingerprint

        const body = JSON.stringify({
            pubkey: PUBKEY_B64,
            hostname: "edge-01",
            os: "linux",
            arch: "amd64",
            cpus: 4,
            mem_bytes: 8_000_000_000,
            ip_addrs: ["10.0.0.5"],
            version: "0.1.0",
        });

        const res = await app.inject({
            method: "POST",
            url: "/agent/register",
            headers: signedHeaders("POST", "/agent/register", body),
            payload: body,
        });

        assert.equal(res.statusCode, 201);
        const json = JSON.parse(res.body);
        assert.equal(json.approval, "pending");
        assert.equal(json.fingerprint, FINGERPRINT);
        assert.ok(json.gateway_uid);

        assert.equal(inserted.length, 1);
        const values = inserted[0].values as Record<string, unknown>;
        assert.equal(values.status, "pending");
        assert.equal(values.fingerprint, FINGERPRINT);
    });

    it("rejects a body signed by a different key", async () => {
        tableRows.set(gateways, []);
        const other = crypto.generateKeyPairSync("ed25519").privateKey;
        const body = JSON.stringify({ pubkey: PUBKEY_B64 });
        const timestamp = Math.floor(Date.now() / 1000);

        const res = await app.inject({
            method: "POST",
            url: "/agent/register",
            headers: signedHeaders("POST", "/agent/register", body, {
                timestamp,
                signature: sign(
                    "POST",
                    "/agent/register",
                    timestamp,
                    body,
                    other,
                ),
            }),
            payload: body,
        });

        assert.equal(res.statusCode, 401);
        assert.equal(inserted.length, 0);
    });

    it("is idempotent for a known fingerprint and cannot reset approval", async () => {
        tableRows.set(gateways, [
            gatewayRow({ status: "revoked", id: 7, name: "old" }),
        ]);
        const body = JSON.stringify({
            pubkey: PUBKEY_B64,
            hostname: "edge-01",
        });

        const res = await app.inject({
            method: "POST",
            url: "/agent/register",
            headers: signedHeaders("POST", "/agent/register", body),
            payload: body,
        });

        assert.equal(res.statusCode, 200);
        assert.equal(JSON.parse(res.body).approval, "revoked");
        assert.equal(inserted.length, 0, "must not create a second row");
        // Only systeminfo + last_seen are refreshed; status is untouched.
        const values = updated[0].values as Record<string, unknown>;
        assert.equal(values.status, undefined);
    });

    it("rejects a malformed pubkey with 400", async () => {
        const body = JSON.stringify({ pubkey: "not-base64" });
        const res = await app.inject({
            method: "POST",
            url: "/agent/register",
            headers: signedHeaders("POST", "/agent/register", body),
            payload: body,
        });
        assert.equal(res.statusCode, 400);
    });
});

describe("agent authentication", () => {
    it("rejects an unsigned request", async () => {
        tableRows.set(gateways, [gatewayRow()]);
        const res = await app.inject({ method: "GET", url: "/agent/status" });
        assert.equal(res.statusCode, 401);
    });

    it("rejects a bad signature", async () => {
        tableRows.set(gateways, [gatewayRow()]);
        const res = await app.inject({
            method: "GET",
            url: "/agent/status",
            headers: signedHeaders("GET", "/agent/status", "", {
                signature: Buffer.alloc(64).toString("base64"),
            }),
        });
        assert.equal(res.statusCode, 401);
    });

    it("rejects a stale timestamp even when correctly signed", async () => {
        tableRows.set(gateways, [gatewayRow()]);
        const stale = Math.floor(Date.now() / 1000) - 3600;
        const res = await app.inject({
            method: "GET",
            url: "/agent/status",
            headers: signedHeaders("GET", "/agent/status", "", {
                timestamp: stale,
            }),
        });
        assert.equal(res.statusCode, 401);
    });

    it("rejects an unknown gateway id", async () => {
        tableRows.set(gateways, []); // lookup finds nothing
        const res = await app.inject({
            method: "GET",
            url: "/agent/status",
            headers: signedHeaders("GET", "/agent/status"),
        });
        assert.equal(res.statusCode, 401);
    });
});

describe("GET /agent/status", () => {
    it("answers a pending gateway, so it can learn it is waiting", async () => {
        tableRows.set(gateways, [gatewayRow({ status: "pending" })]);
        const res = await app.inject({
            method: "GET",
            url: "/agent/status",
            headers: signedHeaders("GET", "/agent/status"),
        });

        assert.equal(res.statusCode, 200);
        const json = JSON.parse(res.body);
        assert.equal(json.approval, "pending");
        assert.equal(json.desired_version, null);
    });

    it("reports the desired version and bundle digest", async () => {
        tableRows.set(gateways, [gatewayRow({ desired_version_id: 42 })]);
        tableRows.set(configVersions, [{ version: 7, sha256: "a".repeat(64) }]);

        const res = await app.inject({
            method: "GET",
            url: "/agent/status",
            headers: signedHeaders("GET", "/agent/status"),
        });

        const json = JSON.parse(res.body);
        assert.equal(json.desired_version, 7);
        assert.equal(json.bundle_sha256, "a".repeat(64));
    });
});

describe("GET /agent/config", () => {
    it("refuses a pending gateway with 403", async () => {
        tableRows.set(gateways, [
            gatewayRow({ status: "pending", desired_version_id: 42 }),
        ]);
        const res = await app.inject({
            method: "GET",
            url: "/agent/config",
            headers: signedHeaders("GET", "/agent/config"),
        });

        assert.equal(res.statusCode, 403);
        assert.equal(JSON.parse(res.body).approval, "pending");
    });

    it("refuses a revoked gateway with 403", async () => {
        tableRows.set(gateways, [
            gatewayRow({ status: "revoked", desired_version_id: 42 }),
        ]);
        const res = await app.inject({
            method: "GET",
            url: "/agent/config",
            headers: signedHeaders("GET", "/agent/config"),
        });
        assert.equal(res.statusCode, 403);
    });

    it("404s an approved gateway with nothing deployed", async () => {
        tableRows.set(gateways, [gatewayRow({ desired_version_id: null })]);
        const res = await app.inject({
            method: "GET",
            url: "/agent/config",
            headers: signedHeaders("GET", "/agent/config"),
        });
        assert.equal(res.statusCode, 404);
    });

    it("serves the deployed bundle to an approved gateway", async () => {
        const bundle = { version: 7, relays: { Outbound: [] } };
        tableRows.set(gateways, [gatewayRow({ desired_version_id: 42 })]);
        tableRows.set(configVersions, [
            {
                id: 42,
                version: 7,
                bundle: JSON.stringify(bundle),
                bundle_sha256: "b".repeat(64),
            },
        ]);

        const res = await app.inject({
            method: "GET",
            url: "/agent/config",
            headers: signedHeaders("GET", "/agent/config"),
        });

        assert.equal(res.statusCode, 200);
        const json = JSON.parse(res.body);
        assert.equal(json.version, 7);
        assert.deepEqual(json.bundle, bundle);
    });
});

describe("POST /agent/report", () => {
    it("records the applied version and an apply error", async () => {
        tableRows.set(gateways, [gatewayRow()]);
        const body = JSON.stringify({
            applied_version_id: 41,
            apply_error: "rule 'block exe': unknown field attachment.nope",
            restart_required: true,
            version: "0.2.0",
        });

        const res = await app.inject({
            method: "POST",
            url: "/agent/report",
            headers: signedHeaders("POST", "/agent/report", body),
            payload: body,
        });

        assert.equal(res.statusCode, 200);
        const values = updated.at(-1)?.values as Record<string, unknown>;
        assert.equal(values.applied_version_id, 41);
        assert.equal(values.restart_required, true);
        assert.match(String(values.apply_error), /unknown field/);
    });

    it("rejects a malformed report with 400", async () => {
        tableRows.set(gateways, [gatewayRow()]);
        const body = JSON.stringify({ applied_version_id: "not-a-number" });
        const res = await app.inject({
            method: "POST",
            url: "/agent/report",
            headers: signedHeaders("POST", "/agent/report", body),
            payload: body,
        });
        assert.equal(res.statusCode, 400);
    });
});
