import type { FastifyReply, FastifyRequest } from "fastify";

import { uuidv4 } from "../adapter.ts";

import { checkAuth } from "./util.ts";
import { sessions } from "../globals.ts";
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
    sessions[sessionid] = { email };

    reply.setCookie("session", sessionid, {
        maxAge: 8 * 60 * 60, // seconds (Fastify); Express's res.cookie used ms
        signed: true,
        path: "/",
        httpOnly: true,
        sameSite: "strict",
        secure: true,
    });

    return reply.redirect("/");
}
