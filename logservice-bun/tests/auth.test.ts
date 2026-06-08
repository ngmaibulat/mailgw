import { describe, it, expect, beforeAll, afterAll } from "bun:test";

const BASE = "http://localhost:3001";
let server: ReturnType<typeof Bun.serve>;

beforeAll(() => {
    process.env.API_KEY = "test-key-123";

    server = Bun.serve({
        port: 3001,
        routes: {
            "/api/test": {
                GET: () => Response.json({ status: "OK" }),
            },
        },
        fetch: () => new Response("Not Found", { status: 404 }),
    });
});

afterAll(() => server.stop());

describe("withAuth middleware", () => {
    it("returns 401 when API_KEY is set and header is missing", async () => {
        const { withAuth } = await import("../src/middleware/auth");
        const handler = withAuth(() => Response.json({ status: "OK" }));
        const res = await handler(new Request(`${BASE}/api/test`));
        expect(res.status).toBe(401);
    });

    it("returns 401 when wrong key is provided", async () => {
        const { withAuth } = await import("../src/middleware/auth");
        const handler = withAuth(() => Response.json({ status: "OK" }));
        const req = new Request(`${BASE}/api/test`, {
            headers: { "X-API-Key": "wrong-key" },
        });
        const res = await handler(req);
        expect(res.status).toBe(401);
    });

    it("calls through when correct key is provided", async () => {
        const { withAuth } = await import("../src/middleware/auth");
        const handler = withAuth(() => Response.json({ status: "OK" }));
        const req = new Request(`${BASE}/api/test`, {
            headers: { "X-API-Key": "test-key-123" },
        });
        const res = await handler(req);
        expect(res.status).toBe(200);
        const body = await res.json();
        expect(body.status).toBe("OK");
    });
});
