import type { FastifyReply, FastifyRequest } from "fastify";

export async function logout(request: FastifyRequest, reply: FastifyReply) {
    reply.clearCookie("session", { path: "/" });
    return reply.redirect("/");
}
