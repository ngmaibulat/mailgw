import type { FastifyInstance } from "fastify";

export default async function rootRoutes(fastify: FastifyInstance) {
    fastify.get("/", async (request, reply) => {
        return reply.view("index", {});
    });
}
