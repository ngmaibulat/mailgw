import { z } from "zod/v4";

import { zodErr } from "./config.ts";

// Mirrors logservice's accepted search payload (logservice/src/query/builder.ts
// — SearchQuery / SearchParam). The webui proxies the frontend's
// `?request=<json>` straight through to logservice's `?q=` param; validating it
// here lets us reject a malformed request with a 400 instead of bouncing it off
// logservice, and strips unknown keys before forwarding. logservice still does
// the authoritative field whitelist + parse, so this is defense-in-depth.
const OPERATORS = [
    "is",
    "=",
    "begins",
    "contains",
    "ends",
    "between",
    ">",
    ">=",
    "<",
    "<=",
    "less",
    "more",
] as const;

// Search values are scalars, or a 2-tuple for the `between` operator. Empty
// strings are allowed — logservice's buildWhere simply skips empty values.
const scalar = z.union([z.string(), z.number()]);

const searchParam = z.object({
    field: z.string().min(1),
    operator: z.enum(OPERATORS),
    value: z.union([scalar, z.tuple([scalar, scalar])]),
});

// Sort params mirror logservice's SortParam (src/query/builder.ts). logservice
// whitelists the field + normalises the direction, so this is just shape
// validation; an absent direction defaults to "desc" there.
const sortParam = z.object({
    field: z.string().min(1),
    direction: z.enum(["asc", "desc"]).optional(),
});

export const searchRequest = z.object({
    search: z.array(searchParam).optional(),
    searchLogic: z.enum(["AND", "OR"]).optional(),
    sort: z.array(sortParam).optional(),
    limit: z.int().min(0).optional(),
    offset: z.int().min(0).optional(),
});

export type SearchRequestResult =
    | { ok: true; value: string | undefined }
    | { ok: false; error: string };

// Decode + validate the frontend's `?request=<json>` string. Returns the
// re-serialized (validated, unknown-keys-stripped) JSON to forward, or an error
// message for a 400. An absent/empty request is valid — logservice applies its
// own defaults (limit 100, offset 0, no filter).
export function parseSearchRequest(raw: unknown): SearchRequestResult {
    if (raw === undefined || raw === "") {
        return { ok: true, value: undefined };
    }
    if (typeof raw !== "string") {
        return { ok: false, error: "search request must be a JSON string" };
    }

    let decoded: unknown;
    try {
        decoded = JSON.parse(raw);
    } catch {
        return { ok: false, error: "search request is not valid JSON" };
    }

    const parsed = searchRequest.safeParse(decoded);
    if (!parsed.success) {
        return { ok: false, error: zodErr(parsed.error) };
    }

    return { ok: true, value: JSON.stringify(parsed.data) };
}
