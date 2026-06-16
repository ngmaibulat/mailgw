import type { FastifyReply, FastifyRequest } from "fastify";

import { countUsers, createUser } from "./users.ts";
import { SetupInfo } from "../validation/login.ts";
import { zodErr } from "../validation/config.ts";

// First-run bootstrap. `/setup` is unauthenticated by necessity (you can't log
// in when no account exists), so the ONLY thing keeping it from being an open
// user-creation endpoint is the user-count gate: both handlers refuse once any
// user exists. Once the first admin is created, `/setup` permanently redirects
// to `/login`. Re-check the count inside POST (not just GET) so the gate can't
// be bypassed by posting directly.

export async function setupForm(_request: FastifyRequest, reply: FastifyReply) {
    if ((await countUsers()) > 0) {
        return reply.redirect("/login");
    }
    return reply.view("forms/setup", {});
}

export async function setupSubmit(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    if ((await countUsers()) > 0) {
        return reply.redirect("/login");
    }

    const body = SetupInfo.safeParse(request.body);
    if (!body.success) {
        return reply.view("forms/setup", { error: zodErr(body.error) });
    }

    try {
        await createUser(body.data.email, body.data.pass);
    } catch (err) {
        request.log.error(err);
        return reply.view("forms/setup", {
            error: "Could not create user (is the email already taken?)",
        });
    }

    return reply.redirect("/login?msg=created");
}
