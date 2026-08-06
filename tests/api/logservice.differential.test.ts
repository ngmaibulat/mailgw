/**
 * Differential test: logservice-go (net/http) vs logservice-fiber (Fiber v3).
 *
 * The same request goes to both live services and the two responses are
 * compared byte for byte — status, body text, and the full header set minus an
 * explicit normalisation allowlist.
 *
 * # Why this exists when a contract suite already does
 *
 * logservice-go/apitest runs the same assertions against both implementations
 * in Go, and it is the faster, cheaper gate. But it drives each implementation
 * through its own in-process test harness — httptest.ResponseRecorder on one
 * side, fiber's app.Test on the other — so it cannot see anything the harness
 * normalises away. This runs over a real socket against a real MariaDB, and so
 * catches what no Go test can:
 *
 *   - header casing, and headers one framework sends that the other does not;
 *   - Content-Length versus chunked transfer encoding;
 *   - keep-alive behaviour;
 *   - the exact JSON bytes of a real result set, including how a DATETIME,
 *     a NULL and a number are rendered — the thing internal/rows exists for;
 *   - ServeMux's clean-path redirect on a doubled slash.
 *
 * # Running it
 *
 *   docker compose up -d
 *   pnpm test:ab:logservice
 *
 * Opt-in via MAILGW_AB=1, like the rest of the DB-touching suites. Override the
 * two base URLs if the services are somewhere else:
 *
 *   LOGSERVICE_A_URL   net/http    (default http://127.0.0.1:3000)
 *   LOGSERVICE_B_URL   Fiber v3    (default http://127.0.0.1:3001)
 *   API_KEY            sent as X-API-Key to both when set
 */
import { describe, test, expect, beforeAll } from "bun:test";

const ENABLED = process.env.MAILGW_AB === "1";

const A = process.env.LOGSERVICE_A_URL ?? "http://127.0.0.1:3000";
const B = process.env.LOGSERVICE_B_URL ?? "http://127.0.0.1:3001";
const API_KEY = process.env.API_KEY;

/**
 * Headers whose difference is not a difference.
 *
 * Keep this list SHORT and justified. The milestone's completion gate is that
 * nothing else needs to be added: anything that does is a finding for
 * logservice-fiber's known-differences table, not a line to add quietly here.
 *
 *   date        wall clock; the two are called microseconds apart
 *   connection  keep-alive negotiation, per-connection and not per-response
 *   keep-alive  the timeout hint that rides with it
 */
const IGNORED_HEADERS = new Set(["date", "connection", "keep-alive"]);

/**
 * Framing headers, compared separately rather than ignored.
 *
 * The two frame large responses differently and it is not a bug in either:
 * net/http buffers 2048 bytes to sniff a Content-Length and switches to chunked
 * once a body outgrows that, while fasthttp buffers the whole response and can
 * therefore always declare a length. So a search returning five rows is
 * `Transfer-Encoding: chunked` on one and `Content-Length: 2254` on the other,
 * with identical bytes underneath.
 *
 * Both are valid HTTP/1.1 and every caller here uses a client that handles both
 * transparently — Go's net/http in mailgw-go, undici in the console, curl in
 * deploy/core/deploy.sh. Dropping these two into IGNORED_HEADERS would have hidden
 * it; instead the responses are still checked for VALID framing, and the
 * difference is recorded in logservice-fiber/README.md.
 */
const FRAMING_HEADERS = ["content-length", "transfer-encoding"];

type Captured = {
    status: number;
    body: string;
    headers: Record<string, string>;
    framing: Record<string, string>;
};

function requestHeaders(withBody: boolean): Record<string, string> {
    const h: Record<string, string> = {};
    if (withBody) h["Content-Type"] = "application/json";
    if (API_KEY) h["X-API-Key"] = API_KEY;
    return h;
}

async function capture(base: string, method: string, path: string, body?: string): Promise<Captured> {
    const res = await fetch(`${base}${path}`, {
        method,
        headers: requestHeaders(body !== undefined),
        body,
        // Never follow: a redirect IS a difference we want to see. ServeMux
        // issues one for a doubled slash and Fiber does not.
        redirect: "manual",
    });

    const headers: Record<string, string> = {};
    const framing: Record<string, string> = {};
    for (const [k, v] of res.headers) {
        const key = k.toLowerCase();
        if (IGNORED_HEADERS.has(key)) continue;
        if (FRAMING_HEADERS.includes(key)) framing[key] = v;
        else headers[key] = v;
    }

    return { status: res.status, body: await res.text(), headers, framing };
}

/**
 * A response must declare its length or chunk it, never both and never neither.
 *
 * This is what replaces comparing the framing headers to each other: the two
 * implementations are allowed to disagree about WHICH, but neither is allowed
 * to produce a response a client cannot delimit.
 */
function expectValidFraming(label: string, c: Captured) {
    // A 304 or a 204 carries no body and no framing; nothing here answers one.
    const hasLength = c.framing["content-length"] !== undefined;
    const isChunked = (c.framing["transfer-encoding"] ?? "").includes("chunked");

    expect(hasLength || isChunked, `${label} declared neither a length nor chunked framing`).toBe(true);
    expect(hasLength && isChunked, `${label} declared both a length and chunked framing`).toBe(false);

    if (hasLength) {
        expect(Number(c.framing["content-length"]), `${label} Content-Length disagrees with the body`)
            .toBe(Buffer.byteLength(c.body));
    }
}

type Case = {
    name: string;
    method: string;
    path: string;
    body?: string;
    /**
     * Set when the two are known to differ and the difference is recorded in
     * logservice-fiber/README.md. The case still runs, so the day it stops
     * differing is visible.
     */
    knownDifference?: string;
};

const q = (obj: unknown) => `?q=${encodeURIComponent(JSON.stringify(obj))}`;

const CASES: Case[] = [
    // Routing.
    { name: "GET /", method: "GET", path: "/" },
    { name: "an unknown path", method: "GET", path: "/does-not-exist" },
    { name: "a path under the root", method: "GET", path: "/nope" },
    { name: "a wrong method on a known path", method: "GET", path: "/filter/md5" },
    { name: "a trailing slash", method: "POST", path: "/api/queue/", body: '{"uuid":"ab-diff"}' },
    { name: "a mixed-case path", method: "GET", path: "/API/Connection" },
    {
        name: "a doubled slash",
        method: "GET",
        path: "/api//connection",
        knownDifference:
            "ServeMux issues a 307 to the cleaned path; Fiber's router does not clean and 404s",
    },
    {
        name: "an unrecognised method",
        method: "PROPFIND",
        path: "/api/queue",
        knownDifference:
            "Fiber answers 501 before routing; see logservice-fiber/README.md",
    },

    // Health.
    { name: "GET /healthz", method: "GET", path: "/healthz", knownDifference: "the version string is per-build" },
    { name: "GET /readyz", method: "GET", path: "/readyz" },

    // Ingest refusals. Nothing here writes a row, so the two can be compared
    // without either one changing what the other then reads.
    { name: "a malformed connection body", method: "POST", path: "/api/connection", body: "{not json" },
    { name: "a malformed queue body", method: "POST", path: "/api/queue", body: "{not json" },
    { name: "an invalid delivery", method: "POST", path: "/api/delivery", body: '{"uuid":"x","sender":"bad"}' },
    { name: "an empty delivery", method: "POST", path: "/api/delivery", body: "{}" },

    // /filter/md5 — every one of these must be a 200 allow on both, because a
    // non-2xx defers real mail.
    { name: "/filter/md5 with an empty array", method: "POST", path: "/filter/md5", body: "[]" },
    { name: "/filter/md5 with a non-array", method: "POST", path: "/filter/md5", body: "{}" },
    { name: "/filter/md5 with a bare string", method: "POST", path: "/filter/md5", body: '"x"' },
    { name: "/filter/md5 with null", method: "POST", path: "/filter/md5", body: "null" },
    { name: "/filter/md5 with broken JSON", method: "POST", path: "/filter/md5", body: "{not json" },
    {
        name: "/filter/md5 with a digest that is not blocked",
        method: "POST",
        path: "/filter/md5",
        body: '[{"md5":"00000000000000000000000000000000","filename":"ab.txt","size":1,"contentType":"text/plain","txn_uuid":"AB.1"}]',
    },

    // Searches over the real schema. These are the cases that exercise
    // internal/rows: the response carries real DATETIMEs, NULLs and numbers,
    // and a difference in how any of them is rendered shows up here as a body
    // mismatch.
    { name: "search connections", method: "GET", path: `/api/connection${q({ limit: 5 })}` },
    { name: "search transactions", method: "GET", path: `/api/transaction${q({ limit: 5 })}` },
    { name: "search deliveries", method: "GET", path: `/api/delivery${q({ limit: 5 })}` },
    { name: "search hashlookups (a JOIN)", method: "GET", path: `/api/hashlookup${q({ limit: 5 })}` },
    { name: "search with an offset", method: "GET", path: `/api/connection${q({ limit: 2, offset: 1 })}` },
    { name: "search with a sort", method: "GET", path: `/api/connection${q({ limit: 3, sort: [{ field: "id", direction: "asc" }] })}` },
    { name: "search with a filter", method: "GET", path: `/api/connection${q({ search: [{ field: "uuid", operator: "contains", value: "a" }], limit: 3 })}` },

    // The searches whose contract is that they are NEVER a 400.
    { name: "search with malformed q", method: "GET", path: "/api/connection?q=%7Bnot" },
    { name: "search with a q that is not an object", method: "GET", path: `/api/connection${q([])}` },
    { name: "search with no q at all", method: "GET", path: "/api/connection" },
    { name: "search with a limit past the clamp", method: "GET", path: `/api/connection${q({ limit: 1000000 })}` },
    { name: "search with an unknown field", method: "GET", path: `/api/connection${q({ search: [{ field: "not_a_column", operator: "is", value: "x" }], limit: 3 })}` },
];

describe.skipIf(!ENABLED)("logservice-go vs logservice-fiber", () => {
    beforeAll(async () => {
        for (const [name, base] of [["A (net/http)", A], ["B (Fiber)", B]] as const) {
            const res = await fetch(`${base}/healthz`).catch((e: unknown) => {
                throw new Error(`${name} at ${base} is unreachable: ${String(e)}`);
            });
            if (!res.ok) throw new Error(`${name} at ${base} answered ${res.status} on /healthz`);
        }
    });

    for (const c of CASES) {
        test(c.name, async () => {
            const [a, b] = await Promise.all([
                capture(A, c.method, c.path, c.body),
                capture(B, c.method, c.path, c.body),
            ]);

            if (c.knownDifference) {
                // Still run it, so the day the difference goes away is visible
                // and this entry can be deleted. Only report, never fail.
                if (a.status !== b.status || a.body !== b.body) {
                    console.log(
                        `  known difference — ${c.name}: ${c.knownDifference}\n` +
                            `    net/http: ${a.status} ${JSON.stringify(a.body.slice(0, 120))}\n` +
                            `    fiber:    ${b.status} ${JSON.stringify(b.body.slice(0, 120))}`,
                    );
                }
                return;
            }

            expect(b.status, `status for ${c.method} ${c.path}`).toBe(a.status);
            expect(b.body, `body for ${c.method} ${c.path}`).toBe(a.body);
            expect(b.headers, `headers for ${c.method} ${c.path}`).toEqual(a.headers);

            // Framing may differ; being undelimitable may not.
            expectValidFraming(`net/http ${c.method} ${c.path}`, a);
            expectValidFraming(`fiber ${c.method} ${c.path}`, b);
        });
    }

    // The API key gate, only when there is one to test.
    describe.skipIf(!API_KEY)("auth", () => {
        test("a request with no key is refused identically", async () => {
            const noKey = async (base: string) => {
                const res = await fetch(`${base}/api/queue`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: '{"uuid":"ab-diff"}',
                });
                return { status: res.status, body: await res.text() };
            };
            const [a, b] = await Promise.all([noKey(A), noKey(B)]);
            expect(b).toEqual(a);
        });

        test("a wrong key is refused identically", async () => {
            const wrongKey = async (base: string) => {
                const res = await fetch(`${base}/api/queue`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json", "X-API-Key": `${API_KEY}x` },
                    body: '{"uuid":"ab-diff"}',
                });
                return { status: res.status, body: await res.text() };
            };
            const [a, b] = await Promise.all([wrongKey(A), wrongKey(B)]);
            expect(b).toEqual(a);
        });
    });
});
