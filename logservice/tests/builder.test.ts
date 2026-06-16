import { describe, it, expect } from "bun:test";
import { buildOrderBy, buildWhere } from "../src/query/builder";

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
