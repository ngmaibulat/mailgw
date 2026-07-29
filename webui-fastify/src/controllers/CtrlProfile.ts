import type { FastifyReply, FastifyRequest } from "fastify";
import { asc, eq } from "drizzle-orm";

import { db, configProfiles } from "../../db/index.ts";
import {
    parseProfileBody,
    PROFILE_KINDS,
    zodErr,
} from "../validation/config.ts";

type IdParams = { id: string };

// CRUD for the reusable config blocks assigned to gateways. Same shape as
// CtrlRelayGroup: GET renders a form, POST validates then writes then redirects,
// and a validation failure re-renders the same template with the error.

const BLANK = {
    kind: "ruleset",
    name: "",
    description: "",
    body: "",
};

function errorText(error: unknown): string {
    // parseProfileBody returns either a zod failure or a plain-string message
    // for the JSON-shape checks it does itself.
    if (typeof error === "string") {
        return error;
    }
    return zodErr(error as Parameters<typeof zodErr>[0]);
}

export class CtrlProfile {
    async index(_request: FastifyRequest, reply: FastifyReply) {
        const data = await db
            .select()
            .from(configProfiles)
            .orderBy(asc(configProfiles.kind), asc(configProfiles.name));

        return reply.view("config/profile-index", { data });
    }

    async create(request: FastifyRequest, reply: FastifyReply) {
        const kind = (request.query as { kind?: string }).kind;
        return reply.view("config/profile-form", {
            action: "Create",
            kinds: PROFILE_KINDS,
            data: {
                ...BLANK,
                kind: PROFILE_KINDS.includes(kind as never) ? kind : BLANK.kind,
            },
        });
    }

    async createHandle(request: FastifyRequest, reply: FastifyReply) {
        const parsed = parseProfileBody(
            request.body as Record<string, unknown>,
        );
        if (!parsed.success) {
            return reply.code(400).view("config/profile-form", {
                action: "Create",
                kinds: PROFILE_KINDS,
                data: request.body,
                error: errorText(parsed.error),
            });
        }

        await db.insert(configProfiles).values(parsed.data);
        return reply.redirect("/config/profiles");
    }

    async edit(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(configProfiles)
            .where(eq(configProfiles.id, id))
            .limit(1);

        if (!data) {
            return reply.redirect("/config/profiles");
        }

        return reply.view("config/profile-form", {
            action: "Update",
            kinds: PROFILE_KINDS,
            data,
        });
    }

    async editHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const parsed = parseProfileBody(
            request.body as Record<string, unknown>,
        );
        if (!parsed.success) {
            return reply.code(400).view("config/profile-form", {
                action: "Update",
                kinds: PROFILE_KINDS,
                data: { ...(request.body as Record<string, unknown>), id },
                error: errorText(parsed.error),
            });
        }

        // Editing a profile does NOT change what any gateway is running — the
        // deployed bundle is an immutable snapshot. It takes effect on the next
        // Deploy, which is the point of having versions at all.
        await db
            .update(configProfiles)
            .set(parsed.data)
            .where(eq(configProfiles.id, id));

        return reply.redirect("/config/profiles");
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(configProfiles)
            .where(eq(configProfiles.id, id))
            .limit(1);

        if (!data) {
            return reply.redirect("/config/profiles");
        }

        return reply.view("config/profile-delete", { data });
    }

    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        // Assignments referencing this profile are left behind deliberately:
        // composeBundle resolves a missing profile to "not assigned" rather
        // than failing, so deleting a profile degrades a gateway's next deploy
        // instead of breaking the console.
        await db.delete(configProfiles).where(eq(configProfiles.id, id));
        return reply.redirect("/config/profiles");
    }
}
