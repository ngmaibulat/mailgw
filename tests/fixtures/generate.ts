/**
 * Regenerate tests/fixtures/dev-profiles.json.
 *
 * The fixture exists because M19's own verification block documents it:
 *
 *   curl -sf -XPOST localhost:9090/testctl/config/profiles -d @tests/fixtures/dev-profiles.json
 *
 * ...and the file was never written. It is GENERATED rather than hand-kept so
 * it cannot drift from the profile texts the suite and provision.ts actually
 * use: the day someone edits the baseline and forgets this, the curl recipe in
 * the milestone plan would start configuring a gateway differently from every
 * test.
 *
 *   bun tests/fixtures/generate.ts
 */

import { ALLOWLIST, RELAY_GROUP, RELAY_HOST, RELAY_PORT, RULESET, SERVER } from "../stack/baseline.ts";

// The POST /testctl/config/profiles shape: node.Profiles.
const profiles = {
    server: SERVER,
    routing: RULESET,
    allowlist: JSON.parse(ALLOWLIST),
    relays: {
        [RELAY_GROUP]: [
            { name: "sink", exchange: RELAY_HOST, port: Number(RELAY_PORT), priority: 0 },
        ],
    },
    logging: {
        url_conn: "http://dev-logservice:3000/api/connection",
        url_queue: "http://dev-logservice:3000/api/queue",
        url_delivery: "http://dev-logservice:3000/api/delivery",
    },
};

const out = new URL("./dev-profiles.json", import.meta.url).pathname;
await Bun.write(out, JSON.stringify(profiles, null, 4) + "\n");
console.log(`wrote ${out}`);
