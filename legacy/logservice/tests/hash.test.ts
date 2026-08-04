import { describe, it, expect } from "bun:test";
import { decideActions } from "../src/query/hash";

describe("decideActions", () => {
    it("allows when no attachment md5 is on the blocklist", () => {
        const { overall, decisions } = decideActions(
            [{ md5: "aaa" }, { md5: "bbb" }],
            new Set<string>(),
        );
        expect(overall).toBe("allow");
        expect(decisions.map((d) => d.action)).toEqual(["allow", "allow"]);
    });

    it("blocks the message if any attachment md5 is blocked", () => {
        const { overall, decisions } = decideActions(
            [{ md5: "aaa" }, { md5: "bad" }],
            new Set(["bad"]),
        );
        expect(overall).toBe("block");
        expect(decisions.find((d) => d.md5 === "bad")!.action).toBe("block");
        expect(decisions.find((d) => d.md5 === "aaa")!.action).toBe("allow");
    });

    it("allows an empty attachment list", () => {
        const { overall, decisions } = decideActions([], new Set(["bad"]));
        expect(overall).toBe("allow");
        expect(decisions).toEqual([]);
    });

    it("treats an attachment without an md5 as allow", () => {
        const { overall, decisions } = decideActions(
            [{ filename: "x.pdf" }],
            new Set(["bad"]),
        );
        expect(overall).toBe("allow");
        expect(decisions[0].action).toBe("allow");
    });
});
