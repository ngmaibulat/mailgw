import type { FastifyReply, FastifyRequest } from "fastify";
import { asc, desc, eq } from "drizzle-orm";

import {
    db,
    gateways,
    gatewayAssignments,
    configProfiles,
    configVersions,
    configDeployments,
    relayGroups,
} from "../../db/index.ts";
import { sessionEmail } from "../auth/session.ts";
import { deployBundle, listVersions, rollbackTo } from "../central/bundle.ts";
import { PROFILE_KINDS } from "../validation/config.ts";

type IdParams = { id: string };

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

        const data = rows.map((row) => ({
            ...row,
            desired_version: row.desired_version_id
                ? (versionById.get(row.desired_version_id) ?? null)
                : null,
            applied_version: row.applied_version_id
                ? (versionById.get(row.applied_version_id) ?? null)
                : null,
        }));

        return reply.view("gateways/index", { data });
    }

    async details(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [gateway] = await db
            .select()
            .from(gateways)
            .where(eq(gateways.id, id))
            .limit(1);

        if (!gateway) {
            return reply.redirect("/gateways");
        }

        const [assignments, profiles, groups, versions, history] =
            await Promise.all([
                db
                    .select()
                    .from(gatewayAssignments)
                    .where(eq(gatewayAssignments.gateway_id, id)),
                db
                    .select()
                    .from(configProfiles)
                    .orderBy(
                        asc(configProfiles.kind),
                        asc(configProfiles.name),
                    ),
                db.select().from(relayGroups).orderBy(asc(relayGroups.name)),
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

        const versionById = new Map(versions.map((v) => [v.id, v.version]));

        return reply.view("gateways/details", {
            data: gateway,
            desired_version: gateway.desired_version_id
                ? (versionById.get(gateway.desired_version_id) ?? null)
                : null,
            applied_version: gateway.applied_version_id
                ? (versionById.get(gateway.applied_version_id) ?? null)
                : null,
            kinds: PROFILE_KINDS,
            profiles,
            groups,
            selectedProfile,
            selectedGroups,
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
        const id = +(request.params as IdParams).id;
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

        return reply.redirect(`/gateways/${id}`);
    }

    async rename(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;
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
        const id = +(request.params as IdParams).id;
        const body = request.body as Record<string, unknown>;

        await db
            .delete(gatewayAssignments)
            .where(eq(gatewayAssignments.gateway_id, id));

        for (const kind of PROFILE_KINDS) {
            const raw = body[`profile_${kind}`];
            const profileId = Number(raw);
            if (Number.isInteger(profileId) && profileId > 0) {
                await db.insert(gatewayAssignments).values({
                    gateway_id: id,
                    kind,
                    profile_id: profileId,
                });
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
            await db.insert(gatewayAssignments).values({
                gateway_id: id,
                kind: "relaygroup",
                relay_group_id: groupId,
            });
        }

        return reply.redirect(`/gateways/${id}`);
    }

    // Freeze the current assignments as a new immutable version and point the
    // gateway at it. The gateway picks it up on its next status poll.
    async deploy(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;
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
        const id = +(request.params as IdParams).id;
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
        const id = +(request.params as IdParams).id;
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
        const id = +(request.params as IdParams).id;

        await db
            .delete(configDeployments)
            .where(eq(configDeployments.gateway_id, id));
        await db
            .delete(configVersions)
            .where(eq(configVersions.gateway_id, id));
        await db
            .delete(gatewayAssignments)
            .where(eq(gatewayAssignments.gateway_id, id));
        await db.delete(gateways).where(eq(gateways.id, id));

        return reply.redirect("/gateways");
    }
}
