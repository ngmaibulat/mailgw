import type { FastifyReply, FastifyRequest } from "fastify";

import { countUsers } from "../auth/users.ts";

export async function formLogin(_request: FastifyRequest, reply: FastifyReply) {
    // First run: with no accounts there's nothing to log in to — send the
    // operator to the one-time setup page instead. (/setup redirects back here
    // once a user exists, so the two never loop.)
    if ((await countUsers()) === 0) {
        return reply.redirect("/setup");
    }
    return reply.view("forms/login", {});
}
