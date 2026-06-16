import type { FastifyReply, FastifyRequest } from "fastify";

import { sessionEmail } from "../auth/session.ts";

// Fastify `preHandler` hook (the Express `checkSession` middleware equivalent).
// Signed cookies aren't auto-decoded like Express's req.signedCookies, so the
// unsign + session lookup lives in `sessionEmail` (shared with /profile).
export async function checkSession(
    request: FastifyRequest,
    reply: FastifyReply,
) {
    if (!sessionEmail(request)) {
        const accept = String(request.headers.accept ?? "");
        const wantsJson = accept.includes("application/json");
        if (wantsJson) {
            return reply.code(401).send({ error: "unauthenticated" });
        }
        return reply.redirect("/login");
    }
}
