const { test } = require("node:test");
const assert = require("node:assert/strict");

const Route = require("../plugins/Route");
const RoutingTable = require("../plugins/RoutingTable");

// relays is a name -> relay-object map; routes is an ordered array of Route.

const relays = {
    relayA: { host: "a.example", port: 25 },
    relayB: { host: "b.example", port: 587 },
};

test("findRoute returns the relay object for the first matching route", () => {
    const routes = [new Route("relayA", "", "a.com", "", "")];
    const rt = new RoutingTable(relays, routes);

    assert.deepEqual(rt.findRoute("s@a.com", "r@b.com"), relays.relayA);
});

test("findRoute returns false when no route matches", () => {
    const routes = [new Route("relayA", "", "a.com", "", "")];
    const rt = new RoutingTable(relays, routes);

    assert.equal(rt.findRoute("s@other.com", "r@b.com"), false);
});

test("findRoute is first-match-wins in route order", () => {
    const routes = [
        new Route("relayA", "", "a.com", "", ""), // matches first
        new Route("relayB", "", "a.com", "", ""), // also matches, but later
    ];
    const rt = new RoutingTable(relays, routes);

    assert.deepEqual(rt.findRoute("s@a.com", "r@x.com"), relays.relayA);
});

test("findRoute returns false when the matched route names an unknown relay", () => {
    const routes = [new Route("ghostRelay", "", "a.com", "", "")];
    const rt = new RoutingTable(relays, routes);

    assert.equal(rt.findRoute("s@a.com", "r@b.com"), false);
});

test("an empty routing table matches nothing", () => {
    const rt = new RoutingTable(relays, []);
    assert.equal(rt.findRoute("s@a.com", "r@b.com"), false);
});
