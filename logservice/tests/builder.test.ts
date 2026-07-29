import { describe, it, expect } from "bun:test";
import { buildOrderBy, buildWhere, parseSearchQuery } from "../src/query/builder";

const FIELDS = new Set(["sender", "dt", "host", "delay", "tls"]);

describe("buildWhere", () => {
    it("returns empty sql when no params", () => {
        const { sql, values } = buildWhere([], "AND", FIELDS);
        expect(sql).toBe("");
        expect(values).toEqual([]);
    });

    it("handles 'is' operator", () => {
        const { sql, values } = buildWhere(
            [{ field: "sender", operator: "is", value: "user@example.com" }],
            "AND", FIELDS
        );
        expect(sql).toBe("`sender` = ?");
        expect(values).toEqual(["user@example.com"]);
    });

    it("handles 'contains' operator", () => {
        const { sql, values } = buildWhere(
            [{ field: "sender", operator: "contains", value: "gmail" }],
            "AND", FIELDS
        );
        expect(sql).toBe("`sender` LIKE ?");
        expect(values).toEqual(["%gmail%"]);
    });

    it("handles 'begins' operator", () => {
        const { sql, values } = buildWhere(
            [{ field: "sender", operator: "begins", value: "user" }],
            "AND", FIELDS
        );
        expect(sql).toBe("`sender` LIKE ?");
        expect(values).toEqual(["user%"]);
    });

    it("handles 'ends' operator", () => {
        const { sql, values } = buildWhere(
            [{ field: "sender", operator: "ends", value: ".com" }],
            "AND", FIELDS
        );
        expect(sql).toBe("`sender` LIKE ?");
        expect(values).toEqual(["%.com"]);
    });

    it("handles 'between' operator", () => {
        const { sql, values } = buildWhere(
            [{ field: "dt", operator: "between", value: [1000, 2000] }],
            "AND", FIELDS
        );
        expect(sql).toBe("`dt` BETWEEN ? AND ?");
        expect(values).toEqual([1000, 2000]);
    });

    it("handles '>' and 'more' operators", () => {
        const r1 = buildWhere([{ field: "delay", operator: ">",    value: 5 }], "AND", FIELDS);
        const r2 = buildWhere([{ field: "delay", operator: "more", value: 5 }], "AND", FIELDS);
        expect(r1.sql).toBe("`delay` > ?");
        expect(r2.sql).toBe("`delay` > ?");
        expect(r1.values).toEqual([5]);
    });

    it("handles '<' and 'less' operators", () => {
        const r1 = buildWhere([{ field: "delay", operator: "<",    value: 5 }], "AND", FIELDS);
        const r2 = buildWhere([{ field: "delay", operator: "less", value: 5 }], "AND", FIELDS);
        expect(r1.sql).toBe("`delay` < ?");
        expect(r2.sql).toBe("`delay` < ?");
    });

    it("joins multiple conditions with AND", () => {
        const { sql, values } = buildWhere(
            [
                { field: "sender", operator: "contains", value: "gmail" },
                { field: "tls",    operator: "is",       value: 1 },
            ],
            "AND", FIELDS
        );
        expect(sql).toBe("`sender` LIKE ? AND `tls` = ?");
        expect(values).toEqual(["%gmail%", 1]);
    });

    it("joins multiple conditions with OR", () => {
        const { sql } = buildWhere(
            [
                { field: "sender", operator: "is", value: "a@a.com" },
                { field: "host",   operator: "is", value: "smtp.example.com" },
            ],
            "OR", FIELDS
        );
        expect(sql).toBe("`sender` = ? OR `host` = ?");
    });

    it("silently drops fields not in allowlist", () => {
        const { sql, values } = buildWhere(
            [{ field: "password", operator: "is", value: "secret" }],
            "AND", FIELDS
        );
        expect(sql).toBe("");
        expect(values).toEqual([]);
    });

    it("silently drops params with empty value", () => {
        const { sql } = buildWhere(
            [{ field: "sender", operator: "is", value: "" }],
            "AND", FIELDS
        );
        expect(sql).toBe("");
    });

    // The join operator used to be interpolated straight from caller-supplied
    // JSON, so `searchLogic: "OR 1=1 OR"` was a filter-bypass SQL injection.
    const TWO_PARAMS = [
        { field: "sender", operator: "is", value: "a@a.com" },
        { field: "host",   operator: "is", value: "smtp.example.com" },
    ] as const;

    it("never interpolates a hostile logic value into the sql", () => {
        const { sql, values } = buildWhere(
            [...TWO_PARAMS],
            // biome-ignore lint/suspicious/noExplicitAny: testing bad input
            "OR 1=1 OR" as any,
            FIELDS
        );
        expect(sql).toBe("`sender` = ? AND `host` = ?");
        expect(sql).not.toContain("1=1");
        expect(values).toEqual(["a@a.com", "smtp.example.com"]);
    });

    it("normalises the logic operator case-insensitively", () => {
        for (const logic of ["or", "Or", "OR"]) {
            // biome-ignore lint/suspicious/noExplicitAny: testing bad input
            const { sql } = buildWhere([...TWO_PARAMS], logic as any, FIELDS);
            expect(sql).toBe("`sender` = ? OR `host` = ?");
        }
    });

    it("falls back to AND for junk, empty and missing logic", () => {
        // biome-ignore lint/suspicious/noExplicitAny: testing bad input
        for (const logic of ["junk", "", null, undefined] as any[]) {
            const { sql } = buildWhere([...TWO_PARAMS], logic, FIELDS);
            expect(sql).toBe("`sender` = ? AND `host` = ?");
        }
    });

    it("blocks subquery and time-based injection payloads", () => {
        const payloads = [
            "AND `id` IN (SELECT id FROM Users WHERE hash LIKE 'a%') AND",
            "AND (SELECT SLEEP(5)) AND",
            "UNION SELECT",
        ];
        for (const logic of payloads) {
            // biome-ignore lint/suspicious/noExplicitAny: testing bad input
            const { sql } = buildWhere([...TWO_PARAMS], logic as any, FIELDS);
            expect(sql).toBe("`sender` = ? AND `host` = ?");
        }
    });
});

describe("parseSearchQuery", () => {
    it("returns {} for missing or malformed input", () => {
        expect(parseSearchQuery(null)).toEqual({});
        expect(parseSearchQuery("")).toEqual({});
        expect(parseSearchQuery("{not json")).toEqual({});
    });

    it("returns {} for non-object json", () => {
        expect(parseSearchQuery("null")).toEqual({});
        expect(parseSearchQuery('"str"')).toEqual({});
        expect(parseSearchQuery("[1,2]")).toEqual({});
        expect(parseSearchQuery("42")).toEqual({});
    });

    it("strips a hostile searchLogic down to AND", () => {
        const q = parseSearchQuery('{"searchLogic":"OR 1=1 OR"}');
        expect(q.searchLogic).toBe("AND");
    });

    it("preserves a valid searchLogic and the rest of the query", () => {
        const q = parseSearchQuery(
            '{"searchLogic":"OR","limit":50,"offset":10,' +
                '"search":[{"field":"sender","operator":"is","value":"a@a.com"}]}'
        );
        expect(q.searchLogic).toBe("OR");
        expect(q.limit).toBe(50);
        expect(q.offset).toBe(10);
        expect(q.search).toEqual([
            { field: "sender", operator: "is", value: "a@a.com" },
        ]);
    });

    it("defaults searchLogic to AND when absent", () => {
        expect(parseSearchQuery('{"limit":10}').searchLogic).toBe("AND");
    });
});

describe("buildOrderBy", () => {
    it("returns the fallback when no sort given", () => {
        expect(buildOrderBy(undefined, FIELDS, "`id` DESC")).toBe("`id` DESC");
        expect(buildOrderBy([], FIELDS, "`id` DESC")).toBe("`id` DESC");
    });

    it("defaults direction to DESC", () => {
        expect(buildOrderBy([{ field: "dt" }], FIELDS, "`id` DESC")).toBe(
            "`dt` DESC"
        );
    });

    it("normalises asc/desc (case-insensitive) and ignores junk", () => {
        expect(
            // biome-ignore lint/suspicious/noExplicitAny: testing bad input
            buildOrderBy([{ field: "dt", direction: "ASC" as any }], FIELDS, "x")
        ).toBe("`dt` ASC");
        expect(
            // biome-ignore lint/suspicious/noExplicitAny: testing bad input
            buildOrderBy([{ field: "dt", direction: "junk" as any }], FIELDS, "x")
        ).toBe("`dt` DESC");
    });

    it("joins multiple sort columns in order", () => {
        const sql = buildOrderBy(
            [
                { field: "host", direction: "asc" },
                { field: "delay", direction: "desc" },
            ],
            FIELDS,
            "`id` DESC"
        );
        expect(sql).toBe("`host` ASC, `delay` DESC");
    });

    it("drops fields not in the allowlist, falling back if none remain", () => {
        expect(
            buildOrderBy([{ field: "password" }], FIELDS, "`id` DESC")
        ).toBe("`id` DESC");
        expect(
            buildOrderBy(
                [{ field: "password" }, { field: "dt", direction: "asc" }],
                FIELDS,
                "`id` DESC"
            )
        ).toBe("`dt` ASC");
    });

    it("qualifies columns with a table prefix for joins", () => {
        expect(
            buildOrderBy([{ field: "md5" }], new Set(["md5"]), "`h`.`id` DESC", "h")
        ).toBe("`h`.`md5` DESC");
    });
});
