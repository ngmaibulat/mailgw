const { test } = require("node:test");
const assert = require("node:assert/strict");

// Register Haraka return-code globals (OK, DENYSOFT, CONT, ...) as Haraka does.
require("haraka-constants").import(global);

const npRoute = require("../plugins/npRoute");

// In Haraka a plugin's module.exports *is* the plugin instance, so `this`
// inside hooks/register === the exports object. register() calls
// exports.getRelays/getRoutes (which read this.config), so we attach config
// and logerror directly onto the required module, then build the table.
const relaysCfg = {
    local: { host: "127.0.0.1", port: 25 },
    partner: { host: "mx.partner.com", port: 25 },
};

function setup(routingCfg) {
    npRoute.logerror = () => {};
    npRoute.config = {
        get: (path) => (path === "relays.json" ? relaysCfg : routingCfg),
    };
    npRoute.register();
}

function makeHmail(rcpt_to, mail_from = { user: "s", host: "a.com" }) {
    return { todo: { mail_from, rcpt_to } };
}

test("npRoute does not blanket-enable relaying (npFilter owns the allowlist)", () => {
    // Regression guard: npRoute must not register a connect hook that sets
    // connection.relaying = true unconditionally — that would bypass the
    // npFilter IP allowlist and open the relay.
    assert.equal(typeof npRoute.hook_connect, "undefined");
});

test("hook_get_mx returns OK with the matched relay for routable mail", () => {
    setup([
        { relay: "partner", sender: "", sender_domain: "a.com", rcpt: "", rcpt_domain: "" },
        { relay: "local", sender: "", sender_domain: "", rcpt: "", rcpt_domain: "" },
    ]);

    let code, arg;
    npRoute.hook_get_mx(
        (c, a) => { code = c; arg = a; },
        makeHmail([{ user: "r", host: "x.com" }]),
        "x.com"
    );

    assert.equal(code, OK);
    assert.deepEqual(arg, relaysCfg.partner);
});

test("hook_get_mx falls through to a catch-all route", () => {
    setup([
        { relay: "partner", sender: "", sender_domain: "nomatch.com", rcpt: "", rcpt_domain: "" },
        { relay: "local", sender: "", sender_domain: "", rcpt: "", rcpt_domain: "" },
    ]);

    let code, arg;
    npRoute.hook_get_mx(
        (c, a) => { code = c; arg = a; },
        makeHmail([{ user: "r", host: "x.com" }]),
        "x.com"
    );

    assert.equal(code, OK);
    assert.deepEqual(arg, relaysCfg.local);
});

test("hook_get_mx defers with DENYSOFT when no route matches", () => {
    setup([
        { relay: "partner", sender: "", sender_domain: "nomatch.com", rcpt: "", rcpt_domain: "" },
    ]);

    let code, msg;
    npRoute.hook_get_mx(
        (c, m) => { code = c; msg = m; },
        makeHmail([{ user: "r", host: "x.com" }]),
        "x.com"
    );

    assert.equal(code, DENYSOFT);
    assert.equal(msg, "No route found");
});

test("hook_get_mx defers with DENYSOFT when rcpt_to is empty", () => {
    setup([{ relay: "local", sender: "", sender_domain: "", rcpt: "", rcpt_domain: "" }]);

    let code, msg;
    npRoute.hook_get_mx((c, m) => { code = c; msg = m; }, makeHmail([]), "x.com");

    assert.equal(code, DENYSOFT);
    assert.equal(msg, "No recipients");
});

test("hook_get_mx defers with DENYSOFT when rcpt_to is missing", () => {
    setup([{ relay: "local", sender: "", sender_domain: "", rcpt: "", rcpt_domain: "" }]);

    let code, msg;
    npRoute.hook_get_mx((c, m) => { code = c; msg = m; }, makeHmail(undefined), "x.com");

    assert.equal(code, DENYSOFT);
    assert.equal(msg, "No recipients");
});
