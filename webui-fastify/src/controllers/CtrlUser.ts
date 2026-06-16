import type { FastifyReply, FastifyRequest } from "fastify";

import {
    countUsers,
    createUser,
    deleteUser,
    getUser,
    listUsers,
    updateUser,
} from "../auth/users.ts";
import { UserCreate, UserEdit } from "../validation/login.ts";
import { zodErr } from "../validation/config.ts";

type IdParams = { id: string };

export class CtrlUser {
    async index(_request: FastifyRequest, reply: FastifyReply) {
        const data = await listUsers();
        return reply.view("users/index", { data });
    }

    async create(_request: FastifyRequest, reply: FastifyReply) {
        return reply.view("users/form", {
            action: "Create",
            data: { email: "" },
        });
    }

    async createHandle(request: FastifyRequest, reply: FastifyReply) {
        const parsed = UserCreate.safeParse(request.body);
        if (!parsed.success) {
            return reply.code(400).view("users/form", {
                action: "Create",
                data: request.body,
                error: zodErr(parsed.error),
            });
        }

        try {
            await createUser(parsed.data.email, parsed.data.pass);
        } catch (err) {
            request.log.error(err);
            return reply.code(400).view("users/form", {
                action: "Create",
                data: request.body,
                error: "Could not create user (is the email already taken?)",
            });
        }
        return reply.redirect("/users");
    }

    async edit(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const data = await getUser(id);
        if (!data) {
            return reply.redirect("/users");
        }
        return reply.view("users/form", { action: "Update", data });
    }

    async editHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;
        const body = request.body as Record<string, unknown>;

        const parsed = UserEdit.safeParse(body);
        if (!parsed.success) {
            return reply.code(400).view("users/form", {
                action: "Update",
                data: { ...body, id },
                error: zodErr(parsed.error),
            });
        }

        try {
            await updateUser(id, {
                email: parsed.data.email,
                pass: parsed.data.pass || undefined,
            });
        } catch (err) {
            request.log.error(err);
            return reply.code(400).view("users/form", {
                action: "Update",
                data: { ...body, id },
                error: "Could not update user (is the email already taken?)",
            });
        }
        return reply.redirect("/users");
    }

    async delete(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        const data = await getUser(id);
        if (!data) {
            return reply.redirect("/users");
        }
        return reply.view("users/delete", { data });
    }

    async deleteHandle(request: FastifyRequest, reply: FastifyReply) {
        const id = +(request.params as IdParams).id;

        // Refuse to delete the last account — doing so would lock everyone out
        // and silently re-arm the first-run /setup flow.
        if ((await countUsers()) <= 1) {
            const data = await getUser(id);
            return reply.code(400).view("users/delete", {
                data,
                error: "Cannot delete the last remaining user.",
            });
        }

        await deleteUser(id);
        return reply.redirect("/users");
    }
}
