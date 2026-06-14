const { test } = require("node:test");
const assert = require("node:assert/strict");

const Route = require("../plugins/Route");

// Route constructor signature:
//   new Route(relay, sender, sender_domain, rcpt, rcpt_domain)
// An empty-string predicate field means "wildcard" (matches anything).

test("getDomain extracts the part after the last @", () => {
    const r = new Route("relay", "", "", "", "");
    assert.equal(r.getDomain("addr@example.com"), "example.com");
    assert.equal(r.getDomain("a@b@example.com"), "example.com");
});

test("all-wildcard route matches any sender/rcpt", () => {
    const r = new Route("relay", "", "", "", "");
    assert.equal(r.match("anyone@a.com", "anyone@b.com"), true);
});

test("exact sender must match", () => {
    const r = new Route("relay", "boss@a.com", "", "", "");
    assert.equal(r.match("boss@a.com", "x@b.com"), true);
    assert.equal(r.match("other@a.com", "x@b.com"), false);
});

test("sender_domain is matched against the sender's domain", () => {
    const r = new Route("relay", "", "a.com", "", "");
    assert.equal(r.match("anyone@a.com", "x@b.com"), true);
    assert.equal(r.match("anyone@other.com", "x@b.com"), false);
});

test("exact rcpt must match", () => {
    const r = new Route("relay", "", "", "user@b.com", "");
    assert.equal(r.match("x@a.com", "user@b.com"), true);
    assert.equal(r.match("x@a.com", "nope@b.com"), false);
});

test("rcpt_domain is matched against the rcpt's domain", () => {
    const r = new Route("relay", "", "", "", "b.com");
    assert.equal(r.match("x@a.com", "user@b.com"), true);
    assert.equal(r.match("x@a.com", "user@other.com"), false);
});

test("domain matching is case-insensitive", () => {
    const r = new Route("relay", "", "Example.com", "", "B.COM");
    assert.equal(r.match("s@EXAMPLE.COM", "r@b.com"), true);
    assert.equal(r.match("s@example.com", "r@B.Com"), true);
});

test("address matching is case-insensitive", () => {
    const r = new Route("relay", "Boss@A.com", "", "USER@b.com", "");
    assert.equal(r.match("boss@a.com", "user@B.COM"), true);
});

test("all specified predicates must hold together (AND)", () => {
    const r = new Route("relay", "", "a.com", "", "b.com");
    assert.equal(r.match("s@a.com", "r@b.com"), true);
    assert.equal(r.match("s@a.com", "r@wrong.com"), false);
    assert.equal(r.match("s@wrong.com", "r@b.com"), false);
});

test("getCheckerFunction returns a true-wildcard for empty input", () => {
    const r = new Route("relay", "", "", "", "");
    const checker = r.getCheckerFunction("");
    assert.equal(checker("anything"), true);
});

test("getCheckerFunction does exact (string) comparison for non-empty input", () => {
    const r = new Route("relay", "", "", "", "");
    const checker = r.getCheckerFunction("match-me");
    assert.equal(checker("match-me"), true);
    assert.equal(checker("nope"), false);
});

test("getCheckerFunction treats null/undefined as a wildcard (does not throw)", () => {
    const r = new Route("relay", "", "", "", "");
    assert.equal(r.getCheckerFunction(undefined)("anything"), true);
    assert.equal(r.getCheckerFunction(null)("anything"), true);
});

test("a route built from a config with missing fields matches anything", () => {
    // e.g. routing.json entry that omits sender/rcpt keys entirely
    const r = new Route("relay", undefined, undefined, undefined, undefined);
    assert.equal(r.match("anyone@a.com", "anyone@b.com"), true);
});
