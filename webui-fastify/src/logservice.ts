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

    const resp = await fetch(url, { headers });

    if (!resp.ok) {
        throw new Error(`logservice GET ${path} responded ${resp.status}`);
    }

    return resp.json();
}
