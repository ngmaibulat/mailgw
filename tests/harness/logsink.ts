/**
 * A fake logservice.
 *
 * The gateway posts three kinds of audit event and asks one question
 * (/filter/md5). Standing in for logservice here buys three things a real one
 * cannot: the events are observable synchronously, the answers can be made to
 * fail on demand, and no database is involved.
 *
 * The failure modes are the point. internal/events already covers the retry
 * ladder and the "a 4xx is terminal" rule in Go; what only a real HTTP server
 * can show is the whole loop — a refusal, a spill into <spool>/failed-events/,
 * and the replayer draining it — happening while mail is flowing.
 */

export type EventKind = "connection" | "queue" | "delivery";

export interface RecordedRequest {
    path: string;
    headers: Record<string, string>;
    body: unknown;
    at: number;
}

export class LogSink {
    readonly connections: any[] = [];
    readonly queues: any[] = [];
    readonly deliveries: any[] = [];
    readonly requests: RecordedRequest[] = [];

    private server: Bun.Server<undefined>;
    private statuses = new Map<string, number | ((nth: number) => number)>();
    private counts = new Map<string, number>();
    private delays = new Map<string, number>();
    private md5Verdict: (hashes: string[]) => "allow" | "block" = () => "allow";
    private apiKey = "";

    private constructor(server: Bun.Server<undefined>) {
        this.server = server;
    }

    get url(): string {
        return `http://127.0.0.1:${this.server.port}`;
    }

    /** Drops straight into a bundle's `logging` key. */
    get logging(): { url_conn: string; url_queue: string; url_delivery: string; api_key?: string } {
        const base = this.url;
        const out = {
            url_conn: `${base}/api/connection`,
            url_queue: `${base}/api/queue`,
            url_delivery: `${base}/api/delivery`,
        };
        return this.apiKey ? { ...out, api_key: this.apiKey } : out;
    }

    /** Decide what /filter/md5 says about a set of digests. */
    md5(fn: (hashes: string[]) => "allow" | "block"): void {
        this.md5Verdict = fn;
    }

    /**
     * Answer a path with a status other than 200.
     *
     * A number is forever; a function of the attempt number lets a test fail
     * twice and then recover, which is what the replayer is for.
     */
    respond(path: string, status: number | ((nth: number) => number)): void {
        this.statuses.set(path, status);
    }

    /** Make a path slow, for the attach-scan and event timeouts. */
    hang(path: string, ms: number): void {
        this.delays.set(path, ms);
    }

    /** Require X-API-Key, so a bundle's logging.api_key can be checked. */
    requireApiKey(key: string): void {
        this.apiKey = key;
    }

    /** Restore the default answers, without dropping what was recorded. */
    healthy(): void {
        this.statuses.clear();
        this.delays.clear();
        this.counts.clear();
    }

    events(kind: EventKind): any[] {
        return kind === "connection" ? this.connections : kind === "queue" ? this.queues : this.deliveries;
    }

    async waitFor(
        kind: EventKind,
        pred: (e: any) => boolean = () => true,
        timeoutMs = 20_000,
    ): Promise<any> {
        const deadline = Date.now() + timeoutMs;
        for (;;) {
            const hit = this.events(kind).find(pred);
            if (hit) return hit;
            if (Date.now() > deadline) {
                throw new Error(
                    `no matching ${kind} event in ${timeoutMs}ms ` +
                        `(conn=${this.connections.length} queue=${this.queues.length} delivery=${this.deliveries.length})`,
                );
            }
            await Bun.sleep(25);
        }
    }

    reset(): void {
        this.connections.length = 0;
        this.queues.length = 0;
        this.deliveries.length = 0;
        this.requests.length = 0;
        this.healthy();
    }

    stop(): void {
        this.server.stop(true);
    }

    static start(): LogSink {
        let sink!: LogSink;
        const server = Bun.serve({
            hostname: "127.0.0.1",
            port: 0,
            fetch: (req) => sink.handle(req),
        });
        sink = new LogSink(server);
        return sink;
    }

    private async handle(req: Request): Promise<Response> {
        const path = new URL(req.url).pathname;

        const delay = this.delays.get(path);
        if (delay) await Bun.sleep(delay);

        let body: unknown = null;
        const text = await req.text();
        try {
            body = text ? JSON.parse(text) : null;
        } catch {
            body = text;
        }

        this.requests.push({
            path,
            headers: Object.fromEntries(req.headers.entries()),
            body,
            at: Date.now(),
        });

        if (this.apiKey && req.headers.get("x-api-key") !== this.apiKey) {
            return Response.json({ status: "Fail", error: "bad api key" }, { status: 401 });
        }

        const forced = this.statusFor(path);
        if (forced && forced !== 200) {
            return Response.json({ status: "Fail" }, { status: forced });
        }

        switch (path) {
            case "/api/connection":
                this.connections.push(body);
                return Response.json({ status: "OK" });
            case "/api/queue":
                this.queues.push(body);
                return Response.json({ status: "OK" });
            case "/api/delivery":
                this.deliveries.push(body);
                return Response.json({ status: "OK" });
            case "/filter/md5": {
                const hashes = Array.isArray(body)
                    ? body.map((a: { md5?: string }) => a.md5 ?? "")
                    : [];
                return Response.json({ action: this.md5Verdict(hashes) });
            }
            default:
                return Response.json({ status: "OK" });
        }
    }

    private statusFor(path: string): number | undefined {
        const rule = this.statuses.get(path);
        if (rule === undefined) return undefined;
        if (typeof rule === "number") return rule;
        const nth = (this.counts.get(path) ?? 0) + 1;
        this.counts.set(path, nth);
        return rule(nth);
    }
}

export function startLogSink(): LogSink {
    return LogSink.start();
}
