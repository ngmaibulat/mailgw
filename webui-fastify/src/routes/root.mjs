export default async function rootRoutes(fastify) {
    fastify.get("/", async (request, reply) => {
        return reply.view("index", {});
    });
}
