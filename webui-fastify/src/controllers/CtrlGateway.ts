import type { FastifyReply, FastifyRequest } from "fastify";
import { asc, desc, eq, inArray } from "drizzle-orm";

import {
    db,
    gateways,
    gatewayAssignments,
    configProfiles,
    configVersions,
    configDeployments,
    relayGroups,
    credentialSets,
} from "../../db/index.ts";
import { sessionEmail } from "../auth/session.ts";
import { deployBundle, listVersions, rollbackTo } from "../central/bundle.ts";
import { isStale, metricsFor } from "../central/fleet.ts";
import { notifyGateway } from "../central/notify.ts";
import { PROFILE_KINDS } from "../validation/config.ts";

type IdParams = { id: string };

// `+id` on a non-numeric path segment yields NaN, which flows into eq() and
// silently matches nothing — a 302 back to the list, or worse, a delete that
// quietly does nothing. Reject it at the edge instead.
function gatewayId(request: FastifyRequest): number | null {
    const n = Number((request.params as IdParams).id);
    return Number.isInteger(n) && n > 0 ? n : null;
}

function badId(reply: FastifyReply) {
    return reply.code(400).view("util/error", {
        statusCode: 400,
        message: "That is not a valid gateway id.",
    });
}

// Statuses an operator can move a gateway to. "pending" is not here: it is the
// state registration creates, and nothing should be able to put a gateway back
// into the approval queue by hand.
const SETTABLE_STATUS = ["approved", "rejected", "revoked"] as const;

export class CtrlGateway {
    async index(_request: FastifyRequest, reply: FastifyReply) {
        const rows = await db
            .select()
            .from(gateways)
            .orderBy(asc(gateways.status), asc(gateways.name));

        // Resolve the version numbers behind desired/applied so the list can
        // show "v7 → v6" rather than two meaningless row ids.
        const versions = await db
            .select({
                id: configVersions.id,
                version: configVersions.version,
            })
            .from(configVersions);
        const versionById = new Map(versions.map((v) => [v.id, v.version]));

        const snapshots = await metricsFor(rows.map((r) => r.id));
        const now = Date.now();

        const data = rows.map((row) => ({
            ...row,
            desired_version: row.desired_version_id
                ? (versionById.get(row.desired_version_id) ?? null)
                : null,
            applied_version: row.applied_version_id
                ? (versionById.get(row.applied_version_id) ?? null)
                : null,
            // Only meaningful for a gateway that is meant to be reporting; a
            // rejected or revoked one is silent by design.
            stale: row.status === "approved" && isStale(row.last_seen, now),
            metrics: snapshots.get(row.id) ?? null,
        }));

        return reply.view("gateways/index", { data });
    }

    async details(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }

        const [gateway] = await db
            .select()
            .from(gateways)
            .where(eq(gateways.id, id))
            .limit(1);

        if (!gateway) {
            return reply.redirect("/gateways");
        }

        const [
            assignments,
            profiles,
            groups,
            credentialSetList,
            versions,
            history,
        ] = await Promise.all([
            db
                .select()
                .from(gatewayAssignments)
                .where(eq(gatewayAssignments.gateway_id, id)),
            db
                .select()
                .from(configProfiles)
                .orderBy(asc(configProfiles.kind), asc(configProfiles.name)),
            db.select().from(relayGroups).orderBy(asc(relayGroups.name)),
            db.select().from(credentialSets).orderBy(asc(credentialSets.name)),
            listVersions(id),
            db
                .select()
                .from(configDeployments)
                .where(eq(configDeployments.gateway_id, id))
                .orderBy(desc(configDeployments.id))
                .limit(20),
        ]);

        // kind -> selected profile id, for the assignment form's <select>s.
        const selectedProfile: Record<string, number | null> = {};
        for (const kind of PROFILE_KINDS) {
            selectedProfile[kind] =
                assignments.find((a) => a.kind === kind)?.profile_id ?? null;
        }
        const selectedGroups = new Set(
            assignments
                .filter((a) => a.kind === "relaygroup")
                .map((a) => a.relay_group_id),
        );
        // Single-valued, unlike relay groups: a gateway has one set of inbound
        // logins, and merging several would make "which set is this credential
        // in?" unanswerable from the gateway's side.
        const selectedCredentialSet =
            assignments.find((a) => a.kind === "credentialset")
                ?.credential_set_id ?? null;

        const versionById = new Map(versions.map((v) => [v.id, v.version]));

        const snapshots = await metricsFor([gateway.id]);

        return reply.view("gateways/details", {
            data: gateway,
            metrics: snapshots.get(gateway.id) ?? null,
            stale: gateway.status === "approved" && isStale(gateway.last_seen),
            desired_version: gateway.desired_version_id
                ? (versionById.get(gateway.desired_version_id) ?? null)
                : null,
            applied_version: gateway.applied_version_id
                ? (versionById.get(gateway.applied_version_id) ?? null)
                : null,
            kinds: PROFILE_KINDS,
            profiles,
            groups,
            credentialSetList,
            selectedProfile,
            selectedGroups,
            selectedCredentialSet,
            versions,
            history,
            historyVersion: (versionId: number) =>
                versionById.get(versionId) ?? versionId,
        });
    }

    // Approve / reject / revoke. Approval is the only thing standing between an
    // open registration endpoint and a stranger pulling relay credentials, so
    // this route is admin-only (see requireAdmin in src/routes/gateways.ts).
    async setStatus(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const status = (request.body as { status?: string }).status;

        if (!SETTABLE_STATUS.includes(status as never)) {
            return reply.code(400).view("util/error", {
                statusCode: 400,
                message: `Unknown gateway status ${JSON.stringify(status)}.`,
            });
        }

        const actor = (await sessionEmail(request)) ?? "unknown";
        const changes: Record<string, unknown> = { status };
        if (status === "approved") {
            changes.approved_by = actor;
            changes.approved_at = new Date();
        }

        await db.update(gateways).set(changes).where(eq(gateways.id, id));
        request.log.info(
            { gatewayId: id, status, actor },
            "gateway status set",
        );
        // A pending gateway learning it has just been approved is the single
        // most useful notification there is: it stops waiting and pulls.
        notifyGateway(id);

        return reply.redirect(`/gateways/${id}`);
    }

    async rename(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const name = String((request.body as { name?: string }).name ?? "")
            .trim()
            .slice(0, 255);

        if (name) {
            await db.update(gateways).set({ name }).where(eq(gateways.id, id));
        }
        return reply.redirect(`/gateways/${id}`);
    }

    // Replace the whole assignment set in one go. Simpler than diffing, and the
    // form always posts the complete picture.
    async saveAssignments(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const body = request.body as Record<string, unknown>;

        const rows: (typeof gatewayAssignments.$inferInsert)[] = [];
        const wantedProfiles: { kind: string; profileId: number }[] = [];

        for (const kind of PROFILE_KINDS) {
            const profileId = Number(body[`profile_${kind}`]);
            if (Number.isInteger(profileId) && profileId > 0) {
                wantedProfiles.push({ kind, profileId });
                rows.push({ gateway_id: id, kind, profile_id: profileId });
            }
        }

        // A single checkbox posts a string, several post an array.
        const rawGroups = body.relay_groups;
        const groupIds = (
            Array.isArray(rawGroups)
                ? rawGroups
                : rawGroups === undefined
                  ? []
                  : [rawGroups]
        )
            .map(Number)
            .filter((n) => Number.isInteger(n) && n > 0);

        for (const groupId of groupIds) {
            rows.push({
                gateway_id: id,
                kind: "relaygroup",
                relay_group_id: groupId,
            });
        }

        // Inbound SMTP AUTH credentials. One set or none — the form posts an
        // empty value for "none", which is how a set is unassigned.
        const credentialSetId = Number(body.credential_set_id);
        if (Number.isInteger(credentialSetId) && credentialSetId > 0) {
            rows.push({
                gateway_id: id,
                kind: "credentialset",
                credential_set_id: credentialSetId,
            });
        }

        // One transaction: the delete and the re-insert are only meaningful
        // together. A failure between them used to leave the gateway with NO
        // assignments, and the next Deploy would freeze that emptiness as a
        // real, immutable, rollback-able version.
        try {
            await db.transaction(async (tx) => {
                // Validate the profile kind against the slot it was assigned
                // to. Any integer was previously accepted for any slot, so a
                // `ruleset` profile could be assigned to the `server` slot and
                // its YAML would land in the bundle as server.yaml. The gateway
                // fails closed on that, so it is a footgun rather than a
                // breach — but the console should not be able to compose it.
                if (wantedProfiles.length) {
                    const found = await tx
                        .select({
                            id: configProfiles.id,
                            kind: configProfiles.kind,
                        })
                        .from(configProfiles)
                        .where(
                            inArray(
                                configProfiles.id,
                                wantedProfiles.map((p) => p.profileId),
                            ),
                        );
                    const kindById = new Map(found.map((p) => [p.id, p.kind]));

                    for (const want of wantedProfiles) {
                        const actual = kindById.get(want.profileId);
                        if (actual === undefined) {
                            throw new Error(
                                `no such configuration profile: ${want.profileId}`,
                            );
                        }
                        if (actual !== want.kind) {
                            throw new Error(
                                `profile ${want.profileId} is a "${actual}" profile ` +
                                    `and cannot be assigned to the "${want.kind}" slot`,
                            );
                        }
                    }
                }

                await tx
                    .delete(gatewayAssignments)
                    .where(eq(gatewayAssignments.gateway_id, id));

                if (rows.length) {
                    // One multi-row insert. `timestamps` uses $defaultFn, which
                    // drizzle evaluates per row, so created_at/updated_at are
                    // still filled in.
                    await tx.insert(gatewayAssignments).values(rows);
                }
            });
        } catch (err) {
            request.log.error(err);
            return reply.code(400).view("util/error", {
                statusCode: 400,
                message: `Could not save assignments: ${(err as Error).message}`,
            });
        }

        return reply.redirect(`/gateways/${id}`);
    }

    // Freeze the current assignments as a new immutable version and point the
    // gateway at it. The gateway picks it up on its next status poll.
    async deploy(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const note = String(
            (request.body as { note?: string }).note ?? "",
        ).slice(0, 255);
        const actor = (await sessionEmail(request)) ?? "unknown";

        try {
            const result = await deployBundle(id, actor, note || undefined);
            request.log.info(
                { gatewayId: id, ...result, actor },
                "configuration deployed",
            );
        } catch (err) {
            request.log.error(err);
            return reply.code(400).view("util/error", {
                statusCode: 400,
                message: `Could not deploy: ${(err as Error).message}`,
            });
        }

        return reply.redirect(`/gateways/${id}`);
    }

    async rollback(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const versionId = Number(
            (request.body as { version_id?: string }).version_id,
        );
        const actor = (await sessionEmail(request)) ?? "unknown";

        try {
            const result = await rollbackTo(id, versionId, actor);
            request.log.info(
                { gatewayId: id, ...result, actor },
                "configuration rolled back",
            );
        } catch (err) {
            request.log.error(err);
            return reply.code(400).view("util/error", {
                statusCode: 400,
                message: `Could not roll back: ${(err as Error).message}`,
            });
        }

        return reply.redirect(`/gateways/${id}`);
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }
        const [data] = await db
            .select()
            .from(gateways)
            .where(eq(gateways.id, id))
            .limit(1);

        if (!data) {
            return reply.redirect("/gateways");
        }
        return reply.view("gateways/delete", { data });
    }

    // Forgetting a gateway also drops its versions and assignments. The gateway
    // itself keeps running its last-good config and, if it is still alive, will
    // re-register as `pending` on its next boot.
    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = gatewayId(request);
        if (id === null) {
            return badId(reply);
        }

        // Four tables, one transaction: a failure part-way would otherwise
        // leave orphaned versions and assignments pointing at a gateway row
        // that may or may not still exist.
        await db.transaction(async (tx) => {
            await tx
                .delete(configDeployments)
                .where(eq(configDeployments.gateway_id, id));
            await tx
                .delete(configVersions)
                .where(eq(configVersions.gateway_id, id));
            await tx
                .delete(gatewayAssignments)
                .where(eq(gatewayAssignments.gateway_id, id));
            await tx.delete(gateways).where(eq(gateways.id, id));
        });

        return reply.redirect("/gateways");
    }
}
