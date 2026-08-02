import crypto from "node:crypto";

import { and, desc, eq, inArray } from "drizzle-orm";

import {
    db,
    gateways,
    gatewayAssignments,
    configProfiles,
    configVersions,
    configDeployments,
    relayGroups,
    relays,
    smtpCredentials,
} from "../../db/index.ts";
import type { DB } from "../../db/index.ts";
import { notifyGateway } from "./notify.ts";
import { decrypt } from "./secrets.ts";

// Composes a gateway's assigned profiles and relay groups into the one JSON
// document it pulls from GET /agent/config.
//
// The keys mirror the config-directory files mailgw-go already reads, so the
// same `check` / `explain` code paths and the whole Go test suite keep working:
//
//   server    -> server.yaml   (text, verbatim)
//   routing   -> routing.yaml  (text, verbatim — the rule DSL)
//   allowlist -> ngmfilter.json
//   relays    -> relays.json
//   logging   -> logging.json
//   admin     -> admin.json
//
// The rule DSL is compiled and type-checked by Go. Re-implementing that here
// would give two sources of truth that can disagree, so the gateway is
// authoritative: it validates on pull, keeps its last-good configuration on
// failure, and reports the reason back through POST /agent/report.

export interface BundleRelay {
    name: string;
    exchange: string;
    port: number;
    priority: number;
    auth_user?: string;
    auth_pass?: string;
    // Name of an environment variable on the gateway holding the password.
    // Keeps the credential out of the bundle entirely; mailgw-go prefers it
    // over auth_pass when both are present.
    auth_pass_env?: string;
    // none | opportunistic | required. Omitted when unset, where mailgw-go
    // defaults to opportunistic.
    tls?: string;
    allow_insecure_auth?: boolean;
    // Resolve `exchange` as a domain and deliver to its MX hosts in preference
    // order, rather than dialling it directly. Omitted when false so an
    // unchanged configuration keeps hashing identically.
    use_mx?: boolean;
}

export interface GatewayBundle {
    // Bumped when the bundle format itself changes, not per deploy. The deploy
    // counter is ConfigVersions.version.
    format: 1;
    server: string | null;
    routing: string | null;
    // allow_all is carried through rather than dropped: mailgw-go's allowlist
    // is fail-closed and rejects an empty list unless it is set, so without
    // this there is no way to express an allow-all gateway from the console at
    // all — and the gateway's own error message would advise an option the UI
    // cannot produce.
    allowlist: { allowed: string[]; allow_all?: boolean };
    relays: Record<string, BundleRelay[]>;
    logging: {
        url_conn: string;
        url_queue: string;
        url_delivery: string;
        // The logservice credential. A managed gateway has no environment to
        // read it from, so it has to arrive here or events cannot be posted at
        // all.
        api_key?: string;
    };
    // The gateway's local admin listener. Omitted entirely when there is
    // nothing to say, so every existing bundle keeps its digest.
    admin?: {
        // Bearer token for the gateway's /metrics and /readyz. A scraper needs
        // a credential it can present without a browser, which is why this is
        // not the session the gateway's own wizard runs on. Empty means those
        // endpoints stay open, which is what every install that firewalled port
        // 8080 already has.
        metrics_token?: string;
    };
    // Inbound SMTP AUTH credentials, as bcrypt hashes. Omitted entirely when
    // the gateway has no credential set assigned, so every bundle composed
    // before this key existed keeps its digest and nothing in the fleet
    // re-pulls.
    //
    // Hashes, not passwords, and therefore nothing to do with secrets.ts: the
    // gateway only ever VERIFIES an inbound credential, so unlike a relay
    // password it never needs the plaintext back and a leaked bundle costs an
    // offline bcrypt attack rather than a working login.
    auth?: {
        users: { user: string; hash: string }[];
    };
}

// Where gateways should post audit events. This is infrastructure rather than
// per-gateway policy, so it comes from the environment rather than a profile.
// It is NOT the same value as LOGSERVICE_URL: that one is the webui's own
// (often container-internal) route to logservice, which a remote gateway may
// not be able to resolve.
function loggingEndpoints(): GatewayBundle["logging"] {
    const base = (
        process.env.GATEWAY_LOGSERVICE_URL ||
        process.env.LOGSERVICE_URL ||
        "http://localhost:3000"
    ).replace(/\/+$/, "");

    const logging: GatewayBundle["logging"] = {
        url_conn: `${base}/api/connection`,
        url_queue: `${base}/api/queue`,
        url_delivery: `${base}/api/delivery`,
    };

    // Same split as the URL above, for the same reason: the key the webui uses
    // for its own read proxy is not necessarily the one the fleet should send.
    // Omitted rather than empty when unset, so an unchanged configuration keeps
    // hashing identically.
    const apiKey =
        process.env.GATEWAY_LOGSERVICE_API_KEY ||
        process.env.LOGSERVICE_API_KEY;
    if (apiKey) {
        logging.api_key = apiKey;
    }
    return logging;
}

// The gateway admin listener's settings. Infrastructure rather than per-gateway
// policy, on the same reasoning as loggingEndpoints above: a fleet is scraped by
// one Prometheus with one credential.
//
// Returns undefined rather than an empty object when there is nothing to say —
// stableStringify drops undefined, so an install that never sets a token keeps
// every existing bundle digest and no gateway re-pulls.
function adminSettings(): GatewayBundle["admin"] {
    const token = process.env.GATEWAY_METRICS_TOKEN;
    return token ? { metrics_token: token } : undefined;
}

// A gateway with no allowlist profile gets an empty list, and mailgw-go's
// allowlist is fail-closed — it will refuse every peer rather than relay for
// anyone. That is the correct default for a mail gateway; the UI warns before
// deploying one.
const EMPTY_ALLOWLIST = { allowed: [] as string[] };

function parseAllowlist(body: string): GatewayBundle["allowlist"] {
    const parsed = JSON.parse(body) as unknown;
    if (
        !parsed ||
        typeof parsed !== "object" ||
        !Array.isArray((parsed as { allowed?: unknown }).allowed)
    ) {
        throw new Error('allowlist profile must be {"allowed": [...]}');
    }

    const value = parsed as { allowed: string[]; allow_all?: unknown };
    const out: GatewayBundle["allowlist"] = { allowed: value.allowed };
    // Only emitted when the operator actually set it, so an unchanged profile
    // keeps its digest.
    if (value.allow_all === true) {
        out.allow_all = true;
    }
    return out;
}

export async function composeBundle(
    gatewayId: number,
    conn: DB = db,
): Promise<GatewayBundle> {
    const assignments = await conn
        .select()
        .from(gatewayAssignments)
        .where(eq(gatewayAssignments.gateway_id, gatewayId));

    const profileIds = assignments
        .map((a) => a.profile_id)
        .filter((id): id is number => id !== null);

    const profiles = profileIds.length
        ? await conn
              .select()
              .from(configProfiles)
              .where(inArray(configProfiles.id, profileIds))
        : [];
    const profileById = new Map(profiles.map((p) => [p.id, p]));

    const bodyOfKind = (kind: string): string | null => {
        const assignment = assignments.find(
            (a) => a.kind === kind && a.profile_id !== null,
        );
        if (!assignment?.profile_id) {
            return null;
        }
        return profileById.get(assignment.profile_id)?.body ?? null;
    };

    // --- relays: group name is the top-level key, value is an array --------
    const groupIds = assignments
        .filter((a) => a.kind === "relaygroup")
        .map((a) => a.relay_group_id)
        .filter((id): id is number => id !== null);

    const relayTable: Record<string, BundleRelay[]> = {};
    if (groupIds.length) {
        const groups = await conn
            .select()
            .from(relayGroups)
            .where(inArray(relayGroups.id, groupIds));
        const members = await conn
            .select()
            .from(relays)
            .where(inArray(relays.group_id, groupIds));

        for (const group of groups) {
            if (!group.name) {
                continue;
            }
            relayTable[group.name] = members
                .filter((m) => m.group_id === group.id)
                // Failover order. mailgw-go sorts by (priority asc, config
                // order) too, but emitting them sorted keeps the bundle stable
                // so an unchanged config hashes identically.
                .sort((a, b) => (a.priority ?? 0) - (b.priority ?? 0))
                .map((m) => {
                    const relay: BundleRelay = {
                        name: m.name ?? `relay-${m.id}`,
                        // The DB column is `host`; relays.json calls it
                        // `exchange`.
                        exchange: m.host ?? "",
                        // INT in the DB, string in the legacy file. mailgw-go's
                        // Port type accepts either; a number is the honest one.
                        port: m.port ?? 25,
                        priority: m.priority ?? 0,
                    };
                    if (m.auth_user) {
                        relay.auth_user = m.auth_user;
                    }
                    // Decrypted here, on the console, so no key ever has to
                    // reach a gateway. See src/central/secrets.ts.
                    if (m.auth_pass) {
                        relay.auth_pass = decrypt(m.auth_pass);
                    }
                    if (m.auth_pass_env) {
                        relay.auth_pass_env = m.auth_pass_env;
                    }
                    if (m.tls) {
                        relay.tls = m.tls;
                    }
                    if (m.use_mx) {
                        relay.use_mx = true;
                    }
                    if (m.allow_insecure_auth) {
                        relay.allow_insecure_auth = true;
                    }
                    return relay;
                });
        }
    }

    const allowlistBody = bodyOfKind("allowlist");

    return {
        format: 1,
        server: bodyOfKind("server"),
        routing: bodyOfKind("ruleset"),
        allowlist: allowlistBody
            ? parseAllowlist(allowlistBody)
            : EMPTY_ALLOWLIST,
        relays: relayTable,
        logging: loggingEndpoints(),
        admin: adminSettings(),
        auth: await inboundAuth(assignments, conn),
    };
}

// The inbound SMTP AUTH credentials assigned to this gateway.
//
// Returns undefined — never {} and never {users: []} — when there is no set or
// the set is empty, on the same rule adminSettings() states: stableStringify
// drops undefined, so an install that never issues a credential keeps every
// existing bundle digest.
//
// Sorted by username because stableStringify deliberately does not sort arrays,
// so without this the digest would follow whatever order MySQL happened to
// return the rows in and an untouched configuration could churn.
async function inboundAuth(
    assignments: { kind: string; credential_set_id: number | null }[],
    conn: DB,
): Promise<GatewayBundle["auth"]> {
    const setId = assignments.find(
        (a) => a.kind === "credentialset" && a.credential_set_id !== null,
    )?.credential_set_id;
    if (!setId) {
        return undefined;
    }

    const rows = await conn
        .select()
        .from(smtpCredentials)
        .where(eq(smtpCredentials.set_id, setId));
    if (!rows.length) {
        return undefined;
    }

    return {
        users: rows
            .map((r) => ({ user: r.username, hash: r.hash }))
            .sort((a, b) => (a.user < b.user ? -1 : a.user > b.user ? 1 : 0)),
    };
}

// Stable stringify: key order must not change between two composes of the same
// configuration, or the digest changes and every gateway re-pulls for nothing.
// Object keys are emitted sorted at every depth — relay-group names in
// particular come back in whatever order the database felt like.
//
// Note this cannot be `JSON.stringify(v, keys)`: a replacer *array* filters keys
// at every nesting level, so a top-level key list would silently strip
// `url_conn`, `exchange` and the rest out of the bundle.
function stableStringify(value: unknown): string {
    if (value === null || typeof value !== "object") {
        return JSON.stringify(value) ?? "null";
    }
    if (Array.isArray(value)) {
        return `[${value.map(stableStringify).join(",")}]`;
    }
    const entries = Object.entries(value as Record<string, unknown>)
        .filter(([, v]) => v !== undefined)
        .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
        .map(([k, v]) => `${JSON.stringify(k)}:${stableStringify(v)}`);
    return `{${entries.join(",")}}`;
}

// Exported for bundle.test.ts, which pins the one property this whole file is
// built around: an unchanged configuration must serialise identically.
export function serialiseBundle(bundle: GatewayBundle): string {
    return stableStringify(bundle);
}

export function bundleDigest(serialised: string): string {
    return crypto.createHash("sha256").update(serialised).digest("hex");
}

export interface DeployResult {
    versionId: number;
    version: number;
    sha256: string;
    unchanged: boolean;
}

// Deploy: compose, freeze as a new immutable version, and point the gateway at
// it. Never updates an existing ConfigVersions row — that immutability is what
// makes rollback exact rather than approximate.
// The whole thing runs in one transaction: composing reads the assignments,
// and the writes that follow (version row, gateway pointer, audit row) are only
// meaningful together. A failure part-way used to be able to leave a gateway
// pointing at a version with no deployment record, or none at all.
export async function deployBundle(
    gatewayId: number,
    actor: string,
    note?: string,
): Promise<DeployResult> {
    const result = await deployInTransaction(gatewayId, actor, note);
    // After the commit, never inside it: a gateway woken by a notification
    // fetches immediately, and a notification sent from inside the transaction
    // could arrive before the row it describes is visible.
    notifyGateway(gatewayId);
    return result;
}

async function deployInTransaction(
    gatewayId: number,
    actor: string,
    note?: string,
): Promise<DeployResult> {
    return await db.transaction(async (tx) => {
        const bundle = await composeBundle(gatewayId, tx);
        const serialised = serialiseBundle(bundle);
        const sha256 = bundleDigest(serialised);

        const [latest] = await tx
            .select({
                id: configVersions.id,
                version: configVersions.version,
                bundle_sha256: configVersions.bundle_sha256,
            })
            .from(configVersions)
            .where(eq(configVersions.gateway_id, gatewayId))
            .orderBy(desc(configVersions.version))
            .limit(1);

        // Deploying an unchanged configuration would pile up identical versions
        // and make the history unreadable. Re-point at the existing one instead.
        if (latest && latest.bundle_sha256 === sha256) {
            await tx
                .update(gateways)
                .set({ desired_version_id: latest.id })
                .where(eq(gateways.id, gatewayId));

            return {
                versionId: latest.id,
                version: latest.version,
                sha256,
                unchanged: true,
            };
        }

        const version = (latest?.version ?? 0) + 1;
        await tx.insert(configVersions).values({
            gateway_id: gatewayId,
            version,
            bundle: serialised,
            bundle_sha256: sha256,
            note: note ?? null,
            created_by: actor,
        });

        // Inside the transaction on purpose: the uniq_version_gateway index
        // (migration 019) already stops concurrent deploys from corrupting the
        // numbering, so this read-back is about seeing our own write, not the
        // race.
        const [created] = await tx
            .select({ id: configVersions.id })
            .from(configVersions)
            .where(
                and(
                    eq(configVersions.gateway_id, gatewayId),
                    eq(configVersions.version, version),
                ),
            )
            .limit(1);

        if (!created) {
            throw new Error(
                "config version disappeared immediately after insert",
            );
        }

        await tx
            .update(gateways)
            .set({
                desired_version_id: created.id,
                // The previous error described the previous bundle.
                apply_error: null,
            })
            .where(eq(gateways.id, gatewayId));

        await tx.insert(configDeployments).values({
            gateway_id: gatewayId,
            version_id: created.id,
            action: "deploy",
            actor,
            note: note ?? null,
        });

        return { versionId: created.id, version, sha256, unchanged: false };
    });
}

// Rollback repoints at an existing version rather than minting a new one, so
// what runs afterwards is byte-identical to what ran before.
export async function rollbackTo(
    gatewayId: number,
    versionId: number,
    actor: string,
): Promise<{ version: number }> {
    const result = await rollbackInTransaction(gatewayId, versionId, actor);
    notifyGateway(gatewayId);
    return result;
}

async function rollbackInTransaction(
    gatewayId: number,
    versionId: number,
    actor: string,
): Promise<{ version: number }> {
    return await db.transaction(async (tx) => {
        const [target] = await tx
            .select({ id: configVersions.id, version: configVersions.version })
            .from(configVersions)
            .where(
                and(
                    eq(configVersions.id, versionId),
                    // Scoped to the gateway so a crafted id cannot pull another
                    // gateway's bundle — which would leak its relay credentials.
                    eq(configVersions.gateway_id, gatewayId),
                ),
            )
            .limit(1);

        // Throwing inside the callback aborts the transaction, which is exactly
        // what should happen: nothing has been written yet, and nothing should be.
        if (!target) {
            throw new Error("no such configuration version for this gateway");
        }

        await tx
            .update(gateways)
            .set({ desired_version_id: target.id, apply_error: null })
            .where(eq(gateways.id, gatewayId));

        await tx.insert(configDeployments).values({
            gateway_id: gatewayId,
            version_id: target.id,
            action: "rollback",
            actor,
        });

        return { version: target.version };
    });
}

export async function listVersions(gatewayId: number) {
    return await db
        .select({
            id: configVersions.id,
            version: configVersions.version,
            bundle_sha256: configVersions.bundle_sha256,
            note: configVersions.note,
            created_by: configVersions.created_by,
            createdAt: configVersions.createdAt,
        })
        .from(configVersions)
        .where(eq(configVersions.gateway_id, gatewayId))
        .orderBy(desc(configVersions.version));
}
