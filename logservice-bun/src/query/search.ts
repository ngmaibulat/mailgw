import db from "../db";
import { buildWhere, parseSearchQuery } from "./builder";
import type { SearchLogic, SearchParam } from "./builder";

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

async function searchTable<T>(
    table: string,
    allowedFields: Set<string>,
    params: SearchParam[],
    logic: SearchLogic,
    limit: number,
    offset: number,
): Promise<SearchResult<T>> {
    const { sql: where, values } = buildWhere(params, logic, allowedFields);

    const whereSql = where ? `WHERE ${where}` : "";
    const sql = `SELECT * FROM \`${table}\` ${whereSql} ORDER BY id DESC LIMIT ? OFFSET ?`;

    const rows = await db.unsafe(sql, [...values, limit, offset]) as T[];

    return {
        status: "success",
        total: rows.length,
        records: rows,
    };
}

export async function searchDelivery(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Delivery", DELIVERY_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0,
    );
}

export async function searchConnection(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Connection", CONNECTION_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0,
    );
}

export async function searchTransaction(rawQuery: string | null) {
    const q = parseSearchQuery(rawQuery);
    return searchTable(
        "Transaction", TRANSACTION_FIELDS,
        q.search ?? [], q.searchLogic ?? "AND",
        q.limit ?? 100, q.offset ?? 0,
    );
}
