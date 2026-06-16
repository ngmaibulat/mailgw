import type { FastifyInstance } from "fastify";

export default async function logRoutes(fastify: FastifyInstance) {
    // `wide: true` opts these pages out of the centered max-width shell so the
    // w2ui log grids can use the full viewport width (see page.pug).
    fastify.get("/delivery", async (_request, reply) => {
        return reply.view("log-delivery", { wide: true });
    });

    fastify.get("/connection", async (_request, reply) => {
        return reply.view("log-connection", { wide: true });
    });

    fastify.get("/mails", async (_request, reply) => {
        return reply.view("log-mails", { wide: true });
    });

    fastify.get("/lookups", async (_request, reply) => {
        return reply.view("log-lookups", { wide: true });
    });
}
