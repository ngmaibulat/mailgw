import type { FastifyReply, FastifyRequest } from "fastify";
import { eq } from "drizzle-orm";

import { db, relays, relayGroups } from "../../db/index.ts";
import { relayGroupInsert, zodErr } from "../validation/config.ts";

type IdParams = { id: string };

export class CtrlRelayGroup {
    async create(request: FastifyRequest, reply: FastifyReply) {
        return reply.view("routing/relaygrp-form", {
            action: "Create",
            data: {
                name: "",
                description: "",
            },
        });
    }

    async createHandle(request: FastifyRequest, reply: FastifyReply) {
        const parsed = relayGroupInsert.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("routing/relaygrp-form", {
                action: "Create",
                data: request.body,
                error: zodErr(parsed.error),
            });
        }

        await db.insert(relayGroups).values(parsed.data);
        return reply.redirect("/config/relaygrp");
    }

    async edit(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(relayGroups)
            .where(eq(relayGroups.id, id))
            .limit(1);

        return reply.view("routing/relaygrp-form", {
            action: "Update",
            data: data,
        });
    }

    async editHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const parsed = relayGroupInsert.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("routing/relaygrp-form", {
                action: "Update",
                data: { ...(request.body as Record<string, unknown>), id },
                error: zodErr(parsed.error),
            });
        }

        await db.update(relayGroups).set(parsed.data).where(eq(relayGroups.id, id));
        return reply.redirect("/config/relaygrp");
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const [data] = await db
            .select()
            .from(relayGroups)
            .where(eq(relayGroups.id, id))
            .limit(1);

        return reply.view("routing/relaygrp-delete", { data: data });
    }

    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        await db.delete(relayGroups).where(eq(relayGroups.id, id));
        return reply.redirect("/config/relaygrp");
    }

    async details(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const relayRows = await db.select().from(relays).where(eq(relays.group_id, id));
        const [data] = await db
            .select()
            .from(relayGroups)
            .where(eq(relayGroups.id, id))
            .limit(1);

        return reply.view("routing/relaygrp-details", {
            data: data,
            relays: relayRows,
        });
    }

    async index(request: FastifyRequest, reply: FastifyReply) {
        const data = await db.select().from(relayGroups);

        return reply.view("routing/index", { data: data });
    }
}
