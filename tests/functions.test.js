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

test("buildConnInfo maps connection fields onto the log payload", () => {
    const connection = {
        uuid: "c-1",
        start_time: 123,
        state: 5,
        encoding: "utf8",
        client: { remoteAddress: "1.2.3.4", remotePort: 54321 },
        remote_is_local: false,
        remote_is_private: true,
        remote_host: "host.example",
        remote_info: "info",
        hello: { host: "helo.example" },
        using_tls: true,
        tran_count: 1,
        pipelining: false,
        rcpt_count: { accept: 2, tempfail: 1, reject: 0 },
    };

    const obj = functions.buildConnInfo(connection);
    assert.equal(obj.uuid, "c-1");
    assert.equal(obj.remoteAddr, "1.2.3.4");
    assert.equal(obj.remotePort, 54321);
    assert.equal(obj.hello_name, "helo.example");
    assert.equal(obj.rcpt_count_accept, 2);
    assert.equal(obj.rcpt_count_tempfail, 1);
});

test("buildConnInfo does not throw when early-stage fields are absent", () => {
    // hook_connect fires before EHLO/RCPT: hello, client, rcpt_count may be unset
    const obj = functions.buildConnInfo({ uuid: "c-2" });
    assert.equal(obj.uuid, "c-2");
    assert.equal(obj.remoteAddr, undefined);
    assert.equal(obj.hello_name, undefined);
    assert.equal(obj.rcpt_count_accept, undefined);
});

// postWithLogging posts the payload via httplog and records the outcome to a
// local logfile. Stub both so nothing hits the network or disk.
function withStubbedIO(httplogImpl, run) {
    const origHttplog = functions.httplog;
    const origLog = functions.log;
    const logs = [];
    functions.httplog = httplogImpl;
    functions.log = (msg) => logs.push(msg);
    return Promise.resolve()
        .then(() => run(logs))
        .finally(() => {
            functions.httplog = origHttplog;
            functions.log = origLog;
        });
}

test("postWithLogging posts the payload and logs a successful response", async () => {
    const calls = [];
    await withStubbedIO(
        (obj, url) => {
            calls.push({ obj, url });
            return Promise.resolve({ status: 200, statusText: "OK" });
        },
        async (logs) => {
            await functions.postWithLogging({ a: 1 }, "http://logservice", "f.log");
            assert.deepEqual(calls[0], { obj: { a: 1 }, url: "http://logservice" });
            assert.ok(logs.some((l) => l.includes('"status":200')));
        }
    );
});

test("postWithLogging logs a logfail when the response has no status", async () => {
    await withStubbedIO(
        () => Promise.resolve(undefined),
        async (logs) => {
            await functions.postWithLogging({ a: 1 }, "http://logservice", "f.log");
            assert.ok(logs.some((l) => l.includes("HTTP Logfail")));
        }
    );
});

test("postWithLogging logs a connect error when httplog rejects", async () => {
    await withStubbedIO(
        () => Promise.reject(new Error("boom")),
        async (logs) => {
            await functions.postWithLogging({ a: 1 }, "http://logservice", "f.log");
            assert.ok(logs.some((l) => l.includes("HTTP Connect Error")));
        }
    );
});
