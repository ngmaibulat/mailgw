export type SearchOperator =
    | "is" | "=" | "begins" | "contains" | "ends"
    | "between" | ">" | ">=" | "<" | "<=" | "less" | "more";

export type SearchLogic = "AND" | "OR";

export type SortDirection = "asc" | "desc";

export interface SearchParam {
    field: string;
    operator: SearchOperator;
    value: string | number | [string | number, string | number];
}

export interface SortParam {
    field: string;
    direction?: SortDirection;
}

export interface SearchQuery {
    limit?: number;
    offset?: number;
    search?: SearchParam[];
    searchLogic?: SearchLogic;
    sort?: SortParam[];
}

export interface WhereClause {
    sql: string;
    values: (string | number)[];
}

export function buildWhere(
    params: SearchParam[],
    logic: SearchLogic,
    allowedFields: Set<string>,
    tablePrefix?: string,
): WhereClause {
    const conditions: string[] = [];
    const values: (string | number)[] = [];

    for (const item of params) {
        if (!item.value && item.value !== 0) continue;
        if (!allowedFields.has(item.field)) continue;

        // Qualify columns with a table alias when querying a JOIN, so fields
        // that exist in both tables (e.g. action, createdAt) aren't ambiguous.
        const col = tablePrefix ? `\`${tablePrefix}\`.\`${item.field}\`` : `\`${item.field}\``;

        switch (item.operator) {
            case "is":
            case "=":
                conditions.push(`${col} = ?`);
                values.push(item.value as string | number);
                break;
            case "begins":
                conditions.push(`${col} LIKE ?`);
                values.push(`${item.value}%`);
                break;
            case "contains":
                conditions.push(`${col} LIKE ?`);
                values.push(`%${item.value}%`);
                break;
            case "ends":
                conditions.push(`${col} LIKE ?`);
                values.push(`%${item.value}`);
                break;
            case "between":
                if (Array.isArray(item.value) && item.value.length === 2) {
                    conditions.push(`${col} BETWEEN ? AND ?`);
                    values.push(item.value[0], item.value[1]);
                }
                break;
            case ">":
            case "more":
                conditions.push(`${col} > ?`);
                values.push(item.value as string | number);
                break;
            case ">=":
                conditions.push(`${col} >= ?`);
                values.push(item.value as string | number);
                break;
            case "<":
            case "less":
                conditions.push(`${col} < ?`);
                values.push(item.value as string | number);
                break;
            case "<=":
                conditions.push(`${col} <= ?`);
                values.push(item.value as string | number);
                break;
        }
    }

    return {
        sql: conditions.length > 0 ? conditions.join(` ${logic} `) : "",
        values,
    };
}

// Build a safe `ORDER BY` body from caller-supplied sort params. Like
// `buildWhere`, every field is checked against `allowedFields` (silently
// dropped otherwise) and quoted, and the direction is normalised to the literal
// `ASC`/`DESC` (default `DESC`) — so neither field names nor direction are ever
// interpolated from raw input. Returns `fallback` when no valid sort remains,
// so the SQL always has a deterministic order (callers pass e.g. "`id` DESC").
export function buildOrderBy(
    sort: SortParam[] | undefined,
    allowedFields: Set<string>,
    fallback: string,
    tablePrefix?: string,
): string {
    if (!sort || sort.length === 0) return fallback;

    const parts: string[] = [];
    for (const item of sort) {
        if (!allowedFields.has(item.field)) continue;
        const col = tablePrefix
            ? `\`${tablePrefix}\`.\`${item.field}\``
            : `\`${item.field}\``;
        const dir =
            String(item.direction).toLowerCase() === "asc" ? "ASC" : "DESC";
        parts.push(`${col} ${dir}`);
    }

    return parts.length > 0 ? parts.join(", ") : fallback;
}

export function parseSearchQuery(raw: string | null): SearchQuery {
    if (!raw) return {};
    try {
        return JSON.parse(raw) as SearchQuery;
    } catch {
        return {};
    }
}
