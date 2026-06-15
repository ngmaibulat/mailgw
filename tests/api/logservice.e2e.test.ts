/**
 * End-to-end tests for the logservice HTTP API.
 *
 * These talk to a *running* logservice (and its MariaDB), exactly like the SMTP
 * e2e suite talks to a running mailgw. They are opt-in and skipped by default;
 * enable them against the dev stack with:
 *
 *   MAILGW_API_E2E=1 bun test api
 *
 * Bring the stack up first:  docker compose up -d
 *
 * Connection / DB config is read from the repo-root `.env` (Bun auto-loads it
 * when the suite is run from the repo root, e.g. `bun test tests/api`). Override
 * any of these via the environment:
 *
 *   PORT            logservice port           (default 3000)
 *   LOGSERVICE_URL  full base URL             (default http://127.0.0.1:$PORT)
 *   API_KEY         sent as X-API-Key header  (default unset → no auth)
 */
import { describe, test, expect } from "bun:test";

const ENABLED = process.env.MAILGW_API_E2E === "1";

const PORT = Number(process.env.PORT ?? "3000");
const BASE = process.env.LOGSERVICE_URL ?? `http://127.0.0.1:${PORT}`;
const API_KEY = process.env.API_KEY;

function headers(): Record<string, string> {
    const h: Record<string, string> = { "Content-Type": "application/json" };
    if (API_KEY) h["X-API-Key"] = API_KEY;
    return h;
}

function post(path: string, body: unknown): Promise<Response> {
    return fetch(`${BASE}${path}`, { method: "POST", headers: headers(), body: JSON.stringify(body) });
}

function get(path: string): Promise<Response> {
    return fetch(`${BASE}${path}`, { headers: headers() });
}

// The search endpoints take a JSON `q` describing field/operator/value filters.
function searchQuery(field: string, value: string): string {
    return encodeURIComponent(JSON.stringify({ search: [{ field, operator: "is", value }] }));
}

// POST awaits the DB insert before replying, so the row exists by the time we
// search; a couple of retries just absorb any incidental lag.
async function findRecord(path: string, field: string, value: string): Promise<any | null> {
    for (let i = 0; i < 5; i++) {
        const res = await get(`${path}?q=${searchQuery(field, value)}`);
        const data = await res.json();
        if (Array.isArray(data.records) && data.records.length > 0) return data.records[0];
        await Bun.sleep(300);
    }
    return null;
}

const uuid = () => `e2e-${crypto.randomUUID()}`;

describe.skipIf(!ENABLED)("logservice API e2e", () => {
    describe("health", () => {
        test("GET / returns OK", async () => {
            const res = await get("/");
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ status: "OK" });
        });

        test("unknown path returns 404", async () => {
            const res = await get("/does-not-exist");
            expect(res.status).toBe(404);
        });
    });

    describe("connection events", () => {
        test("POST persists and is searchable by uuid", async () => {
            const id = uuid();
            const res = await post("/api/connection", {
                uuid: id,
                dt: Date.now(),
                hello_name: "e2e.test",
                remoteAddr: "203.0.113.10",
                remotePort: 54321,
                using_tls: false,
            });
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ status: "OK" });

            const row = await findRecord("/api/connection", "uuid", id);
            expect(row).not.toBeNull();
            expect(row.uuid).toBe(id);
            expect(row.remoteAddr).toBe("203.0.113.10");
        });
    });

    describe("queue events (stored as transactions)", () => {
        test("POST /api/queue persists a searchable transaction", async () => {
            const id = uuid();
            const res = await post("/api/queue", {
                uuid: id,
                dt: Date.now(),
                action: "queue",
                sender: "me@ngm.dev",
                rcpt_list: "test@ngm.dev",
                rcpt_count_accept: 1,
            });
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ status: "OK" });

            const row = await findRecord("/api/transaction", "uuid", id);
            expect(row).not.toBeNull();
            expect(row.sender).toBe("me@ngm.dev");
        });
    });

    describe("delivery events", () => {
        const validDelivery = (id: string) => ({
            uuid: id,
            dt: Date.now(),
            sender: "me@ngm.dev",
            rcpt_domain: "ngm.dev",
            rcpt_list: "test@ngm.dev",
            rcpt_accepted: "test@ngm.dev",
            tls_forced: false,
            tls: false,
            auth: false,
            host: "relay.ngm.dev",
            ip: "127.0.0.1",
            port: "25",
            response: "250 message queued",
            delay: 0.123,
        });

        test("POST a valid delivery persists and is searchable", async () => {
            const id = uuid();
            const res = await post("/api/delivery", validDelivery(id));
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ status: "OK" });

            const row = await findRecord("/api/delivery", "uuid", id);
            expect(row).not.toBeNull();
            expect(row.rcpt_accepted).toBe("test@ngm.dev");
            expect(row.response).toMatch(/queued/i);
        });

        test("POST an invalid delivery is rejected with 400", async () => {
            // Missing required fields / wrong types → Zod safeParse fails.
            const res = await post("/api/delivery", { uuid: uuid(), sender: "not-an-email" });
            expect(res.status).toBe(400);
            expect(await res.json()).toEqual({ status: "Fail" });
        });
    });

    describe("attachment md5 filter", () => {
        test("empty list is allowed", async () => {
            const res = await post("/filter/md5", []);
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ action: "allow" });
        });

        test("an unknown md5 is allowed", async () => {
            const res = await post("/filter/md5", [
                { txn_uuid: uuid(), md5: crypto.randomUUID().replace(/-/g, ""), filename: "x.txt", size: 12 },
            ]);
            expect(res.status).toBe(200);
            expect(await res.json()).toEqual({ action: "allow" });
        });
    });

    // Only meaningful when the server was started with API_KEY set.
    describe.skipIf(!API_KEY)("auth", () => {
        test("requests without the API key are rejected with 401", async () => {
            const res = await fetch(`${BASE}/api/queue`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ uuid: uuid(), dt: Date.now(), sender: "me@ngm.dev", rcpt_list: "x@y.dev" }),
            });
            expect(res.status).toBe(401);
        });
    });
});
