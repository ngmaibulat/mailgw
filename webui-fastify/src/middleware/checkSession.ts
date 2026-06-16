import type { FastifyReply, FastifyRequest } from "fastify";

import { sessions } from "../globals.ts";

// Fastify `preHandler` hook (the Express `checkSession` middleware equivalent).
// Signed cookies aren't auto-decoded like Express's req.signedCookies, so we
// unsign the raw cookie ourselves via @fastify/cookie's request.unsignCookie.
export async function checkSession(request: FastifyRequest, reply: FastifyReply) {
    const raw = request.cookies?.session;
    const unsigned = raw ? request.unsignCookie(raw) : { valid: false as const, value: null };
    const sessionID = unsigned.valid ? unsigned.value : null;

    if (!sessionID || !sessions[sessionID] || !sessions[sessionID].email) {
        return reply.redirect("/login");
    }
}
