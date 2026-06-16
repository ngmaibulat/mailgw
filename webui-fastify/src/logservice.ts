// Thin client for logservice's read API. The webui no longer queries the log
// tables directly for its viewer grids — it proxies to logservice, the service
// that owns that data. logservice's GET search endpoints take a `q` JSON param
// and return `{ status: "success", total, records }`, which is exactly the
// shape the w2ui grids (public/js/log-*.js) expect, so responses pass through
// verbatim.

const BASE = (process.env.LOGSERVICE_URL || "http://localhost:3000").replace(
    /\/+$/,
    "",
);
const API_KEY = process.env.LOGSERVICE_API_KEY || "";
// Abort the upstream fetch if logservice doesn't respond in time, so a hung
// logservice can't hang the webui request indefinitely.
const TIMEOUT_MS = Number(process.env.LOGSERVICE_TIMEOUT_MS) || 10_000;

// Thrown when the logservice request fails. `kind` lets the route map it to the
// right gateway status: "upstream" (logservice answered non-2xx) -> 502,
// "network" (couldn't reach/parse logservice) -> 504. `upstreamStatus`/`body`
// carry context for logging.
export class LogserviceError extends Error {
    readonly kind: "upstream" | "network";
    readonly upstreamStatus?: number;
    readonly body?: string;

    constructor(
        message: string,
        kind: "upstream" | "network",
        upstreamStatus?: number,
        body?: string,
    ) {
        super(message);
        this.name = "LogserviceError";
        this.kind = kind;
        this.upstreamStatus = upstreamStatus;
        this.body = body;
    }
}

// The frontend sends its search payload as `?request=<json>`; logservice reads
// it from `?q=<json>`. Same JSON structure ({ search, searchLogic, limit,
// offset }), just a different param name — so we forward it across.
export async function search(
    path: string,
    rawRequest?: string,
): Promise<unknown> {
    const url = new URL(BASE + path);
    if (rawRequest) {
        url.searchParams.set("q", rawRequest);
    }

    const headers: Record<string, string> = {};
    if (API_KEY) {
        headers["X-API-Key"] = API_KEY;
    }

    let resp: Response;
    try {
        resp = await fetch(url, {
            headers,
            signal: AbortSignal.timeout(TIMEOUT_MS),
        });
    } catch (err) {
        const e = err as Error;
        const reason =
            e.name === "TimeoutError"
                ? `timed out after ${TIMEOUT_MS}ms`
                : e.message;
        throw new LogserviceError(
            `logservice GET ${path} unreachable: ${reason}`,
            "network",
        );
    }

    if (!resp.ok) {
        const body = await resp.text().catch(() => "");
        throw new LogserviceError(
            `logservice GET ${path} responded ${resp.status}`,
            "upstream",
            resp.status,
            body,
        );
    }

    try {
        return await resp.json();
    } catch (err) {
        throw new LogserviceError(
            `logservice GET ${path} returned invalid JSON: ${(err as Error).message}`,
            "network",
        );
    }
}
