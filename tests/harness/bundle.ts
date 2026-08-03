/**
 * Building the configuration document a gateway is given.
 *
 * This mirrors `config.Bundle` (mailgw-go/internal/config) and, on the other
 * side, what `webui-fastify/src/central/bundle.ts` composes. Hand-written on
 * purpose: it is the same contract from a third place, so a change to the wire
 * shape that nobody updated here fails a suite instead of drifting quietly.
 *
 * `tests/stack/bundle-parity.test.ts` closes the loop by feeding a bundle the
 * CONSOLE produced to a gateway configured from this file.
 */

/** The bundle format version, matching config.BundleFormat. */
export const BUNDLE_FORMAT = 1;

export interface Relay {
    name: string;
    exchange: string;
    /** A number here; the console emits a number too, having read an INT column. */
    port: number;
    priority?: number;
    auth_user?: string;
    auth_pass?: string;
    tls?: "opportunistic" | "required" | "none";
    allow_insecure_auth?: boolean;
    use_mx?: boolean;
}

export interface Bundle {
    format: number;
    server?: string;
    routing?: string;
    allowlist?: { allowed: string[]; allow_all?: boolean };
    relays?: Record<string, Relay[]>;
    logging: {
        url_conn?: string;
        url_queue?: string;
        url_delivery?: string;
        api_key?: string;
    };
    admin?: { metrics_token?: string };
    auth?: { users: { user: string; hash: string }[] };
}

export interface BaselineOptions {
    /** Relay groups. Every Tier-B suite passes at least one fake sink here. */
    relays?: Record<string, Relay[]>;
    /** Where audit events go. Omit for a gateway whose events go nowhere. */
    logging?: Bundle["logging"];
    /** The routing YAML. Defaults to "relay everything to Outbound". */
    routing?: string;
    /** Extra YAML appended to the server profile, e.g. an `outbound:` block. */
    serverExtra?: string;
    /** The SMTP listen address. `:0` is the point of the whole harness. */
    listen?: string;
    hostname?: string;
    allowlist?: Bundle["allowlist"];
    admin?: Bundle["admin"];
    auth?: Bundle["auth"];
}

/**
 * The one restart-required configuration a Tier-B suite starts from.
 *
 * NOTE the deliberate divergence from tests/stack/baseline.ts: this does NOT
 * set `outbound.spool_dir`. tests/provision.ts pins /opt/mailgw-go/queue, which
 * is correct for the container and wrong for a host process — two gateways would
 * share it, and on a developer's machine it is usually unwritable, so the first
 * apply would fail inside queue.NewSpool before SMTP ever bound. Leaving it out
 * lets node.SpoolFallback answer with <dataDir>/queue, which is per-instance and
 * always writable.
 */
export function baseline(o: BaselineOptions = {}): Bundle {
    const server =
        `hostname: ${o.hostname ?? "gw.test"}\n` +
        "local_domains: [gw.test, ngm.dev]\n" +
        "listen:\n" +
        `    - addr: "${o.listen ?? "127.0.0.1:0"}"\n` +
        // Short, so stopping a gateway between tests costs milliseconds rather
        // than the 10s default. The container sets 30s for the opposite reason.
        "shutdown_timeout: 2s\n" +
        (o.serverExtra ?? "");

    const bundle: Bundle = {
        format: BUNDLE_FORMAT,
        server,
        routing: o.routing ?? relayEverythingTo("Outbound"),
        // allow_all because a host-spawned gateway is dialled from 127.0.0.1
        // and, under PROXY or a container, from addresses the test cannot
        // predict. Suites that are ABOUT the allowlist override this.
        allowlist: o.allowlist ?? { allowed: [], allow_all: true },
        relays: o.relays,
        // Always present, never omitted: a gateway with no logservice is a legal
        // configuration and the zero value is what expresses it.
        logging: o.logging ?? {},
    };
    if (o.admin) bundle.admin = o.admin;
    if (o.auth) bundle.auth = o.auth;
    return bundle;
}

/** The simplest useful ruleset: everything goes to one relay group. */
export function relayEverythingTo(group: string): string {
    return (
        "version: 1\n" +
        "routes:\n" +
        "    - name: Default\n" +
        "      match: {always: true}\n" +
        `      then: [{action: relay, relay: ${group}}]\n`
    );
}

/**
 * A ruleset from a list of rule bodies, each already indented as YAML.
 *
 * A template rather than a builder object: rules are the thing under test in
 * half these suites, and a test that reads as the YAML an operator would write
 * is worth more than one that reads as an API.
 */
export function ruleset(parts: { policy?: string[]; routes?: string[]; default?: string }): string {
    let out = "version: 1\n";
    if (parts.policy?.length) out += "policy:\n" + parts.policy.join("");
    if (parts.routes?.length) out += "routes:\n" + parts.routes.join("");
    if (parts.default) out += `default_action: ${parts.default}\n`;
    return out;
}

/** One relay group pointing at a host:port — usually a fake sink. */
export function relayGroup(name: string, host: string, port: number, extra: Partial<Relay> = {}) {
    return {
        [name]: [{ name: `${name}-1`, exchange: host, port, priority: 0, ...extra }],
    };
}
