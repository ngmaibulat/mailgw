import db from "../db";
import { buildOrderBy, buildWhere, parseSearchQuery } from "./builder";
import type { SearchLogic, SearchParam, SortParam } from "./builder";

export interface SearchResult<T> {
    status: "success";
    total: number;
    records: T[];
}

const DELIVERY_FIELDS = new Set([
    "id", "uuid", "dt", "sender", "rcpt_domain", "rcpt_list", "rcpt_accepted",
    "tls_forced", "tls", "auth", "host", "ip", "port", "response", "delay",
]);

const CONNECTION_FIELDS = new Set([
    "id", "uuid", "dt", "encoding", "hello_name", "remoteAddr", "remotePort",
    "remote_host", "remote_info", "remote_is_local", "remote_is_private",
    "using_tls", "tran_count", "rcpt_count_accept", "rcpt_count_tempfail", "rcpt_count_reject",
]);

const TRANSACTION_FIELDS = new Set([
    "id", "uuid", "dt", "action", "encoding", "sender", "rcpt_list",
    "rcpt_count_accept", "rcpt_count_tempfail", "rcpt_count_reject",
    "delay_data_post", "data_bytes", "mime_part_count",
]);

// Only HashLookups columns are searchable; the joined Transaction columns
// (sender, rcpt_list, dt) are display-only in the viewer grid.
const HASHLOOKUP_FIELDS = new Set([
    "id", "txn_uuid", "md5", "contentType", "filename", "size", "action", "createdAt",
]);

async function searchTable<T>(
    table: string,
    allowedFields: Set<string>,
    params: SearchParam[],
    logic: SearchLogic,
    limit: number,
    offset: number,
    sort: SortParam[] | undefined,
): Promise<SearchResult<T>> {
    const { sql: where, values } = buildWhere(params, logic, allowedFields);

    const whereSql = where ? `WHERE ${where}` : "";

    // `total` is the full count of matching rows (ignoring limit/offset) so the
    // client can paginate; the same WHERE values are reused.
    const countSql = `SELECT COUNT(*) AS total FROM \`${table}\` ${whereSql}`;
    const countRows = (await db.unsafe(countSql, values)) as {
        total: number | bigint | string;
    }[];
    const total = Number(countRows[0]?.total ?? 0);

    const orderBy = buildOrderBy(sort, allowedFields, "`id` DESC");
    const sql = `SELECT * FROM \`${table}\` ${whereSql} ORDER BY ${orderBy} LIMIT ? OFFSET ?`;
    const rows = (await db.unsafe(sql, [...values, limit, offset])) as T[];

    return {
        status: "success",
        total,
        records: rows,
    };
}

export async function searchDelivery(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Delivery", DELIVERY_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0, q.sort,
    );
}

export async function searchConnection(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Connection", CONNECTION_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0, q.sort,
    );
}

export async function searchTransaction(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Transaction", TRANSACTION_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0, q.sort,
    );
}

// HashLookups joined to its originating Transaction (txn_uuid -> uuid) so the
// lookup viewer can show the message's sender/rcpt alongside the attachment.
// `h.action` (allow/block for the attachment) is kept as `action`; the
// transaction's own action is surfaced as `txn_action`.
export async function searchHashlookup(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);

    const { sql: where, values } = buildWhere(
        q.search ?? [], q.searchLogic ?? "AND", HASHLOOKUP_FIELDS, "h",
    );
    const whereSql = where ? `WHERE ${where}` : "";
    const limit = q.limit ?? 100;
    const offset = q.offset ?? 0;
    // Sort is restricted to the searchable HashLookups columns (the joined
    // Transaction columns are display-only), qualified to the `h` alias.
    const orderBy = buildOrderBy(q.sort, HASHLOOKUP_FIELDS, "`h`.`id` DESC", "h");

    const fromJoin = `
        FROM \`HashLookups\` h
        LEFT JOIN \`Transaction\` t ON t.uuid = h.txn_uuid
        ${whereSql}`;

    const countSql = `SELECT COUNT(*) AS total ${fromJoin}`;
    const countRows = (await db.unsafe(countSql, values)) as {
        total: number | bigint | string;
    }[];
    const total = Number(countRows[0]?.total ?? 0);

    const sql = `
        SELECT h.*,
               t.dt, t.sender, t.rcpt_list, t.encoding,
               t.action AS txn_action,
               t.rcpt_count_accept, t.rcpt_count_tempfail, t.rcpt_count_reject,
               t.delay_data_post, t.data_bytes, t.mime_part_count
        ${fromJoin}
        ORDER BY ${orderBy}
        LIMIT ? OFFSET ?`;

    const rows = await db.unsafe(sql, [...values, limit, offset]);

    return {
        status: "success" as const,
        total,
        records: rows,
    };
}
