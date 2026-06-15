import * as logservice from "../logservice.mjs";

// Read-only query endpoints backing the log-viewer grids (public/js/log-*.js).
// All are proxied to logservice, which owns the log data (event ingestion + the
// MD5 filter live there). The webui no longer reads any log table directly.
//
// Note the path remaps: queue -> logservice /api/transaction (queue events are
// stored as Transaction rows), and hashlookups -> /api/hashlookup (HashLookups
// joined to its Transaction for sender/rcpt).
export default async function apiRoutes(fastify) {
    fastify.get("/connection", async (request, reply) => {
        return reply.send(await logservice.search("/api/connection", request.query.request));
    });

    fastify.get("/delivery", async (request, reply) => {
        return reply.send(await logservice.search("/api/delivery", request.query.request));
    });

    fastify.get("/queue", async (request, reply) => {
        return reply.send(await logservice.search("/api/transaction", request.query.request));
    });

    fastify.get("/hashlookups", async (request, reply) => {
        return reply.send(await logservice.search("/api/hashlookup", request.query.request));
    });
}
