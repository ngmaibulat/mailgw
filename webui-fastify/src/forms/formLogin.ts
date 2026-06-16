import type { FastifyReply, FastifyRequest } from "fastify";

export async function formLogin(_request: FastifyRequest, reply: FastifyReply) {
    return reply.view("forms/login", {});
}
