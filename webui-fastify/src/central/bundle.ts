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
} from "../../db/index.ts";

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
}

export interface GatewayBundle {
    // Bumped when the bundle format itself changes, not per deploy. The deploy
    // counter is ConfigVersions.version.
    format: 1;
    server: string | null;
    routing: string | null;
    allowlist: { allowed: string[] };
    relays: Record<string, BundleRelay[]>;
    logging: {
        url_conn: string;
        url_queue: string;
        url_delivery: string;
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

    return {
        url_conn: `${base}/api/connection`,
        url_queue: `${base}/api/queue`,
        url_delivery: `${base}/api/delivery`,
    };
}

// A gateway with no allowlist profile gets an empty list, and mailgw-go's
// allowlist is fail-closed — it will refuse every peer rather than relay for
// anyone. That is the correct default for a mail gateway; the UI warns before
// deploying one.
const EMPTY_ALLOWLIST = { allowed: [] as string[] };

function parseAllowlist(body: string): { allowed: string[] } {
    const parsed = JSON.parse(body) as unknown;
    if (
        !parsed ||
        typeof parsed !== "object" ||
        !Array.isArray((parsed as { allowed?: unknown }).allowed)
    ) {
        throw new Error('allowlist profile must be {"allowed": [...]}');
    }
    return { allowed: (parsed as { allowed: string[] }).allowed };
}

export async function composeBundle(gatewayId: number): Promise<GatewayBundle> {
    const assignments = await db
        .select()
        .from(gatewayAssignments)
        .where(eq(gatewayAssignments.gateway_id, gatewayId));

    const profileIds = assignments
        .map((a) => a.profile_id)
        .filter((id): id is number => id !== null);

    const profiles = profileIds.length
        ? await db
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
        const groups = await db
            .select()
            .from(relayGroups)
            .where(inArray(relayGroups.id, groupIds));
        const members = await db
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
                    if (m.auth_pass) {
                        relay.auth_pass = m.auth_pass;
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

function serialiseBundle(bundle: GatewayBundle): string {
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
export async function deployBundle(
    gatewayId: number,
    actor: string,
    note?: string,
): Promise<DeployResult> {
    const bundle = await composeBundle(gatewayId);
    const serialised = serialiseBundle(bundle);
    const sha256 = bundleDigest(serialised);

    const [latest] = await db
        .select({
            id: configVersions.id,
            version: configVersions.version,
            bundle_sha256: configVersions.bundle_sha256,
        })
        .from(configVersions)
        .where(eq(configVersions.gateway_id, gatewayId))
        .orderBy(desc(configVersions.version))
        .limit(1);

    // Deploying an unchanged configuration would pile up identical versions and
    // make the history unreadable. Re-point at the existing one instead.
    if (latest && latest.bundle_sha256 === sha256) {
        await db
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
    await db.insert(configVersions).values({
        gateway_id: gatewayId,
        version,
        bundle: serialised,
        bundle_sha256: sha256,
        note: note ?? null,
        created_by: actor,
    });

    const [created] = await db
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
        throw new Error("config version disappeared immediately after insert");
    }

    await db
        .update(gateways)
        .set({
            desired_version_id: created.id,
            // The previous error described the previous bundle.
            apply_error: null,
        })
        .where(eq(gateways.id, gatewayId));

    await db.insert(configDeployments).values({
        gateway_id: gatewayId,
        version_id: created.id,
        action: "deploy",
        actor,
        note: note ?? null,
    });

    return { versionId: created.id, version, sha256, unchanged: false };
}

// Rollback repoints at an existing version rather than minting a new one, so
// what runs afterwards is byte-identical to what ran before.
export async function rollbackTo(
    gatewayId: number,
    versionId: number,
    actor: string,
): Promise<{ version: number }> {
    const [target] = await db
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

    if (!target) {
        throw new Error("no such configuration version for this gateway");
    }

    await db
        .update(gateways)
        .set({ desired_version_id: target.id, apply_error: null })
        .where(eq(gateways.id, gatewayId));

    await db.insert(configDeployments).values({
        gateway_id: gatewayId,
        version_id: target.id,
        action: "rollback",
        actor,
    });

    return { version: target.version };
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
