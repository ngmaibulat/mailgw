/**
 * A typed client for the test-only control API.
 *
 * The types below mirror `mailgw-go/internal/node/control.go` and
 * `internal/testctl/server.go`. They are hand-written rather than generated
 * because that mirroring IS a contract test: if the Go wire shape changes and
 * this file does not, a suite fails, which is the point.
 *
 * Every method throws a TestctlError carrying the status and the gateway's own
 * message. That message is the single most useful thing this API returns — the
 * rule compiler's complaint about which relay group does not exist — so it is
 * never paraphrased.
 */

/** node.Applied */
export interface Applied {
    version_id: number;
    version: number;
    sha256: string;
    restart_required: string[];
}

/** node.Status */
export interface Status {
    build: string;
    version: string;
    commit: string;
    fingerprint: string;
    gateway_uid: string;
    central_url: string;
    approval: string;
    provisioned: boolean;
    serving: boolean;
    applied_version: number | null;
    applied_version_id: number;
    apply_error: string;
    last_error: string;
    restart_required: string[];
    /** The addresses SMTP actually bound — not what the bundle asked for. */
    listeners: string[];
    /** The same answer for the admin listener, where /metrics and /readyz live. */
    admin_addr: string;
    spool_dir: string;
}

/** node.QueueEntry */
export interface QueueEntry {
    queue: "q" | "inflight" | "quarantine" | "dead" | string;
    name: string;
    uuid: string;
    sender: string;
    rcpts: string[] | null;
    relay_group: string;
    attempts: number;
    last_error?: string;
    due?: string;
    parse_error?: string;
    body_bytes?: number;
}

/** testctl.CachedBundle */
export interface CachedBundle {
    version_id: number;
    version: number;
    sha256: string;
    bundle: unknown;
}

export interface EnrollRequest {
    central_url: string;
    insecure_tls?: boolean;
    ca_file?: string;
}

export class TestctlError extends Error {
    constructor(
        readonly status: number,
        readonly reason: string,
        readonly method: string,
        readonly path: string,
    ) {
        super(`${method} ${path} -> ${status}: ${reason}`);
        this.name = "TestctlError";
    }
}

export class Testctl {
    constructor(readonly baseUrl: string) {}

    applyBundle(bundle: unknown): Promise<Applied> {
        return this.req("POST", "/testctl/config", bundle);
    }

    /**
     * Apply and expect a refusal, returning the gateway's reason.
     *
     * A separate method rather than a try/catch at every call site: "this
     * configuration must be refused" is an assertion, and a test that wrote it
     * as a catch would pass just as happily if the bundle were accepted.
     */
    async applyExpectingFailure(bundle: unknown): Promise<string> {
        try {
            const applied = await this.applyBundle(bundle);
            throw new Error(
                `the gateway ACCEPTED a bundle that should have been refused ` +
                    `(version ${applied.version_id})`,
            );
        } catch (e) {
            if (e instanceof TestctlError) return e.reason;
            throw e;
        }
    }

    applyProfiles(profiles: unknown): Promise<Applied> {
        return this.req("POST", "/testctl/config/profiles", profiles);
    }

    config(): Promise<CachedBundle> {
        return this.req("GET", "/testctl/config");
    }

    status(): Promise<Status> {
        return this.req("GET", "/testctl/status");
    }

    enroll(req: EnrollRequest): Promise<Status> {
        return this.req("POST", "/testctl/enroll", req);
    }

    reset(): Promise<{ reset: boolean; restart_required: boolean; note: string }> {
        return this.req("POST", "/testctl/reset");
    }

    async queue(): Promise<QueueEntry[]> {
        const { entries } = await this.req<{ entries: QueueEntry[] }>("GET", "/testctl/queue");
        return entries;
    }

    /** No uuids means the whole ready queue; the three verbs below refuse that. */
    async flush(uuids?: string[]): Promise<number> {
        const body = uuids ? { uuids } : undefined;
        const { flushed } = await this.req<{ flushed: number }>(
            "POST",
            "/testctl/queue/flush",
            body,
        );
        return flushed;
    }

    async release(uuids: string[]): Promise<number> {
        const r = await this.req<{ released: number }>("POST", "/testctl/queue/release", { uuids });
        return r.released;
    }

    async hold(uuids: string[]): Promise<number> {
        const r = await this.req<{ held: number }>("POST", "/testctl/queue/hold", { uuids });
        return r.held;
    }

    async remove(uuids: string[]): Promise<number> {
        const r = await this.req<{ removed: number }>("POST", "/testctl/queue/remove", { uuids });
        return r.removed;
    }

    /** Raw access, for asserting on status codes and malformed bodies. */
    async raw(method: string, path: string, body?: BodyInit): Promise<Response> {
        return fetch(this.baseUrl + path, {
            method,
            headers: body === undefined ? undefined : { "content-type": "application/json" },
            body,
            signal: AbortSignal.timeout(30_000),
        });
    }

    private async req<T = any>(method: string, path: string, body?: unknown): Promise<T> {
        const res = await this.raw(
            method,
            path,
            body === undefined ? undefined : JSON.stringify(body),
        );
        const text = await res.text();
        if (!res.ok) {
            let reason = text;
            try {
                reason = (JSON.parse(text) as { error?: string }).error ?? text;
            } catch {
                // Not JSON. The body is still the most useful thing we have.
            }
            throw new TestctlError(res.status, reason, method, path);
        }
        return text ? (JSON.parse(text) as T) : (undefined as T);
    }
}
