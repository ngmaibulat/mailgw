import type { FastifyReply, FastifyRequest } from "fastify";

export async function profile(_request: FastifyRequest, reply: FastifyReply) {
    return reply.view("util/notimpl", {});
}
