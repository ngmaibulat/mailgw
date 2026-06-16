import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

// build() reads SIGN_COOKIE at call time and throws without it. Tests never go
// through checkenv, so set it here.
process.env.SIGN_COOKIE = process.env.SIGN_COOKIE || "test-secret";

import type { FastifyInstance } from "fastify";

import { build } from "./app.ts";
import { db, users } from "../db/index.ts";
import { sessions } from "./globals.ts";
import { bcrypt } from "./adapter.ts";

// Stub db.insert: the audit-log onRequest hook does a fire-and-forget insert on
// every non-noise request, and the setup route inserts a user. Recording the
// table lets setup tests assert whether a user was actually created without
// opening a real DB connection.
const userInserts: unknown[] = [];
(
    db as unknown as {
        insert: (table: unknown) => { values: () => Promise<void> };
    }
).insert = (table: unknown) => ({
    values: () => {
        if (table === users) {
            userInserts.push(table);
        }
        return Promise.resolve();
    },
});

// Stub db.$count (used by countUsers) so the first-run setup gate is testable.
// Default to "users exist"; setup tests flip it to 0.
let userCount = 1;
(db as unknown as { $count: () => Promise<number> }).$count = () =>
    Promise.resolve(userCount);

// Stub db.select (used by checkAuth, listUsers, getUser). A chainable thenable
// so it resolves to `selectResult` whether the query awaits `.from()` directly
// (listUsers) or chains `.where().limit()` (checkAuth/getUser). Tests set
// `selectResult` to the row(s) the query should "return".
let selectResult: unknown[] = [];
interface SelectStub extends PromiseLike<unknown[]> {
    from: () => SelectStub;
    where: () => SelectStub;
    limit: () => Promise<unknown[]>;
}
const selectStub: SelectStub = {
    from: () => selectStub,
    where: () => selectStub,
    limit: () => Promise.resolve(selectResult),
    // Intentionally thenable — mirrors drizzle's awaitable query builder so
    // `await db.select().from(...)` resolves without a terminal `.limit()`.
    // biome-ignore lint/suspicious/noThenProperty: deliberate query-builder stub
    then: (onF, onR) => Promise.resolve(selectResult).then(onF, onR),
};
(db as unknown as { select: () => SelectStub }).select = () => selectStub;

// Stub db.update (used by updatePassword + updateUser). Records the set() values
// so tests can assert what (if anything) was written.
const dbUpdates: unknown[] = [];
(
    db as unknown as {
        update: () => {
            set: (vals: unknown) => { where: () => Promise<void> };
        };
    }
).update = () => ({
    set: (vals: unknown) => ({
        where: () => {
            dbUpdates.push(vals);
            return Promise.resolve();
        },
    }),
});

// Stub db.delete (used by deleteUser). Records that a delete happened.
const dbDeletes: unknown[] = [];
(db as unknown as { delete: () => { where: () => Promise<void> } }).delete =
    () => ({
        where: () => {
            dbDeletes.push(1);
            return Promise.resolve();
        },
    });

// Controllable fetch stub for the logservice proxy. `fetchImpl` is swapped per
// test; `calls` records the URLs the proxy fetched so we can assert path remaps.
const realFetch = globalThis.fetch;
let fetchImpl: (
    input: string | URL | Request,
    init?: RequestInit,
) => Promise<Response>;
const calls: URL[] = [];
globalThis.fetch = ((input: string | URL | Request, init?: RequestInit) => {
    calls.push(input instanceof URL ? input : new URL(String(input)));
    return fetchImpl(input, init);
}) as typeof fetch;

// A valid search payload (mirrors logservice's accepted shape).
const VALID_SEARCH = JSON.stringify({
    search: [{ field: "ip", operator: "contains", value: "1.2.3" }],
    limit: 50,
});

function okResponse(): Response {
    return new Response(
        JSON.stringify({ status: "success", total: 0, records: [] }),
        { status: 200, headers: { "content-type": "application/json" } },
    );
}

let app: FastifyInstance;

// A session cookie signed with the same secret the app uses, so checkSession
// accepts it. The session is registered in the in-memory store first.
function authCookie(): string {
    const sid = "test-session";
    sessions[sid] = { email: "admin@test", expiresAt: Date.now() + 60_000 };
    return `session=${app.signCookie(sid)}`;
}

before(async () => {
    app = await build({});
    // A root-scope route that throws, to exercise setErrorHandler.
    app.get("/__boom", async () => {
        throw new Error("kaboom");
    });
    await app.ready();
});

after(async () => {
    await app.close();
    globalThis.fetch = realFetch;
});

describe("health + auth gate", () => {
    it("serves GET /health without authentication", async () => {
        const res = await app.inject({ method: "GET", url: "/health" });
        assert.equal(res.statusCode, 200);
        assert.deepEqual(JSON.parse(res.body), { status: "ok" });
    });

    it("redirects an unauthenticated browser request to /login", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/",
            headers: { accept: "text/html" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
    });

    it("returns 401 for an unauthenticated JSON API request", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/api/connection",
            headers: { accept: "application/json" },
        });
        assert.equal(res.statusCode, 401);
    });

    it("sets helmet security headers, with CSP in report-only mode", async () => {
        const res = await app.inject({ method: "GET", url: "/health" });
        assert.equal(res.headers["x-content-type-options"], "nosniff");
        assert.ok(res.headers["x-frame-options"]);
        // CSP is shipped report-only — the enforcing header must be absent.
        assert.ok(res.headers["content-security-policy-report-only"]);
        assert.equal(res.headers["content-security-policy"], undefined);
    });
});

describe("logservice read proxy", () => {
    it("remaps /api/queue -> logservice /api/transaction and forwards q", async () => {
        fetchImpl = async () => okResponse();
        calls.length = 0;

        const res = await app.inject({
            method: "GET",
            url: `/api/queue?request=${encodeURIComponent(VALID_SEARCH)}`,
            headers: { cookie: authCookie(), accept: "application/json" },
        });

        assert.equal(res.statusCode, 200);
        assert.equal(calls.length, 1);
        assert.equal(calls[0].pathname, "/api/transaction");
        assert.deepEqual(JSON.parse(calls[0].searchParams.get("q") ?? ""), {
            search: [{ field: "ip", operator: "contains", value: "1.2.3" }],
            limit: 50,
        });
    });

    it("remaps /api/hashlookups -> logservice /api/hashlookup", async () => {
        fetchImpl = async () => okResponse();
        calls.length = 0;

        const res = await app.inject({
            method: "GET",
            url: "/api/hashlookups",
            headers: { cookie: authCookie(), accept: "application/json" },
        });

        assert.equal(res.statusCode, 200);
        assert.equal(calls[0].pathname, "/api/hashlookup");
    });

    it("returns 400 for a malformed search request without calling logservice", async () => {
        calls.length = 0;
        const res = await app.inject({
            method: "GET",
            url: `/api/connection?request=${encodeURIComponent("{not json")}`,
            headers: { cookie: authCookie(), accept: "application/json" },
        });

        assert.equal(res.statusCode, 400);
        assert.equal(calls.length, 0);
    });

    it("maps a logservice non-2xx response to 502", async () => {
        fetchImpl = async () => new Response("upstream boom", { status: 500 });
        const res = await app.inject({
            method: "GET",
            url: "/api/connection",
            headers: { cookie: authCookie(), accept: "application/json" },
        });
        assert.equal(res.statusCode, 502);
    });

    it("maps a logservice network failure to 504", async () => {
        fetchImpl = async () => {
            throw new Error("ECONNREFUSED");
        };
        const res = await app.inject({
            method: "GET",
            url: "/api/connection",
            headers: { cookie: authCookie(), accept: "application/json" },
        });
        assert.equal(res.statusCode, 504);
    });
});

describe("first-run setup", () => {
    it("redirects /login to /setup when no users exist", async () => {
        userCount = 0;
        const res = await app.inject({ method: "GET", url: "/login" });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/setup");
    });

    it("serves the setup form when no users exist", async () => {
        userCount = 0;
        const res = await app.inject({ method: "GET", url: "/setup" });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /Create the first admin user/);
    });

    it("redirects /setup to /login once a user exists", async () => {
        userCount = 1;
        const res = await app.inject({ method: "GET", url: "/setup" });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
    });

    it("refuses POST /setup once a user exists (creates nothing)", async () => {
        userCount = 1;
        userInserts.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/setup",
            payload: {
                email: "a@b.com",
                pass: "password1",
                passConfirm: "password1",
            },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
        assert.equal(userInserts.length, 0);
    });

    it("creates the first user on a valid POST /setup", async () => {
        userCount = 0;
        userInserts.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/setup",
            payload: {
                email: "admin@b.com",
                pass: "password1",
                passConfirm: "password1",
            },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login?msg=created");
        assert.equal(userInserts.length, 1);
    });

    it("re-renders setup with an error when passwords mismatch", async () => {
        userCount = 0;
        userInserts.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/setup",
            payload: {
                email: "admin@b.com",
                pass: "password1",
                passConfirm: "password2",
            },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /do not match/i);
        assert.equal(userInserts.length, 0);
    });
});

describe("profile", () => {
    it("redirects an unauthenticated profile request to /login", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/profile",
            headers: { accept: "text/html" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
    });

    it("shows the logged-in user's email", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/profile",
            headers: { cookie: authCookie() },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /admin@test/);
    });

    it("rejects a wrong current password (no write)", async () => {
        selectResult = [
            { email: "admin@test", hash: await bcrypt.hash("rightpass1", 10) },
        ];
        dbUpdates.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/profile",
            headers: { cookie: authCookie() },
            payload: {
                current: "wrongpass1",
                pass: "newpass12",
                passConfirm: "newpass12",
            },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /Current password is incorrect/);
        assert.equal(dbUpdates.length, 0);
    });

    it("updates the password when the current password is correct", async () => {
        selectResult = [
            { email: "admin@test", hash: await bcrypt.hash("rightpass1", 10) },
        ];
        dbUpdates.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/profile",
            headers: { cookie: authCookie() },
            payload: {
                current: "rightpass1",
                pass: "newpass12",
                passConfirm: "newpass12",
            },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /Password updated/);
        assert.equal(dbUpdates.length, 1);
    });

    it("re-renders with an error when the new passwords mismatch (no write)", async () => {
        dbUpdates.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/profile",
            headers: { cookie: authCookie() },
            payload: {
                current: "rightpass1",
                pass: "newpass12",
                passConfirm: "different1",
            },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /do not match/i);
        assert.equal(dbUpdates.length, 0);
    });
});

describe("user management", () => {
    it("redirects an unauthenticated request to /login", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/users",
            headers: { accept: "text/html" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/login");
    });

    it("lists users with a Create button and edit/delete actions", async () => {
        selectResult = [
            { id: 1, email: "admin@test", createdAt: "2026-06-16" },
            { id: 2, email: "bob@test", createdAt: "2026-06-16" },
        ];
        const res = await app.inject({
            method: "GET",
            url: "/users",
            headers: { cookie: authCookie() },
        });
        assert.equal(res.statusCode, 200);
        assert.match(res.body, /admin@test/);
        assert.match(res.body, /bob@test/);
        assert.match(res.body, /\/users\/create/);
        assert.match(res.body, /\/users\/edit\/2/);
        assert.match(res.body, /\/users\/delete\/2/);
    });

    it("creates a user on a valid POST", async () => {
        userInserts.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/users/create",
            headers: { cookie: authCookie() },
            payload: { email: "new@test.com", pass: "password1" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/users");
        assert.equal(userInserts.length, 1);
    });

    it("rejects an invalid create (no write)", async () => {
        userInserts.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/users/create",
            headers: { cookie: authCookie() },
            payload: { email: "bad", pass: "short" },
        });
        assert.equal(res.statusCode, 400);
        assert.match(res.body, /Create User/);
        assert.equal(userInserts.length, 0);
    });

    it("updates a user, keeping the password when left blank", async () => {
        dbUpdates.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/users/edit/2",
            headers: { cookie: authCookie() },
            payload: { email: "bob2@test.com", pass: "" },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/users");
        assert.equal(dbUpdates.length, 1);
        // Blank password => the update set carries the new email but no hash.
        const set = dbUpdates[0] as { email?: string; hash?: string };
        assert.equal(set.email, "bob2@test.com");
        assert.equal(set.hash, undefined);
    });

    it("deletes a user when more than one exists", async () => {
        userCount = 2;
        selectResult = [{ id: 2, email: "bob@test" }];
        dbDeletes.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/users/delete/2",
            headers: { cookie: authCookie() },
        });
        assert.equal(res.statusCode, 302);
        assert.equal(res.headers.location, "/users");
        assert.equal(dbDeletes.length, 1);
    });

    it("refuses to delete the last remaining user (no write)", async () => {
        userCount = 1;
        selectResult = [{ id: 1, email: "admin@test" }];
        dbDeletes.length = 0;
        const res = await app.inject({
            method: "POST",
            url: "/users/delete/1",
            headers: { cookie: authCookie() },
        });
        assert.equal(res.statusCode, 400);
        assert.match(res.body, /last remaining user/);
        assert.equal(dbDeletes.length, 0);
    });
});

describe("login rate limiting", () => {
    // Default budget is 5 requests / minute per IP (LOGIN_RATE_MAX unset).
    // No other test posts to /login, so this route's counter starts fresh.
    // Empty payloads fail validation (302 -> ?msg=ValidationError) without
    // touching the DB, while still consuming the rate-limit budget.
    it("redirects to /login?msg=TooManyAttempts after exceeding the limit", async () => {
        const post = () =>
            app.inject({
                method: "POST",
                url: "/login",
                headers: { accept: "text/html" },
                payload: {},
            });

        for (let i = 0; i < 5; i++) {
            const res = await post();
            assert.equal(res.statusCode, 302);
            assert.equal(res.headers.location, "/login?msg=ValidationError");
        }

        const limited = await post();
        assert.equal(limited.statusCode, 302);
        assert.equal(limited.headers.location, "/login?msg=TooManyAttempts");
    });
});

describe("error handler", () => {
    it("renders an HTML error page for unhandled exceptions", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/__boom",
            headers: { accept: "text/html" },
        });
        assert.equal(res.statusCode, 500);
        assert.match(res.headers["content-type"] ?? "", /text\/html/);
        assert.match(res.body, /Something went wrong/);
    });

    it("returns JSON for unhandled exceptions on JSON requests", async () => {
        const res = await app.inject({
            method: "GET",
            url: "/__boom",
            headers: { accept: "application/json" },
        });
        assert.equal(res.statusCode, 500);
        assert.equal(JSON.parse(res.body).status, "error");
    });
});
