import { eq } from "drizzle-orm";

import { db, relays } from "../../db/index.mjs";
import { parseRelayBody, zodErr } from "../validation/config.mjs";

export class CtrlRelay {
    async create(request, reply) {
        let id = +request.params.group_id;

        return reply.view("routing/relay-form", {
            action: "Create",
            group_id: id,
            data: {
                host: "",
                port: 25,
                priority: 10,
                auth_user: "",
                auth_pass: "",
            },
        });
    }

    async createHandle(request, reply) {
        let group_id = +request.params.group_id;

        const parsed = parseRelayBody(request.body);
        if (!parsed.success) {
            return reply.code(400).view("routing/relay-form", {
                action: "Create",
                group_id,
                data: request.body,
                error: zodErr(parsed.error),
            });
        }

        await db.insert(relays).values(parsed.data);
        return reply.redirect(`/config/relaygrp/${group_id}`);
    }

    async edit(request, reply) {
        let id = +request.params.id;

        const [data] = await db.select().from(relays).where(eq(relays.id, id)).limit(1);

        return reply.view("routing/relay-form", {
            action: "Update",
            data: data,
            group_id: data.group_id,
        });
    }

    async editHandle(request, reply) {
        let id = +request.params.id;

        const parsed = parseRelayBody(request.body);
        if (!parsed.success) {
            return reply.code(400).view("routing/relay-form", {
                action: "Update",
                group_id: Number(request.body.group_id),
                data: request.body,
                error: zodErr(parsed.error),
            });
        }

        const values = { ...parsed.data };
        // "Leave blank to keep" — don't overwrite the stored password with an
        // empty one (the form never echoes the current password back).
        if (!values.auth_pass) {
            delete values.auth_pass;
        }

        await db.update(relays).set(values).where(eq(relays.id, id));
        return reply.redirect(`/config/relaygrp/${parsed.data.group_id}`);
    }

    async delete(request, reply) {
        let id = +request.params.id;

        const [data] = await db.select().from(relays).where(eq(relays.id, id)).limit(1);

        return reply.view("routing/relay-delete", { data: data });
    }

    async deleteHandle(request, reply) {
        let id = +request.params.id;

        const [data] = await db.select().from(relays).where(eq(relays.id, id)).limit(1);
        const group_id = data.group_id;
        await db.delete(relays).where(eq(relays.id, id));

        return reply.redirect(`/config/relaygrp/${group_id}`);
    }
}
