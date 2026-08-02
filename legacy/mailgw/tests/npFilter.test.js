const { test } = require("node:test");
const assert = require("node:assert/strict");

// Haraka exposes its hook return codes (OK, DENYDISCONNECT, CONT, ...) as
// globals. Outside the Haraka runtime we register them ourselves the same way
// Haraka does, then load the plugin.
require("haraka-constants").import(global);

const npFilter = require("../plugins/npFilter");

// Minimal Haraka plugin context: the plugin only uses this.config.get and
// this.logerror in hook_connect.
function makeContext(cfg) {
    return {
        config: { get: () => cfg },
        logerror: () => {},
    };
}

function makeConnection(ip) {
    return {
        uuid: "test-uuid",
        remote: { ip },
        relaying: false,
    };
}

test("hook_connect allows a listed IP and enables relaying", () => {
    const ctx = makeContext({ allowed: ["1.2.3.4"] });
    const connection = makeConnection("1.2.3.4");

    let code;
    npFilter.hook_connect.call(ctx, (c) => (code = c), connection);

    assert.equal(code, OK);
    assert.equal(connection.relaying, true);
});

test("hook_connect denies an unlisted IP without enabling relaying", () => {
    const ctx = makeContext({ allowed: ["9.9.9.9"] });
    const connection = makeConnection("1.2.3.4");

    let code;
    npFilter.hook_connect.call(ctx, (c) => (code = c), connection);

    assert.equal(code, DENYDISCONNECT);
    assert.equal(connection.relaying, false);
});

test("hook_connect fails closed when config is missing", () => {
    const ctx = makeContext(null);
    const connection = makeConnection("1.2.3.4");

    let code;
    npFilter.hook_connect.call(ctx, (c) => (code = c), connection);

    assert.equal(code, DENYDISCONNECT);
    assert.equal(connection.relaying, false);
});

test("hook_connect fails closed when 'allowed' is not an array", () => {
    const ctx = makeContext({ allowed: "1.2.3.4" });
    const connection = makeConnection("1.2.3.4");

    let code;
    npFilter.hook_connect.call(ctx, (c) => (code = c), connection);

    assert.equal(code, DENYDISCONNECT);
    assert.equal(connection.relaying, false);
});
