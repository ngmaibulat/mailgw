import type { FastifyReply, FastifyRequest } from "fastify";

import { deleteSession } from "../globals.ts";

export async function logout(request: FastifyRequest, reply: FastifyReply) {
    // Tear down the server-side session too — clearing only the cookie left the
    // session alive (memory leak) and re-presentable.
    const raw = request.cookies?.session;
    const unsigned = raw
        ? request.unsignCookie(raw)
        : { valid: false as const, value: null };
    if (unsigned.valid && unsigned.value) {
        deleteSession(unsigned.value);
    }

    reply.clearCookie("session", { path: "/" });
    return reply.redirect("/");
}
