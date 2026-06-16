import type { FastifyInstance } from "fastify";

export default async function rootRoutes(fastify: FastifyInstance) {
    fastify.get("/", async (_request, reply) => {
        return reply.view("index", {});
    });
}
