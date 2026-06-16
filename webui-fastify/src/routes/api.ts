import type { FastifyInstance } from "fastify";

import * as logservice from "../logservice.ts";

type SearchQuery = { request?: string };

// Read-only query endpoints backing the log-viewer grids (public/js/log-*.js).
// All are proxied to logservice, which owns the log data (event ingestion + the
// MD5 filter live there). The webui no longer reads any log table directly.
//
// Note the path remaps: queue -> logservice /api/transaction (queue events are
// stored as Transaction rows), and hashlookups -> /api/hashlookup (HashLookups
// joined to its Transaction for sender/rcpt).
export default async function apiRoutes(fastify: FastifyInstance) {
    fastify.get("/connection", async (request, reply) => {
        const { request: q } = request.query as SearchQuery;
        return reply.send(await logservice.search("/api/connection", q));
    });

    fastify.get("/delivery", async (request, reply) => {
        const { request: q } = request.query as SearchQuery;
        return reply.send(await logservice.search("/api/delivery", q));
    });

    fastify.get("/queue", async (request, reply) => {
        const { request: q } = request.query as SearchQuery;
        return reply.send(await logservice.search("/api/transaction", q));
    });

    fastify.get("/hashlookups", async (request, reply) => {
        const { request: q } = request.query as SearchQuery;
        return reply.send(await logservice.search("/api/hashlookup", q));
    });
}
