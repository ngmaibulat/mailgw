const { test, beforeEach, afterEach } = require("node:test");
const assert = require("node:assert/strict");

const functions = require("../plugins/functions");

test("getAddr joins user and host with @", () => {
    assert.equal(functions.getAddr({ user: "alice", host: "a.com" }), "alice@a.com");
});

test("getAddrList comma-joins addresses, no trailing comma", () => {
    const list = [
        { user: "a", host: "x.com" },
        { user: "b", host: "y.com" },
    ];
    assert.equal(functions.getAddrList(list), "a@x.com,b@y.com");
});

test("getAddrList returns empty string for an empty list", () => {
    assert.equal(functions.getAddrList([]), "");
});

// log_transaction / log_connection shape an object and hand it to httplog.
// httplog itself calls fetch(); we stub it so nothing leaves the process and
// we can assert on the shaped payload.
test("log_transaction posts {status:'empty'} when txn is missing", () => {
    const original = functions.httplog;
    const calls = [];
    functions.httplog = (obj, url) => calls.push({ obj, url });
    try {
        functions.log_transaction(null, "http://logservice/queue");
    } finally {
        functions.httplog = original;
    }

    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].obj, { status: "empty" });
    assert.equal(calls[0].url, "http://logservice/queue");
});

test("log_transaction maps txn fields onto the logged payload", () => {
    const original = functions.httplog;
    const calls = [];
    functions.httplog = (obj, url) => calls.push({ obj, url });

    const txn = {
        uuid: "u-1",
        data_post_start: "2026-01-01T00:00:00Z",
        action: "accept",
        data_post_delay: 12,
        encoding: "utf8",
        data_bytes: 2048,
        mime_part_count: 2,
        mail_from: { user: "sender", host: "from.com" },
        rcpt_to: [{ user: "rcpt", host: "to.com" }],
        rcpt_count: { accept: 1, tempfail: 0, reject: 0 },
        header: { headers_decoded: { subject: "hi" } },
    };

    try {
        functions.log_transaction(txn, "http://logservice/queue");
    } finally {
        functions.httplog = original;
    }

    assert.equal(calls.length, 1);
    const obj = calls[0].obj;
    assert.equal(obj.uuid, "u-1");
    assert.equal(obj.sender, "sender@from.com");
    assert.equal(obj.data_bytes, 2048);
    assert.equal(obj.rcpt_count_accept, 1);
    assert.equal(obj.rcpt_count_reject, 0);
    assert.deepEqual(obj.headers, { subject: "hi" });
});
