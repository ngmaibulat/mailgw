import type { FastifyReply, FastifyRequest } from "fastify";

export async function profile(request: FastifyRequest, reply: FastifyReply) {
    return reply.view("util/notimpl", {});
}
