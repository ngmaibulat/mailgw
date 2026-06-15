export type SearchOperator =
    | "is" | "=" | "begins" | "contains" | "ends"
    | "between" | ">" | ">=" | "<" | "<=" | "less" | "more";

export type SearchLogic = "AND" | "OR";

export interface SearchParam {
    field: string;
    operator: SearchOperator;
    value: string | number | [string | number, string | number];
}

export interface SearchQuery {
    limit?: number;
    offset?: number;
    search?: SearchParam[];
    searchLogic?: SearchLogic;
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

export function parseSearchQuery(raw: string | null): SearchQuery {
    if (!raw) return {};
    try {
        return JSON.parse(raw) as SearchQuery;
    } catch {
        return {};
    }
}
