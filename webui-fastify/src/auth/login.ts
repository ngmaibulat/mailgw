import type { FastifyReply, FastifyRequest } from "fastify";

import { uuidv4 } from "../adapter.ts";

import { checkAuth } from "./util.ts";
import { sessions, SESSION_TTL_MS } from "../globals.ts";
import { AuthInfo } from "../validation/login.ts";

export async function login(request: FastifyRequest, reply: FastifyReply) {
    const body = AuthInfo.safeParse(request.body);

    if (!body.success) {
        return reply.redirect("/login?msg=ValidationError");
    }

    const { email, pass } = body.data;
    const authResult = await checkAuth(email, pass);

    if (!authResult) {
        return reply.redirect("/login?msg=InvalidAuth");
    }

    const sessionid = uuidv4();
    sessions[sessionid] = { email, expiresAt: Date.now() + SESSION_TTL_MS };

    reply.setCookie("session", sessionid, {
        maxAge: SESSION_TTL_MS / 1000, // seconds (Fastify); Express's res.cookie used ms
        signed: true,
        path: "/",
        httpOnly: true,
        sameSite: "strict",
        secure: true,
    });

    return reply.redirect("/");
}
