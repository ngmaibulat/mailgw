import type { FastifyReply, FastifyRequest } from "fastify";

export async function formLogin(request: FastifyRequest, reply: FastifyReply) {
    return reply.view("forms/login", {});
}
