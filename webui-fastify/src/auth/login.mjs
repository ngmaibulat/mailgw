import { uuidv4 } from "../adapter.js";

import { checkAuth } from "./util.mjs";
import { sessions } from "../globals.mjs";
import { AuthInfo } from "../validation/login.mjs";

export async function login(request, reply) {
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
