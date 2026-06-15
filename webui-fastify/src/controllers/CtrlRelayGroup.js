import { eq } from "drizzle-orm";

import { db, relays, relayGroups } from "../../db/index.mjs";
import { relayGroupInsert, zodErr } from "../validation/config.mjs";

export class CtrlRelayGroup {
    async create(request, reply) {
        return reply.view("routing/relaygrp-form", {
            action: "Create",
            data: {
                name: "",
                description: "",
            },
        });
    }

    async createHandle(request, reply) {
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

    async edit(request, reply) {
        let id = +request.params.id;

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

    async editHandle(request, reply) {
        let id = +request.params.id;

        const parsed = relayGroupInsert.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("routing/relaygrp-form", {
                action: "Update",
                data: { ...request.body, id },
                error: zodErr(parsed.error),
            });
        }

        await db.update(relayGroups).set(parsed.data).where(eq(relayGroups.id, id));
        return reply.redirect("/config/relaygrp");
    }

    async delete(request, reply) {
        let id = +request.params.id;

        const [data] = await db
            .select()
            .from(relayGroups)
            .where(eq(relayGroups.id, id))
            .limit(1);

        return reply.view("routing/relaygrp-delete", { data: data });
    }

    async deleteHandle(request, reply) {
        let id = +request.params.id;

        await db.delete(relayGroups).where(eq(relayGroups.id, id));
        return reply.redirect("/config/relaygrp");
    }

    async details(request, reply) {
        let id = +request.params.id;

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

    async index(request, reply) {
        const data = await db.select().from(relayGroups);

        return reply.view("routing/index", { data: data });
    }
}
