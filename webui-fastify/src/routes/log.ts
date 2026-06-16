import type { FastifyInstance } from "fastify";

export default async function logRoutes(fastify: FastifyInstance) {
    fastify.get("/delivery", async (request, reply) => {
        return reply.view("log-delivery", {});
    });

    fastify.get("/connection", async (request, reply) => {
        return reply.view("log-connection", {});
    });

    fastify.get("/mails", async (request, reply) => {
        return reply.view("log-mails", {});
    });

    fastify.get("/lookups", async (request, reply) => {
        return reply.view("log-lookups", {});
    });
}
